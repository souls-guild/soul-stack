package mcp

// Guards for the re-authorization of `GET /mcp/events` (NIM-858).
//
// SECURITY invariant, and the reason the file exists:
//
//	An open MCP apply-event stream STOPS when the Archon holding it stops being
//	allowed to hold it.
//
// This stream was the THIRD channel with the hole NIM-844 closed on the console
// socket and the /v1 run-events stream, and the last one to be fixed. It is also
// the worst-placed of the three: `middleware.RejectRevoked` sits on the /v1 chain
// and refuses a revoked Archon's NEXT request, but this is a separate listener
// that the link deliberately does not run on at all (NIM-551) — so before this,
// a revoked Archon's open stream had nothing anywhere that would ever say no,
// and kept delivering task plans and per-host results for the rest of
// sseMaxLifetime.
//
// Each direction has a negative control. "The stream closes when access is
// withdrawn" alone is satisfied by a loop that closes every stream on its first
// tick, which would be a far worse defect than the one being fixed — so the
// withdrawal cases are paired with one where nothing is withdrawn and the stream
// must survive several intervals AND still be delivering.

import (
	"bufio"
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/souls-guild/soul-stack/keeper/internal/applybus"
	"github.com/souls-guild/soul-stack/keeper/internal/applyrun"
)

// sseReauthTestInterval is short enough that a test reaches several checks and
// long enough that it is not racing the goroutine that starts the loop.
const sseReauthTestInterval = 25 * time.Millisecond

// mutableSSERBAC is a [ServerRBAC] that can be changed while a stream is open —
// which is the whole point: the defect was that nothing read it a second time.
type mutableSSERBAC struct {
	mu      sync.Mutex
	revoked bool
	// allow is the permission allow-set, keyed "resource.action". A nil map
	// allows everything, so a test that only cares about revocation says so by
	// leaving it out.
	allow map[string]bool
	// onIncarnation scopes the allow-set. Empty means unscoped. A fake that
	// ignored the context map would answer yes however the re-check spelled the
	// incarnation — including not at all — and every narrowing case here would
	// pass on a rule that had stopped being applied.
	onIncarnation string
}

func (m *mutableSSERBAC) Check(_, resource, action string, ctx map[string]string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.onIncarnation != "" && ctx["incarnation"] != m.onIncarnation {
		return errSSEReauthDenied
	}
	if m.allow == nil || m.allow[resource+"."+action] {
		return nil
	}
	return errSSEReauthDenied
}

func (m *mutableSSERBAC) IsRevoked(string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.revoked
}

func (m *mutableSSERBAC) revoke() {
	m.mu.Lock()
	m.revoked = true
	m.mu.Unlock()
}

func (m *mutableSSERBAC) withdraw(perm string) {
	m.mu.Lock()
	delete(m.allow, perm)
	m.mu.Unlock()
}

// errSSEReauthDenied is the denial this fake returns; only its non-nil-ness is read.
var errSSEReauthDenied = &sseReauthDenial{}

type sseReauthDenial struct{}

func (*sseReauthDenial) Error() string { return "denied" }

// sseReauthServer is [sseRBACServer] with the re-check interval compressed.
func sseReauthServer(t *testing.T, bus *applybus.EventBus, access applyAccessStore, rbac ServerRBAC) *httptest.Server {
	t.Helper()
	h := buildSSEHandler(sseDeps{
		JWTVerifier:    sseTestVerifier(t),
		Bus:            bus,
		Access:         access,
		RBAC:           rbac,
		Logger:         slog.New(slog.NewJSONHandler(io.Discard, nil)),
		ReauthInterval: sseReauthTestInterval,
	})
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return srv
}

// openSSEStream opens the stream and returns the live body.
func openSSEStream(t *testing.T, srv *httptest.Server, aid, applyID string) *http.Response {
	t.Helper()
	req, _ := http.NewRequest(http.MethodGet, srv.URL+"?apply_id="+applyID, nil)
	req.Header.Set("Authorization", "Bearer "+sseTestToken(t, aid))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET stream: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		_ = resp.Body.Close()
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	t.Cleanup(func() { _ = resp.Body.Close() })
	return resp
}

// waitSSESubscribed waits until the handler has reached the bus, so a test that
// then withdraws access is withdrawing it from a stream that is actually running.
func waitSSESubscribed(t *testing.T, bus *applybus.EventBus, applyID string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if bus.Subscribers(applyID) >= 1 {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("no subscriber on %s within 2s", applyID)
}

// expectSSEStreamEnds reads until the body ends, which is how the stream reports
// that the subscription is over (no closing frame is invented — see
// buildSSEHandler).
func expectSSEStreamEnds(t *testing.T, body io.Reader, complaint string) {
	t.Helper()
	done := make(chan struct{})
	go func() {
		_, _ = io.Copy(io.Discard, body)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal(complaint)
	}
}

// TestSSEReauth_RevokedSubscriberLosesTheStream — the case NIM-858 exists for,
// and the one the initiator short-circuit makes non-obvious: having started the
// run is what lets an Archon with no permission at all watch it, so only the
// revocation check can end this stream. Remove the reauth branch and this test
// hangs on the read for the full five seconds.
func TestSSEReauth_RevokedSubscriberLosesTheStream(t *testing.T) {
	bus := applybus.NewBus(slog.New(slog.NewJSONHandler(io.Discard, nil)))
	rbac := &mutableSSERBAC{allow: map[string]bool{}}
	const applyID = "01APPLYREVOKED000000000000"
	access := fakeAccess{byID: map[string]*applyrun.Access{
		applyID: {IncarnationName: "redis-prod", StartedByAID: ptr("archon-op")},
	}}
	srv := sseReauthServer(t, bus, access, rbac)

	resp := openSSEStream(t, srv, "archon-op", applyID)
	waitSSESubscribed(t, bus, applyID)

	rbac.revoke()
	expectSSEStreamEnds(t, resp.Body,
		"the stream outlived the revocation of the Archon holding it — this listener has no RejectRevoked on it at all (NIM-551), so nothing else ever will")
}

// TestSSEReauth_NarrowedSubscriberLosesTheStream — the permission case: a watcher
// who is not the initiator keeps the stream only while `incarnation.get` still
// answers yes. A narrowing short of revocation has to reach it too, or a role
// edit that takes an Archon off an incarnation leaves them watching it.
func TestSSEReauth_NarrowedSubscriberLosesTheStream(t *testing.T) {
	bus := applybus.NewBus(slog.New(slog.NewJSONHandler(io.Discard, nil)))
	rbac := &mutableSSERBAC{allow: map[string]bool{"incarnation.get": true}}
	const applyID = "01APPLYNARROWED00000000000"
	access := fakeAccess{byID: map[string]*applyrun.Access{
		applyID: {IncarnationName: "redis-prod", StartedByAID: ptr("archon-else")},
	}}
	srv := sseReauthServer(t, bus, access, rbac)

	resp := openSSEStream(t, srv, "archon-op", applyID)
	waitSSESubscribed(t, bus, applyID)

	rbac.withdraw("incarnation.get")
	expectSSEStreamEnds(t, resp.Body,
		"the stream outlived the permission that opened it — a narrowing short of revocation must reach it too")
}

// TestSSEReauth_UnchangedRightsKeepTheStream — the negative control, and the one
// that says the loop is re-deciding rather than merely expiring streams.
func TestSSEReauth_UnchangedRightsKeepTheStream(t *testing.T) {
	bus := applybus.NewBus(slog.New(slog.NewJSONHandler(io.Discard, nil)))
	// Scoped to the run's own incarnation, so the stream surviving proves the
	// re-check is still naming it — a check that dropped g.incarnation would be
	// denied here rather than waved through.
	rbac := &mutableSSERBAC{allow: map[string]bool{"incarnation.get": true}, onIncarnation: "redis-prod"}
	const applyID = "01APPLYKEPT0000000000000000"
	access := fakeAccess{byID: map[string]*applyrun.Access{
		applyID: {IncarnationName: "redis-prod", StartedByAID: ptr("archon-else")},
	}}
	srv := sseReauthServer(t, bus, access, rbac)

	resp := openSSEStream(t, srv, "archon-op", applyID)
	waitSSESubscribed(t, bus, applyID)

	time.Sleep(6 * sseReauthTestInterval)

	// Still there AND still delivering: a subscriber that survived on the bus
	// while the writer had given up would look identical from the server side.
	bus.Publish(applybus.Event{
		ApplyID: applyID,
		Kind:    applybus.KindTaskExecuted,
		Payload: map[string]any{"apply_id": applyID},
	})
	frame, err := readSSEFrame(bufio.NewReader(resp.Body), 3*time.Second)
	if err != nil {
		t.Fatalf("reading a frame after %v with rights unchanged: %v — the re-check is closing streams it should keep",
			6*sseReauthTestInterval, err)
	}
	if !strings.Contains(frame, "event: task.executed") {
		t.Fatalf("frame = %q, want task.executed", frame)
	}
}

// TestSSEStillAuthorized_Matrix pins the decision itself, including the two
// asymmetries that are easy to write backwards: revocation outranks the initiator
// short-circuit, and a nil checker leaves the initiator admitted because the
// opening decision does too (a continuing check stricter than the opening one
// produces a stream that dies for a reason that was already true when it opened).
func TestSSEStillAuthorized_Matrix(t *testing.T) {
	const sub = "archon-op"
	grantInitiator := sseGrant{incarnation: "redis-prod", initiator: true}
	grantWatcher := sseGrant{incarnation: "redis-prod"}

	cases := []struct {
		name  string
		rbac  ServerRBAC
		grant sseGrant
		want  bool
	}{
		{"initiator-keeps", &mutableSSERBAC{allow: map[string]bool{}}, grantInitiator, true},
		{"initiator-revoked-loses", &mutableSSERBAC{revoked: true}, grantInitiator, false},
		{"watcher-with-get-keeps", &mutableSSERBAC{allow: map[string]bool{"incarnation.get": true}}, grantWatcher, true},
		{"watcher-narrowed-loses", &mutableSSERBAC{allow: map[string]bool{}}, grantWatcher, false},
		{"watcher-revoked-loses", &mutableSSERBAC{allow: map[string]bool{"incarnation.get": true}, revoked: true}, grantWatcher, false},
		{"nil-rbac-watcher-loses", nil, grantWatcher, false},
		{"nil-rbac-initiator-keeps", nil, grantInitiator, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			deps := sseDeps{RBAC: c.rbac}
			if c.rbac == nil {
				// A typed nil in the interface would still dispatch; the wiring
				// this case is about leaves the field unset.
				deps = sseDeps{}
			}
			if got := sseStillAuthorized(deps, sub, c.grant); got != c.want {
				t.Errorf("sseStillAuthorized = %v, want %v", got, c.want)
			}
		})
	}
}

// TestSSEAuthorize_GrantCarriesTheRunsImmutableFacts — the re-check reads the
// grant and never the database, so a grant that recorded the wrong incarnation or
// lost the initiator flag would re-decide against the wrong subject while every
// opening test stayed green.
func TestSSEAuthorize_GrantCarriesTheRunsImmutableFacts(t *testing.T) {
	const applyID = "01APPLYGRANT0000000000000A"
	deps := sseDeps{
		Access: fakeAccess{byID: map[string]*applyrun.Access{
			applyID: {IncarnationName: "redis-prod", StartedByAID: ptr("archon-owner")},
		}},
		RBAC:   fakeRBAC{allow: map[string]string{"archon-watcher": "redis-prod"}},
		Logger: slog.New(slog.NewJSONHandler(io.Discard, nil)),
	}

	owner, ok := authorizeSSE(context.Background(), deps, "archon-owner", applyID)
	if !ok {
		t.Fatal("the run's initiator was refused at the open")
	}
	if !owner.initiator || owner.incarnation != "redis-prod" {
		t.Errorf("initiator grant = %+v, want {redis-prod true} — the re-check would ask the wrong question", owner)
	}

	watcher, ok := authorizeSSE(context.Background(), deps, "archon-watcher", applyID)
	if !ok {
		t.Fatal("a watcher holding incarnation.get was refused at the open")
	}
	if watcher.initiator {
		t.Error("a watcher's grant claims they started the run — the re-check would then never fall through to the permission")
	}
	if watcher.incarnation != "redis-prod" {
		t.Errorf("watcher grant names incarnation %q, want redis-prod", watcher.incarnation)
	}
}

// TestSSEAuthorize_RevokedInitiatorIsRefusedAtTheOpen — the half that makes the
// re-check terminal rather than periodic.
//
// The initiator branch admits a subscriber holding no permission at all, so
// nothing else on the opening path ever consults the revoked projection: the JWT
// verifier checks a signature and an expiry, and this listener carries no
// RejectRevoked (NIM-551). Without the revocation check in authorizeSSE a revoked
// Archon opens the stream, loses it to the re-check one interval later, re-opens,
// and keeps receiving run payloads indefinitely — fifteen seconds at a time. Take
// the branch out of authorizeSSE and this test reads 200.
func TestSSEAuthorize_RevokedInitiatorIsRefusedAtTheOpen(t *testing.T) {
	bus := applybus.NewBus(slog.New(slog.NewJSONHandler(io.Discard, nil)))
	const applyID = "01APPLYOPENREVOKED00000000"
	access := fakeAccess{byID: map[string]*applyrun.Access{
		applyID: {IncarnationName: "redis-prod", StartedByAID: ptr("archon-op")},
	}}
	srv := sseReauthServer(t, bus, access, &mutableSSERBAC{revoked: true})

	req, _ := http.NewRequest(http.MethodGet, srv.URL+"?apply_id="+applyID, nil)
	req.Header.Set("Authorization", "Bearer "+sseTestToken(t, "archon-op"))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET stream: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 — a revoked Archon who STARTED the run is admitted by the "+
			"initiator branch, and nothing else on this listener asks about revocation", resp.StatusCode)
	}
	// The control: the same request from a live Archon still opens, so the check
	// above is about the revocation and not about a fixture that refuses
	// everyone. openSSEStream fails the test on anything but 200.
	openSSEStream(t, sseReauthServer(t, bus, access, &mutableSSERBAC{}), "archon-op", applyID)
}

// TestSSEAuthorize_OpeningAndContinuingAgreeOnEveryCase — the two decisions are one
// rule written twice, and this is what says they still agree. A divergence either
// way is a defect: looser at the open admits a subscriber the stream then kills
// within an interval (the shape of the bug above), stricter at the open refuses one
// the stream would have kept.
func TestSSEAuthorize_OpeningAndContinuingAgreeOnEveryCase(t *testing.T) {
	const applyID = "01APPLYAGREEMENT000000000A"
	cases := []struct {
		name    string
		sub     string
		rbac    ServerRBAC
		started *string
		// grant is what this run's row implies, stated INDEPENDENTLY of what the
		// open answers. Handing the re-check the open's own grant would make
		// every refused case compare false against a zero grant and assert
		// nothing: both sides would be false, for different reasons, and a new
		// refusal added to authorizeSSE would drive the pair to agree rather
		// than be caught.
		grant sseGrant
	}{
		{"initiator", "archon-op", &mutableSSERBAC{allow: map[string]bool{}}, ptr("archon-op"),
			sseGrant{incarnation: "redis-prod", initiator: true}},
		{"initiator-revoked", "archon-op", &mutableSSERBAC{revoked: true}, ptr("archon-op"),
			sseGrant{incarnation: "redis-prod", initiator: true}},
		{"watcher-with-get", "archon-op", &mutableSSERBAC{allow: map[string]bool{"incarnation.get": true}, onIncarnation: "redis-prod"}, ptr("archon-else"),
			sseGrant{incarnation: "redis-prod"}},
		{"watcher-narrowed", "archon-op", &mutableSSERBAC{allow: map[string]bool{}}, ptr("archon-else"),
			sseGrant{incarnation: "redis-prod"}},
		{"watcher-revoked", "archon-op", &mutableSSERBAC{allow: map[string]bool{"incarnation.get": true}, onIncarnation: "redis-prod", revoked: true}, ptr("archon-else"),
			sseGrant{incarnation: "redis-prod"}},
		{"watcher-scoped-to-another-incarnation", "archon-op", &mutableSSERBAC{allow: map[string]bool{"incarnation.get": true}, onIncarnation: "redis-staging"}, ptr("archon-else"),
			sseGrant{incarnation: "redis-prod"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			deps := sseDeps{
				Access: fakeAccess{byID: map[string]*applyrun.Access{
					applyID: {IncarnationName: "redis-prod", StartedByAID: c.started},
				}},
				RBAC:   c.rbac,
				Logger: slog.New(slog.NewJSONHandler(io.Discard, nil)),
			}
			grant, opened := authorizeSSE(context.Background(), deps, c.sub, applyID)
			kept := sseStillAuthorized(deps, c.sub, c.grant)
			if opened != kept {
				t.Errorf("the open says %v and the re-check says %v for the same subscriber — "+
					"one of the two rules has drifted", opened, kept)
			}
			// And when both admit, the grant the open hands over must be the one
			// the re-check was asked about, or the two agree here and diverge in
			// production.
			if opened && grant != c.grant {
				t.Errorf("the open produced grant %+v, want %+v", grant, c.grant)
			}
		})
	}
}
