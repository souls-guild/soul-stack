package soul_test

import (
	"context"
	"fmt"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/souls-guild/soul-stack/keeper/internal/coremod/internaltest"
	coremodsoul "github.com/souls-guild/soul-stack/keeper/internal/coremod/soul"
	"github.com/souls-guild/soul-stack/keeper/internal/coremod/util"
	keepersoul "github.com/souls-guild/soul-stack/keeper/internal/soul"
	"github.com/souls-guild/soul-stack/keeper/internal/soulforget"
	"github.com/souls-guild/soul-stack/shared/audit"

	pluginv1 "github.com/souls-guild/soul-stack/proto/plugin/gen/go/v1"
)

// fakeForgetter holds the registry as sid → the incarnations it is a member of; a
// sid is registered iff it is a key. Its Forget applies the ownership rule the real
// transaction re-checks under the row lock, so a test that drops the module's own
// pre-check still sees a refusal — but only after the hosts sorted before the shared
// one were already forgotten. That is the failure "check every member first"
// prevents.
type fakeForgetter struct {
	registry map[string][]string
	// voices maps a sid to the incarnations whose Choir holds a Voice of it.
	voices map[string][]string

	membersCalls  int
	forgetCalls   []string
	forgetReasons []string

	// movedBeforeForget rewrites a host's memberships between the read and the
	// delete, as a concurrent bind would.
	movedBeforeForget map[string][]string
	// goneBeforeForget removes a host's row between the read and the delete.
	goneBeforeForget map[string]bool
	forgetErr        map[string]error
	warnings         map[string][]string
}

func newFakeForgetter(registry map[string][]string) *fakeForgetter {
	cp := map[string][]string{}
	for sid, incs := range registry {
		cp[sid] = append([]string{}, incs...)
	}
	return &fakeForgetter{registry: cp, voices: map[string][]string{}}
}

func (f *fakeForgetter) ties(sid string) soulforget.Ties {
	memberOf := append([]string{}, f.registry[sid]...)
	sort.Strings(memberOf)
	return soulforget.Ties{MemberOf: memberOf, VoiceIn: append([]string{}, f.voices[sid]...)}
}

func (f *fakeForgetter) Members(_ context.Context, incarnation string) (map[string]soulforget.Ties, error) {
	f.membersCalls++
	out := map[string]soulforget.Ties{}
	for sid, incs := range f.registry {
		for _, inc := range incs {
			if inc == incarnation {
				out[sid] = f.ties(sid)
				break
			}
		}
	}
	return out, nil
}

func (f *fakeForgetter) Forget(_ context.Context, sid, incarnation, reason string) (soulforget.Result, error) {
	f.forgetCalls = append(f.forgetCalls, sid)
	f.forgetReasons = append(f.forgetReasons, reason)
	if err := f.forgetErr[sid]; err != nil {
		return soulforget.Result{}, err
	}
	if f.goneBeforeForget[sid] {
		delete(f.registry, sid)
	}
	if moved, ok := f.movedBeforeForget[sid]; ok {
		f.registry[sid] = moved
	}
	memberOf, ok := f.registry[sid]
	if !ok {
		return soulforget.Result{}, keepersoul.ErrSoulNotFound
	}
	if err := soulforget.OwnedBy(sid, incarnation, f.ties(sid)); err != nil {
		return soulforget.Result{}, err
	}
	delete(f.registry, sid)
	delete(f.voices, sid)
	return soulforget.Result{
		Counts:  soulforget.Counts{StatusBefore: "connected", MembershipsSevered: int64(len(memberOf))},
		Release: soulforget.Release{Broadcast: true, Warnings: f.warnings[sid]},
	}, nil
}

func (f *fakeForgetter) registered() []string {
	out := []string{}
	for sid := range f.registry {
		out = append(out, sid)
	}
	sort.Strings(out)
	return out
}

type recordingAudit struct{ events []*audit.Event }

func (a *recordingAudit) Write(_ context.Context, e *audit.Event) error {
	a.events = append(a.events, e)
	return nil
}

func incCtx() context.Context {
	ctx := util.WithIncarnation(context.Background(), "inc-one")
	return util.WithRunScope(ctx, util.RunScope{Scenario: "destroy", ApplyID: "01JAPPLY", Destroy: true})
}

var scopeIncarnation = map[string]any{"scope": "incarnation"}

func applyForgotten(t *testing.T, ctx context.Context, f coremodsoul.Forgetter, a coremodsoul.AuditWriter, params map[string]any) *pluginv1.ApplyEvent {
	t.Helper()
	m := coremodsoul.New(newFakeStore())
	if f != nil {
		m.WithForget(f, a)
	}
	stream := internaltest.NewApplyStreamCtx(ctx)
	req := &pluginv1.ApplyRequest{State: coremodsoul.StateForgotten, Params: mustStruct(t, params)}
	if err := m.Apply(req, stream); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	ev := stream.Last()
	if ev == nil {
		t.Fatal("no final event")
	}
	return ev
}

func outList(t *testing.T, ev *pluginv1.ApplyEvent, key string) []string {
	t.Helper()
	raw, ok := ev.GetOutput().AsMap()[key].([]any)
	if !ok {
		t.Fatalf("output has no %q list: %v", key, ev.GetOutput().AsMap())
	}
	out := make([]string, len(raw))
	for i, x := range raw {
		out[i] = x.(string)
	}
	return out
}

// TestForgotten_ForgetsTheIncarnationsMembersOnly — every member of the running
// incarnation goes, sorted; a host of ANOTHER incarnation only, and a registered host
// of no incarnation, are never touched.
func TestForgotten_ForgetsTheIncarnationsMembersOnly(t *testing.T) {
	f := newFakeForgetter(map[string][]string{
		"b.example.com":       {"inc-one"},
		"a.example.com":       {"inc-one"},
		"other.example.com":   {"inc-two"},
		"pending.example.com": {},
	})
	a := &recordingAudit{}
	ev := applyForgotten(t, incCtx(), f, a, scopeIncarnation)
	if ev.GetFailed() {
		t.Fatalf("failed: %s", ev.GetMessage())
	}
	if !ev.GetChanged() {
		t.Error("changed=false although two hosts were forgotten")
	}
	want := []string{"a.example.com", "b.example.com"}
	if got := outList(t, ev, "forgotten"); !reflect.DeepEqual(got, want) {
		t.Errorf("forgotten = %v, want %v", got, want)
	}
	if !reflect.DeepEqual(f.forgetCalls, want) {
		t.Errorf("Forget calls = %v, want %v", f.forgetCalls, want)
	}
	if got := f.registered(); !reflect.DeepEqual(got, []string{"other.example.com", "pending.example.com"}) {
		t.Errorf("registry after = %v, want the other incarnation's host and the non-member", got)
	}
	if len(a.events) != 2 {
		t.Fatalf("audit events = %d, want one per forgotten host", len(a.events))
	}
	for i, e := range a.events {
		if e.EventType != audit.EventSoulForgotten || e.Source != audit.SourceKeeperInternal {
			t.Errorf("event %d = %s/%s, want soul.forgotten/keeper_internal", i, e.EventType, e.Source)
		}
		if e.Payload["sid"] != want[i] || e.Payload["incarnation"] != "inc-one" ||
			e.Payload["scenario"] != "destroy" || e.Payload["apply_id"] != "01JAPPLY" {
			t.Errorf("event %d payload = %v", i, e.Payload)
		}
	}
	if !strings.Contains(f.forgetReasons[0], "scenario destroy of incarnation inc-one") {
		t.Errorf("reason = %q, want it to name the run", f.forgetReasons[0])
	}
}

// TestForgotten_SharedHostFailsWithNothingForgotten — a member that is ALSO a member
// of another incarnation is refused: forgetting it would sever that incarnation's
// roster and Voice. The own member sorted BEFORE it is not forgotten either — every
// member is checked first.
func TestForgotten_SharedHostFailsWithNothingForgotten(t *testing.T) {
	f := newFakeForgetter(map[string][]string{
		"a-own.example.com":    {"inc-one"},
		"b-shared.example.com": {"inc-one", "inc-two"},
	})
	before := f.registered()
	a := &recordingAudit{}
	ev := applyForgotten(t, incCtx(), f, a, scopeIncarnation)
	if !ev.GetFailed() {
		t.Fatalf("a shared host was accepted; forgotten=%v", f.forgetCalls)
	}
	if len(f.forgetCalls) != 0 {
		t.Errorf("hosts were forgotten before the refusal: %v", f.forgetCalls)
	}
	if got := f.registered(); !reflect.DeepEqual(got, before) {
		t.Errorf("registry changed: %v -> %v", before, got)
	}
	if len(a.events) != 0 {
		t.Errorf("audit written on a refusal: %d", len(a.events))
	}
	msg := ev.GetMessage()
	if !strings.Contains(msg, "b-shared.example.com") || !strings.Contains(msg, "also a member of inc-two") || !strings.Contains(msg, "nothing was forgotten") {
		t.Errorf("message does not name the shared host, the other incarnation and the outcome: %s", msg)
	}
}

// TestForgotten_VoiceInAnotherIncarnationRefused — a member of this incarnation
// only, unbound from inc-two but still holding a Voice in its Choir, is refused: the
// cascade off `souls` would delete inc-two's Voice. Nothing is forgotten.
func TestForgotten_VoiceInAnotherIncarnationRefused(t *testing.T) {
	f := newFakeForgetter(map[string][]string{
		"a-own.example.com":   {"inc-one"},
		"b-voice.example.com": {"inc-one"},
	})
	f.voices["b-voice.example.com"] = []string{"inc-two"}
	ev := applyForgotten(t, incCtx(), f, &recordingAudit{}, scopeIncarnation)
	if !ev.GetFailed() {
		t.Fatalf("a host with a Voice in another incarnation was accepted; forgotten=%v", f.forgetCalls)
	}
	if len(f.forgetCalls) != 0 {
		t.Errorf("hosts were forgotten before the refusal: %v", f.forgetCalls)
	}
	if !strings.Contains(ev.GetMessage(), "holds a Choir Voice in inc-two") {
		t.Errorf("message does not name the Voice: %s", ev.GetMessage())
	}
}

// TestForgotten_RefusedOutsideTheDestroyRun — in any run but the incarnation's
// destroy (an `update`, or a run whose kind is unknown) the step refuses before the
// registry is read: otherwise `incarnation.run` could erase hosts past
// `soul.forget`.
func TestForgotten_RefusedOutsideTheDestroyRun(t *testing.T) {
	inc := util.WithIncarnation(context.Background(), "inc-one")
	for name, ctx := range map[string]context.Context{
		"update run": util.WithRunScope(inc, util.RunScope{Scenario: "update", ApplyID: "01JAPPLY"}),
		"scenario named destroy, not the destroy run": util.WithRunScope(inc, util.RunScope{Scenario: "destroy", ApplyID: "01JAPPLY"}),
		"run kind unknown":                            inc,
	} {
		t.Run(name, func(t *testing.T) {
			f := newFakeForgetter(map[string][]string{"a.example.com": {"inc-one"}})
			ev := applyForgotten(t, ctx, f, &recordingAudit{}, scopeIncarnation)
			if !ev.GetFailed() || !strings.Contains(ev.GetMessage(), "destroy run") {
				t.Fatalf("failed=%v msg=%q, want a refusal naming the destroy run", ev.GetFailed(), ev.GetMessage())
			}
			if f.membersCalls != 0 || len(f.forgetCalls) != 0 {
				t.Errorf("registry touched outside destroy: members=%d forget=%v", f.membersCalls, f.forgetCalls)
			}
			if got := f.registered(); !reflect.DeepEqual(got, []string{"a.example.com"}) {
				t.Errorf("registry after = %v", got)
			}
		})
	}
}

// TestForgotten_NoMembersIsNoOp — an incarnation with no members (it never got a
// host, or a re-run after all are gone): changed=false, nothing forgotten.
func TestForgotten_NoMembersIsNoOp(t *testing.T) {
	f := newFakeForgetter(map[string][]string{"other.example.com": {"inc-two"}})
	a := &recordingAudit{}
	ev := applyForgotten(t, incCtx(), f, a, scopeIncarnation)
	if ev.GetFailed() {
		t.Fatalf("failed: %s", ev.GetMessage())
	}
	if ev.GetChanged() {
		t.Error("changed=true with no members")
	}
	if got := outList(t, ev, "forgotten"); len(got) != 0 {
		t.Errorf("forgotten = %v, want empty", got)
	}
	if len(f.forgetCalls) != 0 || len(a.events) != 0 {
		t.Errorf("forget calls = %v, audit = %d; want none", f.forgetCalls, len(a.events))
	}
}

// TestForgotten_RerunIsIdempotent — the second run of the same step finds no members
// and succeeds without change.
func TestForgotten_RerunIsIdempotent(t *testing.T) {
	f := newFakeForgetter(map[string][]string{"a.example.com": {"inc-one"}, "b.example.com": {"inc-one"}})
	if first := applyForgotten(t, incCtx(), f, &recordingAudit{}, scopeIncarnation); first.GetFailed() || !first.GetChanged() {
		t.Fatalf("first run: failed=%v changed=%v msg=%s", first.GetFailed(), first.GetChanged(), first.GetMessage())
	}
	a := &recordingAudit{}
	second := applyForgotten(t, incCtx(), f, a, scopeIncarnation)
	if second.GetFailed() || second.GetChanged() {
		t.Fatalf("second run: failed=%v changed=%v msg=%s", second.GetFailed(), second.GetChanged(), second.GetMessage())
	}
	if len(a.events) != 0 {
		t.Errorf("second run wrote %d audit events", len(a.events))
	}
}

// TestForgotten_MemberGoneBeforeTheDeleteIsSkipped — a member whose souls row is
// already gone by the time its turn comes is skipped, not a failure.
func TestForgotten_MemberGoneBeforeTheDeleteIsSkipped(t *testing.T) {
	f := newFakeForgetter(map[string][]string{"a.example.com": {"inc-one"}, "b.example.com": {"inc-one"}})
	f.goneBeforeForget = map[string]bool{"a.example.com": true}
	ev := applyForgotten(t, incCtx(), f, &recordingAudit{}, scopeIncarnation)
	if ev.GetFailed() {
		t.Fatalf("failed: %s", ev.GetMessage())
	}
	if got := outList(t, ev, "forgotten"); !reflect.DeepEqual(got, []string{"b.example.com"}) {
		t.Errorf("forgotten = %v, want only b", got)
	}
}

// TestForgotten_NoIncarnationFailsClosed — outside an incarnation's run there are no
// members to read; the registry is not even asked.
func TestForgotten_NoIncarnationFailsClosed(t *testing.T) {
	f := newFakeForgetter(map[string][]string{"a.example.com": {"inc-one"}})
	ev := applyForgotten(t, context.Background(), f, &recordingAudit{}, scopeIncarnation)
	if !ev.GetFailed() {
		t.Fatal("forgotten ran without an incarnation")
	}
	if f.membersCalls != 0 || len(f.forgetCalls) != 0 {
		t.Errorf("registry touched without an incarnation: members=%d forget=%v", f.membersCalls, f.forgetCalls)
	}
}

func TestForgotten_NotConfigured(t *testing.T) {
	ev := applyForgotten(t, incCtx(), nil, nil, scopeIncarnation)
	if !ev.GetFailed() || !strings.Contains(ev.GetMessage(), "not configured") {
		t.Errorf("failed=%v msg=%q, want a not-configured failure", ev.GetFailed(), ev.GetMessage())
	}
}

// TestForgotten_WarningsAreReported — a partial release is a success with warnings,
// as on REST, never a plain success.
func TestForgotten_WarningsAreReported(t *testing.T) {
	f := newFakeForgetter(map[string][]string{"a.example.com": {"inc-one"}})
	f.warnings = map[string][]string{"a.example.com": {"redis purge failed"}}
	a := &recordingAudit{}
	ev := applyForgotten(t, incCtx(), f, a, scopeIncarnation)
	if ev.GetFailed() || !ev.GetChanged() {
		t.Fatalf("failed=%v changed=%v msg=%s", ev.GetFailed(), ev.GetChanged(), ev.GetMessage())
	}
	if got := outList(t, ev, "warnings"); !reflect.DeepEqual(got, []string{"a.example.com: redis purge failed"}) {
		t.Errorf("warnings = %v", got)
	}
	if got := a.events[0].Payload["warnings"]; !reflect.DeepEqual(got, []any{"redis purge failed"}) {
		t.Errorf("audit warnings = %v", got)
	}
}

// TestForgotten_FailureMidwayNamesWhatIsGone — the teardown notice failing for the
// second host fails the step and names the host already forgotten.
func TestForgotten_FailureMidwayNamesWhatIsGone(t *testing.T) {
	f := newFakeForgetter(map[string][]string{"a.example.com": {"inc-one"}, "b.example.com": {"inc-one"}})
	f.forgetErr = map[string]error{"b.example.com": fmt.Errorf("%w: redis down", soulforget.ErrTeardownUnavailable)}
	ev := applyForgotten(t, incCtx(), f, &recordingAudit{}, scopeIncarnation)
	if !ev.GetFailed() {
		t.Fatal("a failed forget did not fail the step")
	}
	if !strings.Contains(ev.GetMessage(), "already forgotten: a.example.com") {
		t.Errorf("message does not name what is gone: %s", ev.GetMessage())
	}
}

// TestForgotten_MembershipChangedBeforeTheDelete — a host bound to another
// incarnation between the read and the delete is refused by the in-transaction
// check, and the step fails rather than reporting it forgotten.
func TestForgotten_MembershipChangedBeforeTheDelete(t *testing.T) {
	f := newFakeForgetter(map[string][]string{"a.example.com": {"inc-one"}})
	f.movedBeforeForget = map[string][]string{"a.example.com": {"inc-one", "inc-two"}}
	ev := applyForgotten(t, incCtx(), f, &recordingAudit{}, scopeIncarnation)
	if !ev.GetFailed() {
		t.Fatal("a host that became shared was reported forgotten")
	}
	if !strings.Contains(ev.GetMessage(), "was NOT forgotten") || !strings.Contains(ev.GetMessage(), "another instance held for it was closed") {
		t.Errorf("message does not say the host stays and which streams the notice closed: %s", ev.GetMessage())
	}
	if got := f.registered(); !reflect.DeepEqual(got, []string{"a.example.com"}) {
		t.Errorf("registry after = %v, want the host still registered", got)
	}
}

// TestForgotten_ScopeIsTheOnlyParam — `scope: incarnation` is required and is the
// only thing the step accepts; in particular no host list, so no author input can
// name a host of another incarnation.
func TestForgotten_ScopeIsTheOnlyParam(t *testing.T) {
	m := coremodsoul.New(newFakeStore())
	bad := map[string]map[string]any{
		"no scope":      {},
		"other scope":   {"scope": "service"},
		"scope not str": {"scope": 1},
		"a host list":   {"scope": "incarnation", "sids": []any{"other.example.com"}},
	}
	for name, params := range bad {
		t.Run(name, func(t *testing.T) {
			rep, err := m.Validate(context.Background(), &pluginv1.ValidateRequest{State: coremodsoul.StateForgotten, Params: mustStruct(t, params)})
			if err != nil {
				t.Fatalf("Validate: %v", err)
			}
			if rep.GetOk() {
				t.Errorf("Validate accepted %v", params)
			}
			f := newFakeForgetter(map[string][]string{"a.example.com": {"inc-one"}})
			if ev := applyForgotten(t, incCtx(), f, &recordingAudit{}, params); !ev.GetFailed() || len(f.forgetCalls) != 0 {
				t.Errorf("Apply with %v: failed=%v forget=%v", params, ev.GetFailed(), f.forgetCalls)
			}
		})
	}
	rep, _ := m.Validate(context.Background(), &pluginv1.ValidateRequest{State: coremodsoul.StateForgotten, Params: mustStruct(t, scopeIncarnation)})
	if !rep.GetOk() {
		t.Errorf("Validate refused scope: incarnation: %v", rep.GetErrors())
	}
}
