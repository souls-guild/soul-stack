package errand

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/souls-guild/soul-stack/shared/audit"
	"github.com/souls-guild/soul-stack/shared/config"
)

// Guards for the dry-run capability gate (soulcompat.go, ADR-0076(i)). The mode
// being closed is the dangerous one: a Soul that predates ErrandRequest.dry_run
// ignores the flag and calls Apply, so an operation advertised as a pure read
// executes for real. Every guard therefore asserts more than "an error came
// back" — that NOTHING went out to the Soul, that no errands row was written,
// and that no `errand.invoked` audit event claims otherwise.

// stubSoulCap — a controllable [SoulCapabilityChecker]. lacking — SIDs that
// announce nothing (nil → every host supports everything asked of it);
// err — simulates a Redis failure. asked records the (capability, sids) pairs the
// gate actually queried, so a test can prove the gate asked about the right thing
// rather than trusting an error message.
type stubSoulCap struct {
	mu      sync.Mutex
	lacking []string
	err     error
	asked   map[string][]string
	calls   int
}

func (s *stubSoulCap) SoulsLackingCapability(_ context.Context, sids []string, capability string) ([]string, error) {
	s.mu.Lock()
	if s.asked == nil {
		s.asked = map[string][]string{}
	}
	s.asked[capability] = append(s.asked[capability], sids...)
	s.calls++
	s.mu.Unlock()
	if s.err != nil {
		return nil, s.err
	}
	asked := make(map[string]struct{}, len(sids))
	for _, sid := range sids {
		asked[sid] = struct{}{}
	}
	var out []string
	for _, sid := range s.lacking {
		if _, ok := asked[sid]; ok {
			out = append(out, sid)
		}
	}
	return out, nil
}

func (s *stubSoulCap) askedFor(capability string) []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.asked[capability]
}

// callCount is the number of CALLS, not the number of distinct capabilities asked
// about — `asked` is keyed by capability, so len(asked) would read 1 for a hundred
// calls about dry_run. Today every assertion wants 0 and the two agree; the next
// one that wants 1 would not.
func (s *stubSoulCap) callCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls
}

// fakeAudit records written events. The gate must leave none: `errand.invoked`
// means "we invoked it", and a refused request invoked nothing.
type fakeAudit struct {
	mu     sync.Mutex
	events []*audit.Event
}

func (a *fakeAudit) Write(_ context.Context, ev *audit.Event) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.events = append(a.events, ev)
	return nil
}

func (a *fakeAudit) count() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return len(a.events)
}

// gateFixture is one dispatcher wired with fakes plus the observation points the
// guards read: what was sent, what was inserted, what was audited.
type gateFixture struct {
	d     *Dispatcher
	store *fakeStore
	ob    *fakeOutbound
	aud   *fakeAudit
}

// newGateFixture builds a dispatcher whose target host is locally connected and
// whose bus never answers — so if the gate ever let a request through, the test
// would see it in ob.sent regardless of what came back.
func newGateFixture(cap SoulCapabilityChecker) *gateFixture {
	store := newFakeStore()
	ob := &fakeOutbound{}
	aud := &fakeAudit{}
	return &gateFixture{
		store: store,
		ob:    ob,
		aud:   aud,
		d: &Dispatcher{deps: Deps{
			Store:       store,
			Outbound:    ob,
			Publisher:   ob,
			LeaseLookup: &fakeLease{holders: map[string]string{gateSID: "kid-test"}},
			ApplyBus:    newFakeBus(),
			Logger:      slog.New(slog.NewJSONHandler(io.Discard, nil)),
			Audit:       aud,
			KID:         "kid-test",
			ServerCap:   80 * time.Millisecond,
			Clock:       time.Now,
			SoulCap:     cap,
		}},
	}
}

const gateSID = "host.test"

// dryRunReq — the request under test: a verb-shell module, which is the case that
// makes the gate load-bearing. On a Soul honoring dry_run the module is refused
// as not PlanReadSafe and nothing runs; on one that ignores the flag the command
// line executes for real.
func dryRunReq() DispatchRequest {
	return DispatchRequest{
		SID:          gateSID,
		Module:       "core.cmd.shell",
		Input:        map[string]any{"command": "rm -rf /var/lib/soul-stack/state"},
		TimeoutSec:   5,
		DryRun:       true,
		StartedByAID: "archon-alice",
	}
}

// assertNothingHappened is the shared half of every refusal guard: the request
// must not have reached the Soul, must not have left a row, and must not have
// been audited as invoked.
// Read under the fakes' own locks. A refusal path spawns nothing, but the
// dispatching cases in this file escalate to async (ServerCap < TimeoutSec) and
// leave a waitAsync goroutine writing to the same fakes for seconds afterwards —
// so reusing this helper in a test that also dispatches must not become a race.
func (f *gateFixture) assertNothingHappened(t *testing.T) {
	t.Helper()
	if n := f.ob.sentCount(); n != 0 {
		t.Fatalf("* SendErrand calls = %d, want 0 (refusal must precede dispatch - the flag-ignoring Soul would have run the command)", n)
	}
	if n := f.store.insertCount(); n != 0 {
		t.Errorf("errands rows inserted = %d, want 0 (a refused request was never invoked)", n)
	}
	if n := f.aud.count(); n != 0 {
		t.Errorf("audit events written = %d, want 0 (errand.invoked would claim an invocation that never happened)", n)
	}
}

func (s *fakeStore) insertCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.inserts
}

func (o *fakeOutbound) sentCount() int {
	o.mu.Lock()
	defer o.mu.Unlock()
	return len(o.sent)
}

func (o *fakeOutbound) sentDryRun(i int) bool {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.sent[i].GetDryRun()
}

// TestDispatch_DryRunNotAnnounced_RefusedBeforeDispatch — ★ THE GUARD. The target
// announced everything except dry_run (a binary predating the flag). Dispatching
// there would run the shell command for real inside an operation that promised a
// pure read (ADR-031(b)). ASSERT: refused with ErrDryRunNotAnnounced, the message
// names the host and the capability so the operator knows what to upgrade, the
// gate asked about exactly that capability for exactly that host, and nothing was
// sent, stored, or audited.
func TestDispatch_DryRunNotAnnounced_RefusedBeforeDispatch(t *testing.T) {
	cap := &stubSoulCap{lacking: []string{gateSID}}
	f := newGateFixture(cap)

	res, err := f.d.Dispatch(context.Background(), dryRunReq())
	if err == nil {
		t.Fatal("a host that did not announce dry_run must not be asked for a dry_run - it would apply for real")
	}
	if !errors.Is(err, ErrDryRunNotAnnounced) {
		t.Fatalf("error = %v, want ErrDryRunNotAnnounced (handlers map this sentinel to 409)", err)
	}
	for _, want := range []string{gateSID, config.CapabilityDryRun, "apply for real"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error = %q, does not mention %q", err, want)
		}
	}
	// The refusal must not claim to know the host is running an old binary: the
	// presence layer cannot distinguish "announced a set without dry_run" from
	// "no presence record at all", and stating certainty it does not have is how
	// an operator ends up chasing the wrong host.
	if strings.Contains(err.Error(), "confirmed") {
		t.Errorf("error = %q claims certainty the presence layer cannot provide", err)
	}
	if res.ErrandID != "" {
		t.Errorf("ErrandID = %q, want empty (no errand was created)", res.ErrandID)
	}
	f.assertNothingHappened(t)

	// The gate asked about the capability it is named for, for the target host —
	// not a fixed set, and not some other host.
	asked := cap.askedFor(config.CapabilityDryRun)
	if len(asked) != 1 || asked[0] != gateSID {
		t.Errorf("asked about %q for %v, want exactly [%s]", config.CapabilityDryRun, asked, gateSID)
	}
}

// TestDispatch_DryRunAnnounced_Dispatched — the other half: a host that announces
// dry_run is not blocked, and the flag survives onto the wire. Without this, a
// gate that refused every dry_run would look correct to the guard above.
func TestDispatch_DryRunAnnounced_Dispatched(t *testing.T) {
	f := newGateFixture(&stubSoulCap{})

	// The bus stays silent, so this times out; irrelevant here — what matters is
	// that the request reached the Soul at all.
	if _, err := f.d.Dispatch(context.Background(), dryRunReq()); err != nil {
		t.Fatalf("Dispatch: %v (an announcing host must not be blocked)", err)
	}
	if n := f.ob.sentCount(); n != 1 {
		t.Fatalf("SendErrand calls = %d, want 1 (an announcing host must not be blocked)", n)
	}
	if !f.ob.sentDryRun(0) {
		t.Error("ErrandRequest.dry_run = false on the wire, want true (the gate must not swallow the flag it guards)")
	}
}

// TestDispatch_DryRunNilChecker_FailClosed — no Redis, so no announcement to read.
// Refused rather than sent on the theory that the fleet is probably current:
// guessing wrong here means a real apply on a host the operator asked only to
// read, which is the whole failure mode ADR-0076(i) exists to stop.
func TestDispatch_DryRunNilChecker_FailClosed(t *testing.T) {
	f := newGateFixture(nil)

	_, err := f.d.Dispatch(context.Background(), dryRunReq())
	if err == nil {
		t.Fatal("dry_run with no presence source must be refused, not sent unconfirmed")
	}
	if !errors.Is(err, ErrDryRunUnverifiable) {
		t.Fatalf("error = %v, want ErrDryRunUnverifiable", err)
	}
	if errors.Is(err, ErrDryRunNotAnnounced) {
		t.Error("unverifiable must not report as not-announced: the operator's fix is Redis, not a binary upgrade")
	}
	f.assertNothingHappened(t)
}

// TestDispatch_DryRunCheckerFailure_FailClosed — Redis answered with an error. Same
// refusal as a nil checker (support is equally unconfirmed), and the underlying
// cause stays wrapped so the operator log says why the check failed.
func TestDispatch_DryRunCheckerFailure_FailClosed(t *testing.T) {
	boom := errors.New("redis: connection refused")
	f := newGateFixture(&stubSoulCap{err: boom})

	_, err := f.d.Dispatch(context.Background(), dryRunReq())
	if err == nil {
		t.Fatal("a failed capability check must refuse the dry_run, not fall through to dispatch")
	}
	if !errors.Is(err, ErrDryRunUnverifiable) {
		t.Fatalf("error = %v, want ErrDryRunUnverifiable", err)
	}
	if !errors.Is(err, boom) {
		t.Errorf("error = %v, does not wrap the underlying cause %v", err, boom)
	}
	f.assertNothingHappened(t)
}

// TestDispatch_WithoutDryRun_ChecksNothing — the gate must not become a tax on the
// ordinary Errand. A request without dry_run dispatches with a checker that would
// reject every host, and never consults it: no extra Redis round-trip on the
// common path, and no new way for a presence outage to break plain exec.
func TestDispatch_WithoutDryRun_ChecksNothing(t *testing.T) {
	cap := &stubSoulCap{lacking: []string{gateSID}}
	f := newGateFixture(cap)

	req := dryRunReq()
	req.DryRun = false
	if _, err := f.d.Dispatch(context.Background(), req); err != nil {
		t.Fatalf("Dispatch: %v (a non-dry-run Errand must not be gated)", err)
	}
	if n := f.ob.sentCount(); n != 1 {
		t.Fatalf("SendErrand calls = %d, want 1 (a non-dry-run Errand must not be gated)", n)
	}
	if n := cap.callCount(); n != 0 {
		t.Errorf("capability checker consulted %d times, want 0 (only dry_run pays for the gate)", n)
	}
}

// TestDispatch_WithoutDryRun_NilCheckerIsFine — the same property against the
// deployment that has no checker at all: a nil SoulCap is fail-closed for dry_run
// and invisible to everything else, which is why it stays optional in Deps.
func TestDispatch_WithoutDryRun_NilCheckerIsFine(t *testing.T) {
	f := newGateFixture(nil)

	req := dryRunReq()
	req.DryRun = false
	if _, err := f.d.Dispatch(context.Background(), req); err != nil {
		t.Fatalf("Dispatch: %v (nil SoulCap must not break a plain Errand)", err)
	}
	if n := f.ob.sentCount(); n != 1 {
		t.Fatalf("SendErrand calls = %d, want 1", n)
	}
}

// TestDispatch_DryRunGateRunsAfterValidation_Order — validation still comes first:
// a malformed request is a client error and must read as one, not as "your fleet
// is too old". Guards the ordering inside Dispatch against a later reshuffle.
func TestDispatch_DryRunGateRunsAfterValidation_Order(t *testing.T) {
	cap := &stubSoulCap{lacking: []string{gateSID}}
	f := newGateFixture(cap)

	req := dryRunReq()
	req.Module = ""
	_, err := f.d.Dispatch(context.Background(), req)
	if !errors.Is(err, ErrModuleEmpty) {
		t.Fatalf("error = %v, want ErrModuleEmpty (validation precedes the capability gate)", err)
	}
	if n := cap.callCount(); n != 0 {
		t.Errorf("capability checker consulted %d times on an invalid request, want 0", n)
	}
}

// TestDispatch_LackingButNoLease_ReportsNotConnected — ★ the diagnosis guard.
// [redis.SoulsLackingCapability] reports a SID as lacking both when its caps field
// omits the capability AND when the heartbeat hash is absent entirely (its own
// fail-closed default), so an unknown or never-connected SID arrives at the gate
// looking exactly like an outdated agent. Taken at face value the operator is told
// to upgrade the binary on a host that may not exist, while the routes document
// 404 for a Soul that is not connected. So a lacking verdict is cross-checked
// against the session lease: no holder → not connected, and that is what is said.
func TestDispatch_LackingButNoLease_ReportsNotConnected(t *testing.T) {
	f := newGateFixture(&stubSoulCap{lacking: []string{gateSID}})
	// Authoritative empty lease: the SID is known to hold no session anywhere —
	// the same signal Dispatcher.send treats as ErrSoulNotConnected.
	f.d.deps.LeaseLookup = &fakeLease{holders: map[string]string{}}

	_, err := f.d.Dispatch(context.Background(), dryRunReq())
	if err == nil {
		t.Fatal("a dry_run to a host with no session must be refused")
	}
	if !errors.Is(err, ErrSoulNotConnected) {
		t.Fatalf("error = %v, want ErrSoulNotConnected (handlers map it to the documented 404); "+
			"reporting an absent presence record as an outdated binary sends the operator after the wrong host", err)
	}
	if errors.Is(err, ErrDryRunNotAnnounced) {
		t.Error("a host that holds no lease must not be reported as an un-announcing binary")
	}
	f.assertNothingHappened(t)
}

// TestDispatch_LackingWithUnreadableLease_StaysNotAnnounced — the other side of
// that cross-check: if the lease itself cannot be read, the verdict stays
// not-announced. Both outcomes are refusals, so nothing unsafe follows from the
// ambiguity — what must not happen is falling through to a dispatch.
func TestDispatch_LackingWithUnreadableLease_StaysNotAnnounced(t *testing.T) {
	f := newGateFixture(&stubSoulCap{lacking: []string{gateSID}})
	f.d.deps.LeaseLookup = &fakeLease{lookupErr: errors.New("redis: connection refused")}

	_, err := f.d.Dispatch(context.Background(), dryRunReq())
	if !errors.Is(err, ErrDryRunNotAnnounced) {
		t.Fatalf("error = %v, want ErrDryRunNotAnnounced (an unreadable lease must not turn the refusal into a dispatch)", err)
	}
	f.assertNothingHappened(t)
}

// TestDispatch_DryRunRefusal_NeverAdvisesARealApply — the refusal text is read by
// an operator mid-incident and by an agent deciding what to do next. "Repeat
// without dry_run" is the one suggestion it must never make: that is a real Apply
// on a host the operator asked only to read, i.e. exactly the mutation the gate
// was built to prevent.
func TestDispatch_DryRunRefusal_NeverAdvisesARealApply(t *testing.T) {
	for name, cap := range map[string]SoulCapabilityChecker{
		"not announced": &stubSoulCap{lacking: []string{gateSID}},
		"checker error": &stubSoulCap{err: errors.New("redis: down")},
		"nil checker":   nil,
	} {
		t.Run(name, func(t *testing.T) {
			f := newGateFixture(cap)
			_, err := f.d.Dispatch(context.Background(), dryRunReq())
			if err == nil {
				t.Fatal("expected a refusal")
			}
			for _, forbidden := range []string{"without dry_run", "dry_run=false", "omit dry_run", "apply deliberately"} {
				if strings.Contains(err.Error(), forbidden) {
					t.Errorf("refusal = %q suggests %q - that is the real Apply this gate refuses", err, forbidden)
				}
			}
		})
	}
}
