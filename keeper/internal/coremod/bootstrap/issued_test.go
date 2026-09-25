package bootstrap_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/souls-guild/soul-stack/keeper/internal/bootstraptoken"
	coremodbootstrap "github.com/souls-guild/soul-stack/keeper/internal/coremod/bootstrap"
	"github.com/souls-guild/soul-stack/keeper/internal/coremod/internaltest"
	coremodutil "github.com/souls-guild/soul-stack/keeper/internal/coremod/util"
	"github.com/souls-guild/soul-stack/shared/audit"

	pluginv1 "github.com/souls-guild/soul-stack/proto/plugin/gen/go/v1"
)

type fakeIssuer struct {
	hosts        []coremodbootstrap.IssuedHost
	err          error
	calls        [][]string
	incarnations []string
	reissues     []bool
}

func (f *fakeIssuer) IssueBatch(_ context.Context, sids []string, incarnationName string, reissue bool) ([]coremodbootstrap.IssuedHost, error) {
	f.calls = append(f.calls, append([]string(nil), sids...))
	f.incarnations = append(f.incarnations, incarnationName)
	f.reissues = append(f.reissues, reissue)
	return f.hosts, f.err
}

func issuedReq(t *testing.T, sids ...string) *pluginv1.ApplyRequest {
	t.Helper()
	return issuedReqParams(t, map[string]any{"sids": sidValues(sids)})
}

func issuedReqParams(t *testing.T, params map[string]any) *pluginv1.ApplyRequest {
	t.Helper()
	return &pluginv1.ApplyRequest{
		State:  coremodbootstrap.StateIssued,
		Params: mustStruct(t, params),
	}
}

func sidValues(sids []string) []any {
	values := make([]any, len(sids))
	for i, sid := range sids {
		values[i] = sid
	}
	return values
}

func TestApplyIssued_OutputForDeliveryAndAuditWithoutSecret(t *testing.T) {
	tok1, err := bootstraptoken.Generate()
	if err != nil {
		t.Fatal(err)
	}
	tok2, err := bootstraptoken.Generate()
	if err != nil {
		t.Fatal(err)
	}
	expires := time.Now().UTC().Add(time.Hour).Truncate(time.Second)
	issuer := &fakeIssuer{hosts: []coremodbootstrap.IssuedHost{
		{SID: "vm1.example.com", Token: tok1, ExpiresAt: expires, Created: true},
		{SID: "vm2.example.com", Token: tok2, ExpiresAt: expires, Reissued: true},
	}}
	aud := &fakeAudit{}
	m := &coremodbootstrap.Module{Issuer: issuer, Audit: aud}
	stream := internaltest.NewApplyStream()

	if err := m.Apply(issuedReq(t, "vm1.example.com", "vm2.example.com"), stream); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	last := stream.Last()
	if last == nil || last.GetFailed() || !last.GetChanged() {
		t.Fatalf("issued final flags = present:%t failed:%t changed:%t, want present changed success",
			last != nil, last != nil && last.GetFailed(), last != nil && last.GetChanged())
	}
	out := last.GetOutput().AsMap()
	if out["action"] != coremodbootstrap.StateIssued || out["count"] != float64(2) || out["created"] != float64(1) || out["reissued"] != float64(1) {
		t.Fatalf("issued action/count/created/reissued = %v/%v/%v/%v",
			out["action"], out["count"], out["created"], out["reissued"])
	}
	hosts, _ := out["hosts"].([]any)
	if len(hosts) != 2 {
		t.Fatalf("hosts length = %d, want 2", len(hosts))
	}
	first := hosts[0].(map[string]any)
	if first["sid"] != "vm1.example.com" || first["bootstrap_token"] != tok1.Reveal() || first["expires_at"] != expires.Format(time.RFC3339) {
		t.Fatalf("first host delivery fields are incomplete or mismatched (sid=%v expires_at=%v)",
			first["sid"], first["expires_at"])
	}

	// The register is the one intentional plaintext boundary. Every observable
	// projection that uses the common masker redacts bootstrap_token by key.
	masked := audit.MaskSecrets(out)
	maskedHosts := masked["hosts"].([]any)
	if got := maskedHosts[0].(map[string]any)["bootstrap_token"]; got != "***MASKED***" {
		t.Fatalf("bootstrap_token mask = %v, want ***MASKED***", got)
	}

	if len(aud.events) != 1 {
		t.Fatalf("audit events = %d, want 1", len(aud.events))
	}
	ev := aud.events[0]
	if ev.EventType != audit.EventBootstrapIssued || ev.Payload["action"] != coremodbootstrap.StateIssued {
		t.Fatalf("audit type/action = %q/%v, want %q/%q",
			ev.EventType, ev.Payload["action"], audit.EventBootstrapIssued, coremodbootstrap.StateIssued)
	}
	for _, token := range []string{tok1.Reveal(), tok2.Reveal()} {
		if containsTokenDeep(ev.Payload, token) {
			t.Fatal("SECURITY: plaintext token leaked into audit payload")
		}
	}
}

func TestApplyIssued_InputGuardBeforeMutation(t *testing.T) {
	cases := []struct {
		name string
		req  *pluginv1.ApplyRequest
		want string
	}{
		{name: "empty", req: issuedReq(t), want: "empty list"},
		{name: "duplicate", req: issuedReq(t, "vm1.example.com", "vm1.example.com"), want: "duplicate sid"},
		{name: "invalid", req: issuedReq(t, "UPPER.example.com"), want: "invalid sid"},
		{name: "reserved", req: issuedReq(t, "keeper"), want: "invalid sid"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			issuer := &fakeIssuer{}
			m := &coremodbootstrap.Module{Issuer: issuer}
			stream := internaltest.NewApplyStream()
			if err := m.Apply(tc.req, stream); err != nil {
				t.Fatalf("Apply: %v", err)
			}
			last := stream.Last()
			if last == nil || !last.GetFailed() || !strings.Contains(last.GetMessage(), tc.want) {
				t.Fatalf("failed event present/failed/message-match = %t/%t/%t, want true/true/true",
					last != nil, last != nil && last.GetFailed(), last != nil && strings.Contains(last.GetMessage(), tc.want))
			}
			if len(issuer.calls) != 0 {
				t.Fatalf("issuer called for invalid input: %v", issuer.calls)
			}
		})
	}
}

func TestApplyIssued_HostFailureIsDiagnosableAndHasNoPartialOutput(t *testing.T) {
	issuer := &fakeIssuer{err: &coremodbootstrap.SIDIssueError{
		SID: "vm2.example.com",
		Err: errors.New("Soul is already onboarded (status \"connected\"); refusing identity takeover"),
	}}
	m := &coremodbootstrap.Module{Issuer: issuer, Audit: &fakeAudit{}}
	stream := internaltest.NewApplyStream()
	if err := m.Apply(issuedReq(t, "vm1.example.com", "vm2.example.com"), stream); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	last := stream.Last()
	if last == nil || !last.GetFailed() {
		t.Fatalf("final flags = present:%t failed:%t, want failed", last != nil, last != nil && last.GetFailed())
	}
	if !strings.Contains(last.GetMessage(), "vm2.example.com") || !strings.Contains(last.GetMessage(), "identity takeover") {
		t.Fatalf("failure is not host-diagnostic: %q", last.GetMessage())
	}
	if last.GetOutput() != nil {
		t.Fatal("failed issuance exposed partial output")
	}
}

func TestApplyIssued_RejectsIncompleteIssuerResultWithoutLoggingToken(t *testing.T) {
	tok, err := bootstraptoken.Generate()
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name string
		host coremodbootstrap.IssuedHost
	}{
		{name: "wrong sid", host: coremodbootstrap.IssuedHost{SID: "other.example.com", Token: tok, ExpiresAt: time.Now().Add(time.Hour)}},
		{name: "missing token", host: coremodbootstrap.IssuedHost{SID: "vm1.example.com", ExpiresAt: time.Now().Add(time.Hour)}},
		{name: "missing expiry", host: coremodbootstrap.IssuedHost{SID: "vm1.example.com", Token: tok}},
		// Converged AND carrying issuance data is contradictory: the issuer did two
		// mutually exclusive things, and the output would have to drop one of them
		// silently — a host reported as a clean converge while a capability was
		// minted for it (NIM-780). Both halves count, including an expiry with no
		// token behind it.
		{name: "onboarded with a token", host: coremodbootstrap.IssuedHost{SID: "vm1.example.com", Onboarded: true, Token: tok, ExpiresAt: time.Now().Add(time.Hour)}},
		{name: "onboarded with an expiry", host: coremodbootstrap.IssuedHost{SID: "vm1.example.com", Onboarded: true, ExpiresAt: time.Now().Add(time.Hour)}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			stream := internaltest.NewApplyStream()
			m := &coremodbootstrap.Module{Issuer: &fakeIssuer{hosts: []coremodbootstrap.IssuedHost{tc.host}}}
			if err := m.Apply(issuedReq(t, "vm1.example.com"), stream); err != nil {
				t.Fatalf("Apply: %v", err)
			}
			last := stream.Last()
			if last == nil || !last.GetFailed() || strings.Contains(last.GetMessage(), tok.Reveal()) {
				t.Fatalf("incomplete issuer result was not rejected safely (present=%t failed=%t)",
					last != nil, last != nil && last.GetFailed())
			}
			if last.GetOutput() != nil {
				t.Fatal("incomplete issuer result exposed output")
			}
		})
	}
}

// ★ NIM-780, the step's half. The issuer decides WHICH hosts are converged over;
// this is what the scenario downstream can see of that decision, and the shape
// is not free: core.bootstrap.delivered refuses an empty `hosts` list and
// refuses a host with no `bootstrap_token` UNLESS it is flagged `onboarded`. So
// a skipped SID has to stay in the list, in place, carrying the flag — dropping
// it would make a fully converged re-run fail at delivery on an empty list,
// which is the same dead end from the other side.
func TestApplyIssued_ConvergedHostKeepsItsSlotAndCarriesTheFlag(t *testing.T) {
	tok, err := bootstraptoken.Generate()
	if err != nil {
		t.Fatal(err)
	}
	expires := time.Now().UTC().Add(time.Hour).Truncate(time.Second)
	issuer := &fakeIssuer{hosts: []coremodbootstrap.IssuedHost{
		{SID: "live.example.com", Onboarded: true},
		{SID: "fresh.example.com", Token: tok, ExpiresAt: expires, Created: true},
	}}
	aud := &fakeAudit{}
	m := &coremodbootstrap.Module{Issuer: issuer, Audit: aud}
	stream := internaltest.NewApplyStreamCtx(
		coremodutil.WithIncarnation(context.Background(), "redis-sa"))

	if err := m.Apply(issuedReq(t, "live.example.com", "fresh.example.com"), stream); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	last := stream.Last()
	if last == nil || last.GetFailed() {
		t.Fatalf("mixed batch failed: %q", last.GetMessage())
	}

	// The run's incarnation reaches the issuer, or ownership is decided against
	// "" and every converged host turns back into a refusal.
	if len(issuer.incarnations) != 1 || issuer.incarnations[0] != "redis-sa" {
		t.Fatalf("issuer saw incarnations %v, want [redis-sa] from the run context", issuer.incarnations)
	}

	out := last.GetOutput().AsMap()
	if out["count"] != float64(2) || out["skipped"] != float64(1) || out["created"] != float64(1) {
		t.Fatalf("count/skipped/created = %v/%v/%v, want 2/1/1", out["count"], out["skipped"], out["created"])
	}
	hosts, _ := out["hosts"].([]any)
	if len(hosts) != 2 {
		t.Fatalf("hosts length = %d, want both requested SIDs", len(hosts))
	}
	live, _ := hosts[0].(map[string]any)
	if live["sid"] != "live.example.com" || live["onboarded"] != true {
		t.Fatalf("converged entry = %v, want the requested sid flagged onboarded", live)
	}
	if _, has := live["bootstrap_token"]; has {
		t.Error("converged entry carries a bootstrap_token — it was issued none, and a value there would be somebody's live capability")
	}
	if _, has := live["expires_at"]; has {
		t.Error("converged entry carries expires_at with no token behind it")
	}
	fresh, _ := hosts[1].(map[string]any)
	if fresh["sid"] != "fresh.example.com" || fresh["bootstrap_token"] != tok.Reveal() || fresh["onboarded"] != nil {
		t.Fatalf("issued entry = %v, want a normal token entry for the second sid", fresh)
	}

	// The audit still answers for every requested SID: a host that needed no
	// token is a fact about the run, not an absence of one.
	ev := aud.events[0]
	if ev.Payload["skipped"] != float64(1) {
		t.Errorf("audit skipped = %v, want 1", ev.Payload["skipped"])
	}
	sids, _ := ev.Payload["sids"].([]any)
	if len(sids) != 2 || sids[0] != "live.example.com" || sids[1] != "fresh.example.com" {
		t.Errorf("audit sids = %v, want both requested SIDs", sids)
	}
}

// ★★ NIM-900, the pinned flag. `changed` was the constant `true`, so a run over a
// fleet that was already up — nothing issued, every host passed through — still
// reported itself as having changed something. Everything keyed on `changed`
// downstream (`onchanges:`, the run's own changed-count, an operator reading the
// report) then said a repeat did work it did not do.
//
// The other two tokenless outcomes are here for the same reason, because the
// constant made all three indistinguishable from an issuance.
func TestApplyIssued_ChangedIsFalseWhenNothingWasIssued(t *testing.T) {
	tok, err := bootstraptoken.Generate()
	if err != nil {
		t.Fatal(err)
	}
	expires := time.Now().UTC().Add(time.Hour).Truncate(time.Second)
	cases := []struct {
		name        string
		hosts       []coremodbootstrap.IssuedHost
		wantChanged bool
	}{
		{
			name: "every host already onboarded",
			hosts: []coremodbootstrap.IssuedHost{
				{SID: "vm1.example.com", Onboarded: true},
				{SID: "vm2.example.com", Onboarded: true},
			},
		},
		{
			name: "every host keeps the token it already holds",
			hosts: []coremodbootstrap.IssuedHost{
				{SID: "vm1.example.com", TokenHeld: true},
				{SID: "vm2.example.com", TokenHeld: true},
			},
		},
		{
			name: "onboarded and token-held in one batch",
			hosts: []coremodbootstrap.IssuedHost{
				{SID: "vm1.example.com", Onboarded: true},
				{SID: "vm2.example.com", TokenHeld: true},
			},
		},
		{
			name: "one token issued among two pass-throughs",
			hosts: []coremodbootstrap.IssuedHost{
				{SID: "vm1.example.com", Onboarded: true},
				{SID: "vm2.example.com", Token: tok, ExpiresAt: expires, Created: true},
			},
			wantChanged: true,
		},
		// ★ The case that makes `created + reissued > 0` the WRONG predicate: an
		// `expired` Soul is re-armed with a fresh token, its row already existed so
		// nothing was created, and there was no unused token to invalidate so
		// nothing was reissued. A capability was handed out all the same, and the
		// acceptance rule is "issued or reissued → changed".
		{
			name: "re-armed host: a token with neither created nor reissued set",
			hosts: []coremodbootstrap.IssuedHost{
				{SID: "vm1.example.com", Token: tok, ExpiresAt: expires},
			},
			wantChanged: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sids := make([]string, len(tc.hosts))
			for i, h := range tc.hosts {
				sids[i] = h.SID
			}
			aud := &fakeAudit{}
			m := &coremodbootstrap.Module{Issuer: &fakeIssuer{hosts: tc.hosts}, Audit: aud}
			stream := internaltest.NewApplyStream()
			if err := m.Apply(issuedReq(t, sids...), stream); err != nil {
				t.Fatalf("Apply: %v", err)
			}
			last := stream.Last()
			if last == nil || last.GetFailed() {
				t.Fatalf("batch failed: %q", last.GetMessage())
			}
			if last.GetChanged() != tc.wantChanged {
				t.Errorf("changed = %t, want %t — the flag must follow whether a token was issued, not the shape of the call",
					last.GetChanged(), tc.wantChanged)
			}
			// The audit still answers for every requested host either way: a run that
			// changed nothing is a fact about it, not an absence of one.
			if len(aud.events) != 1 {
				t.Fatalf("audit events = %d, want 1", len(aud.events))
			}
			if got := aud.events[0].Payload["count"]; got != float64(len(tc.hosts)) {
				t.Errorf("audit count = %v, want %d", got, len(tc.hosts))
			}
		})
	}
}

// ★★ NIM-900, the THIRD entry shape. A host whose active token `reissue: false`
// left alone comes back with no `bootstrap_token` key AT ALL — the plaintext is
// unrecoverable, Postgres holds only the SHA-256 — so every consumer of
// `register.<name>.hosts` now has two flags to branch on before reaching for the
// token, and reading an absent key in CEL is an error rather than an empty
// string.
//
// Writing an empty token in place of the flag would satisfy the letter and break
// the same thing, exactly as it would for `onboarded` (NIM-780): the consumer
// would then treat a host holding a live capability as one still waiting for one.
func TestApplyIssued_TokenHeldEntryIsTheThirdShapeAndCarriesNoToken(t *testing.T) {
	tok, err := bootstraptoken.Generate()
	if err != nil {
		t.Fatal(err)
	}
	expires := time.Now().UTC().Add(time.Hour).Truncate(time.Second)
	issuer := &fakeIssuer{hosts: []coremodbootstrap.IssuedHost{
		{SID: "onboarded.example.com", Onboarded: true},
		{SID: "holding.example.com", TokenHeld: true},
		{SID: "fresh.example.com", Token: tok, ExpiresAt: expires, Created: true},
	}}
	aud := &fakeAudit{}
	m := &coremodbootstrap.Module{Issuer: issuer, Audit: aud}
	stream := internaltest.NewApplyStream()
	if err := m.Apply(issuedReq(t,
		"onboarded.example.com", "holding.example.com", "fresh.example.com"), stream); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	last := stream.Last()
	if last == nil || last.GetFailed() {
		t.Fatalf("mixed batch failed: %q", last.GetMessage())
	}
	out := last.GetOutput().AsMap()
	if out["count"] != float64(3) || out["skipped"] != float64(1) || out["held"] != float64(1) || out["created"] != float64(1) {
		t.Fatalf("count/skipped/held/created = %v/%v/%v/%v, want 3/1/1/1",
			out["count"], out["skipped"], out["held"], out["created"])
	}
	hosts, _ := out["hosts"].([]any)
	if len(hosts) != 3 {
		t.Fatalf("hosts length = %d, want every requested SID", len(hosts))
	}

	// The three shapes are told apart by which key is present, and a held host
	// carries exactly one key beyond the sid.
	onboarded, _ := hosts[0].(map[string]any)
	if onboarded["onboarded"] != true || onboarded["token_held"] != nil {
		t.Errorf("onboarded entry = %v, want only the onboarded flag", onboarded)
	}
	holding, _ := hosts[1].(map[string]any)
	if holding["sid"] != "holding.example.com" || holding["token_held"] != true {
		t.Fatalf("held entry = %v, want the requested sid flagged token_held", holding)
	}
	if holding["onboarded"] != nil {
		t.Error("held entry carries onboarded — it holds a capability precisely BECAUSE it has no identity yet")
	}
	for _, key := range []string{"bootstrap_token", "expires_at", "created", "reissued"} {
		if _, has := holding[key]; has {
			t.Errorf("held entry carries %q — nothing was issued for it, and the plaintext of what it holds is gone", key)
		}
	}
	if len(holding) != 2 {
		t.Errorf("held entry has %d keys (%v), want exactly sid + token_held", len(holding), holding)
	}
	fresh, _ := hosts[2].(map[string]any)
	if fresh["bootstrap_token"] != tok.Reveal() || fresh["token_held"] != nil {
		t.Errorf("issued entry = %v, want a normal token entry", fresh)
	}

	if got := aud.events[0].Payload["held"]; got != float64(1) {
		t.Errorf("audit held = %v, want 1", got)
	}
}

// The flag reaches the issuer, and its DEFAULT is the safe one. A default of
// `true` would silently keep the pre-NIM-900 behaviour — killing a token the
// operator may already have delivered — for every scenario that never mentions
// the key.
func TestApplyIssued_ReissueDefaultsToFalseAndReachesTheIssuer(t *testing.T) {
	cases := []struct {
		name   string
		params map[string]any
		want   bool
	}{
		{name: "absent", params: map[string]any{"sids": sidValues([]string{"vm1.example.com"})}, want: false},
		{name: "explicit false", params: map[string]any{"sids": sidValues([]string{"vm1.example.com"}), "reissue": false}, want: false},
		{name: "explicit true", params: map[string]any{"sids": sidValues([]string{"vm1.example.com"}), "reissue": true}, want: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			issuer := &fakeIssuer{hosts: []coremodbootstrap.IssuedHost{{SID: "vm1.example.com", TokenHeld: true}}}
			m := &coremodbootstrap.Module{Issuer: issuer}
			stream := internaltest.NewApplyStream()
			if err := m.Apply(issuedReqParams(t, tc.params), stream); err != nil {
				t.Fatalf("Apply: %v", err)
			}
			if last := stream.Last(); last == nil || last.GetFailed() {
				t.Fatalf("Apply failed: %q", last.GetMessage())
			}
			if len(issuer.reissues) != 1 || issuer.reissues[0] != tc.want {
				t.Fatalf("issuer saw reissue %v, want [%t]", issuer.reissues, tc.want)
			}
		})
	}
}

// A non-bool `reissue` is refused by the same gate as a bad SID — before the
// issuer is called, and identically by Validate, so soul-lint and the runtime
// agree offline.
func TestIssued_NonBoolReissueIsRefusedBeforeMutation(t *testing.T) {
	req := issuedReqParams(t, map[string]any{
		"sids":    sidValues([]string{"vm1.example.com"}),
		"reissue": "yes",
	})
	issuer := &fakeIssuer{}
	m := &coremodbootstrap.Module{Issuer: issuer}
	stream := internaltest.NewApplyStream()
	if err := m.Apply(req, stream); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	last := stream.Last()
	if last == nil || !last.GetFailed() || !strings.Contains(last.GetMessage(), "reissue") {
		t.Fatalf("non-bool reissue was not refused: present=%t failed=%t message=%q",
			last != nil, last != nil && last.GetFailed(), last.GetMessage())
	}
	if len(issuer.calls) != 0 {
		t.Fatalf("issuer called for invalid input: %v", issuer.calls)
	}
	reply, err := m.Validate(context.Background(), &pluginv1.ValidateRequest{
		State:  coremodbootstrap.StateIssued,
		Params: req.Params,
	})
	if err != nil || reply.Ok || len(reply.Errors) == 0 || !strings.Contains(reply.Errors[0], "reissue") {
		t.Fatalf("Validate accepted a non-bool reissue: reply=%v err=%v", reply, err)
	}
}

// The two tokenless shapes are different facts, so an entry claiming both — or
// claiming one while carrying issuance data — tells a reader nothing and would
// have to be collapsed into one of them silently. IssuerPG cannot produce these;
// the guard is for the next implementation of the interface.
func TestApplyIssued_RejectsContradictoryTokenHeldResult(t *testing.T) {
	tok, err := bootstraptoken.Generate()
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name string
		host coremodbootstrap.IssuedHost
		want string
	}{
		{
			name: "token-held and onboarded",
			host: coremodbootstrap.IssuedHost{SID: "vm1.example.com", TokenHeld: true, Onboarded: true},
			want: "onboarded AND token-held",
		},
		{
			name: "token-held with a token",
			host: coremodbootstrap.IssuedHost{SID: "vm1.example.com", TokenHeld: true, Token: tok, ExpiresAt: time.Now().Add(time.Hour)},
			want: "token-held AND with issuance data",
		},
		{
			name: "token-held with an expiry",
			host: coremodbootstrap.IssuedHost{SID: "vm1.example.com", TokenHeld: true, ExpiresAt: time.Now().Add(time.Hour)},
			want: "token-held AND with issuance data",
		},
		// A soul row cannot be created for a host that already holds a token — the
		// token's FK needs the row — so either flag beside TokenHeld means the issuer
		// wrote something it then reported as untouched.
		{
			name: "token-held and created",
			host: coremodbootstrap.IssuedHost{SID: "vm1.example.com", TokenHeld: true, Created: true},
			want: "token-held AND with issuance data",
		},
		{
			name: "token-held and reissued",
			host: coremodbootstrap.IssuedHost{SID: "vm1.example.com", TokenHeld: true, Reissued: true},
			want: "token-held AND with issuance data",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := &coremodbootstrap.Module{Issuer: &fakeIssuer{hosts: []coremodbootstrap.IssuedHost{tc.host}}}
			stream := internaltest.NewApplyStream()
			if err := m.Apply(issuedReq(t, "vm1.example.com"), stream); err != nil {
				t.Fatalf("Apply: %v", err)
			}
			last := stream.Last()
			if last == nil || !last.GetFailed() || !strings.Contains(last.GetMessage(), tc.want) {
				t.Fatalf("contradictory result not rejected: present=%t failed=%t message=%q",
					last != nil, last != nil && last.GetFailed(), last.GetMessage())
			}
			if strings.Contains(last.GetMessage(), tok.Reveal()) {
				t.Fatal("SECURITY: plaintext token leaked into the refusal")
			}
			if last.GetOutput() != nil {
				t.Fatal("contradictory issuer result exposed output")
			}
		})
	}
}

func TestValidateIssued_UsesSameUniqueSIDGuard(t *testing.T) {
	m := &coremodbootstrap.Module{}
	good, err := m.Validate(context.Background(), &pluginv1.ValidateRequest{
		State:  coremodbootstrap.StateIssued,
		Params: issuedReq(t, "vm1.example.com").Params,
	})
	if err != nil || !good.Ok {
		t.Fatalf("valid issued rejected: reply=%v err=%v", good, err)
	}
	bad, err := m.Validate(context.Background(), &pluginv1.ValidateRequest{
		State:  coremodbootstrap.StateIssued,
		Params: issuedReq(t, "vm1.example.com", "vm1.example.com").Params,
	})
	if err != nil || bad.Ok || len(bad.Errors) == 0 || !strings.Contains(bad.Errors[0], "duplicate") {
		t.Fatalf("duplicate issued validation = reply=%v err=%v", bad, err)
	}
}
