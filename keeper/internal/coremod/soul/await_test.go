package soul_test

import (
	"context"
	"errors"
	"reflect"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/souls-guild/soul-stack/keeper/internal/coremod/internaltest"
	coremodsoul "github.com/souls-guild/soul-stack/keeper/internal/coremod/soul"

	pluginv1 "github.com/souls-guild/soul-stack/proto/plugin/gen/go/v1"
)

// fakePresence is a deterministic PresenceChecker for guard-tests of the onboarding
// barrier. online is the set of SIDs that are "already online"; aliveAfter is the
// number of polls after which an SID becomes online (gradual onboarding model).
// err is Redis-failure injection.
type fakePresence struct {
	mu         sync.Mutex
	online     map[string]struct{}
	aliveAfter map[string]int // sid → how many SoulsStreamAlive calls until online
	calls      int
	err        error
}

func newFakePresence() *fakePresence {
	return &fakePresence{online: map[string]struct{}{}, aliveAfter: map[string]int{}}
}

func (f *fakePresence) SoulsStreamAlive(_ context.Context, sids []string) (map[string]struct{}, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return nil, f.err
	}
	f.calls++
	res := map[string]struct{}{}
	for _, sid := range sids {
		if _, ok := f.online[sid]; ok {
			res[sid] = struct{}{}
			continue
		}
		if after, ok := f.aliveAfter[sid]; ok && f.calls >= after {
			res[sid] = struct{}{}
		}
	}
	return res, nil
}

// newAwaitModule creates module with presence-checker and test timeout ceiling.
func newAwaitModule(t *testing.T, fs coremodsoul.Store, p coremodsoul.PresenceChecker, maxTimeout string) *coremodsoul.Module {
	t.Helper()
	return coremodsoul.New(fs).WithPresence(p, func() string { return maxTimeout })
}

func sortedOut(out map[string]any, key string) []string {
	raw, _ := out[key].([]any)
	xs := make([]string, 0, len(raw))
	for _, v := range raw {
		if s, ok := v.(string); ok {
			xs = append(xs, s)
		}
	}
	sort.Strings(xs)
	return xs
}

// TestAwait_WaitsUntilOnline — barrier blocks and succeeds when
// all registered SIDs become online (source: presence-poll).
func TestAwait_WaitsUntilOnline(t *testing.T) {
	fs := newFakeStore()
	p := newFakePresence()
	// h1 online from 2nd poll, h2 from 3rd (gradual onboarding).
	p.aliveAfter["h1.example.com"] = 2
	p.aliveAfter["h2.example.com"] = 3

	m := newAwaitModule(t, fs, p, "")
	stream := internaltest.NewApplyStream()
	err := m.Apply(&pluginv1.ApplyRequest{
		State: "registered",
		Params: mustStruct(t, map[string]any{
			"sid":                 []any{"h1.example.com", "h2.example.com"},
			"coven":               []any{"redis", "prod"},
			"await_online":        true,
			"await_timeout":       "5s",
			"await_poll_interval": "1ms",
		}),
	}, stream)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	ev := stream.Last()
	if ev == nil || ev.Failed {
		t.Fatalf("expected success, got %+v", ev)
	}
	out := ev.Output.AsMap()
	if out["satisfied"] != true {
		t.Errorf("satisfied=%v, want true", out["satisfied"])
	}
	if got := sortedOut(out, "online"); !reflect.DeepEqual(got, []string{"h1.example.com", "h2.example.com"}) {
		t.Errorf("online=%v, want both hosts", got)
	}
	if got := sortedOut(out, "pending"); len(got) != 0 {
		t.Errorf("pending=%v, want empty", got)
	}
	if p.calls < 3 {
		t.Errorf("expected at least 3 presence polls (gradual onboarding), got %d", p.calls)
	}
}

// TestAwait_B1Timeout_Failed — online < min_count at timeout → step failed
// (B1-strict fail-stop). output carries pending for diagnostics.
func TestAwait_B1Timeout_Failed(t *testing.T) {
	fs := newFakeStore()
	p := newFakePresence()
	p.online["h1.example.com"] = struct{}{} // only h1 came up, h2 never

	m := newAwaitModule(t, fs, p, "")
	stream := internaltest.NewApplyStream()
	if err := m.Apply(&pluginv1.ApplyRequest{
		State: "registered",
		Params: mustStruct(t, map[string]any{
			"sid":                 []any{"h1.example.com", "h2.example.com"},
			"coven":               []any{"redis"},
			"await_online":        true,
			"await_timeout":       "30ms",
			"await_poll_interval": "1ms",
		}),
	}, stream); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	ev := stream.Last()
	if !ev.Failed {
		t.Fatalf("expected failed=true on B1 timeout, got %+v", ev)
	}
	// Diagnostics in message: how many online / how many waited.
	if ev.Message == "" {
		t.Error("expected diagnostic message on B1 timeout")
	}
}

// TestAwait_MinCountSatisfied_OK — quorum `await_min_count` reached before
// all hosts online → success, don't wait for rest.
func TestAwait_MinCountSatisfied_OK(t *testing.T) {
	fs := newFakeStore()
	p := newFakePresence()
	p.online["h1.example.com"] = struct{}{}
	p.online["h2.example.com"] = struct{}{} // h3 won't come up — not needed

	m := newAwaitModule(t, fs, p, "")
	stream := internaltest.NewApplyStream()
	if err := m.Apply(&pluginv1.ApplyRequest{
		State: "registered",
		Params: mustStruct(t, map[string]any{
			"sid":                 []any{"h1.example.com", "h2.example.com", "h3.example.com"},
			"coven":               []any{"redis"},
			"await_online":        true,
			"await_timeout":       "5s",
			"await_min_count":     2,
			"await_poll_interval": "1ms",
		}),
	}, stream); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	ev := stream.Last()
	if ev.Failed {
		t.Fatalf("expected success at min_count=2, got %+v", ev)
	}
	out := ev.Output.AsMap()
	if out["satisfied"] != true {
		t.Errorf("satisfied=%v, want true", out["satisfied"])
	}
	if got := sortedOut(out, "online"); len(got) < 2 {
		t.Errorf("online=%v, want >= 2", got)
	}
}

// TestAwait_SourceIsLease_NotPGStatus — presence decided by Redis-lease-checker, NOT
// PG souls.status. SID exists in Store with status=connected, but lease does NOT
// see it → barrier does not count host online → B1 timeout.
func TestAwait_SourceIsLease_NotPGStatus(t *testing.T) {
	fs := newFakeStore()
	// PG says "connected", but presence-checker (lease) won't return it.
	p := newFakePresence() // online empty — lease sees nothing

	m := newAwaitModule(t, fs, p, "")
	stream := internaltest.NewApplyStream()
	if err := m.Apply(&pluginv1.ApplyRequest{
		State: "registered",
		Params: mustStruct(t, map[string]any{
			"sid":                 "h1.example.com",
			"coven":               []any{"redis"},
			"await_online":        true,
			"await_timeout":       "20ms",
			"await_poll_interval": "1ms",
		}),
	}, stream); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if !stream.Last().Failed {
		t.Fatal("expected failed: lease (not PG status) is online source; lease saw nothing")
	}
	if p.calls == 0 {
		t.Error("expected presence-checker (lease) polled — it is source of truth")
	}
}

// TestAwait_TimeoutCeiling_Failed — await_timeout > keeper.yml ceiling
// (max_await_timeout) → step failed (fail-closed DoS-guard, NOT silent truncation).
func TestAwait_TimeoutCeiling_Failed(t *testing.T) {
	fs := newFakeStore()
	p := newFakePresence()
	p.online["h1.example.com"] = struct{}{} // host online — failure only from ceiling

	m := newAwaitModule(t, fs, p, "1s") // ceiling 1s
	stream := internaltest.NewApplyStream()
	if err := m.Apply(&pluginv1.ApplyRequest{
		State: "registered",
		Params: mustStruct(t, map[string]any{
			"sid":           "h1.example.com",
			"coven":         []any{"redis"},
			"await_online":  true,
			"await_timeout": "2h", // exceeds ceiling 1s
		}),
	}, stream); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	ev := stream.Last()
	if !ev.Failed {
		t.Fatalf("expected failed: await_timeout exceeds ceiling, got %+v", ev)
	}
	if p.calls != 0 {
		t.Error("ceiling check must reject before polling")
	}
}

// TestAwait_RequiredTimeout — await_online: true without await_timeout → validation
// error (barrier must not hang forever).
func TestAwait_RequiredTimeout(t *testing.T) {
	fs := newFakeStore()
	p := newFakePresence()
	m := newAwaitModule(t, fs, p, "")
	stream := internaltest.NewApplyStream()
	if err := m.Apply(&pluginv1.ApplyRequest{
		State: "registered",
		Params: mustStruct(t, map[string]any{
			"sid":          "h1.example.com",
			"coven":        []any{"redis"},
			"await_online": true,
		}),
	}, stream); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if !stream.Last().Failed {
		t.Fatal("expected failed: await_online requires await_timeout")
	}
}

// TestAwait_NoAwait_RegistersWithoutBlocking — without await_online step behaves
// as before ADR-061 (registration without barrier), presence-checker not called.
func TestAwait_NoAwait_RegistersWithoutBlocking(t *testing.T) {
	fs := newFakeStore()
	p := newFakePresence()
	m := newAwaitModule(t, fs, p, "")
	stream := internaltest.NewApplyStream()
	if err := m.Apply(&pluginv1.ApplyRequest{
		State: "registered",
		Params: mustStruct(t, map[string]any{
			"sid":   []any{"h1.example.com", "h2.example.com"},
			"coven": []any{"redis"},
		}),
	}, stream); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if stream.Last().Failed {
		t.Fatalf("expected success without await, got %+v", stream.Last())
	}
	if p.calls != 0 {
		t.Errorf("presence must not be polled without await_online, got %d calls", p.calls)
	}
	// Both SIDs registered (list-form).
	if fs.insertCalls != 2 {
		t.Errorf("expected 2 inserts for 2-SID list, got %d", fs.insertCalls)
	}
}

// TestAwait_ListSID_RegistersAll — sid list-form registers all hosts with
// shared coven set, barrier aggregates presence across all.
func TestAwait_ListSID_RegistersAll(t *testing.T) {
	fs := newFakeStore()
	p := newFakePresence()
	p.online["a.example.com"] = struct{}{}
	p.online["b.example.com"] = struct{}{}
	p.online["c.example.com"] = struct{}{}

	m := newAwaitModule(t, fs, p, "")
	stream := internaltest.NewApplyStream()
	if err := m.Apply(&pluginv1.ApplyRequest{
		State: "registered",
		Params: mustStruct(t, map[string]any{
			"sid":                 []any{"a.example.com", "b.example.com", "c.example.com"},
			"coven":               []any{"redis", "shard-1"},
			"await_online":        true,
			"await_timeout":       "5s",
			"await_poll_interval": "1ms",
		}),
	}, stream); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	ev := stream.Last()
	if ev.Failed {
		t.Fatalf("expected success, got %+v", ev)
	}
	if fs.insertCalls != 3 {
		t.Errorf("expected 3 inserts, got %d", fs.insertCalls)
	}
	// coven applied to all (last UpdateCoven — shared set).
	got := append([]string(nil), fs.lastCoven...)
	sort.Strings(got)
	if !reflect.DeepEqual(got, []string{"redis", "shard-1"}) {
		t.Errorf("coven=%v, want [redis shard-1] applied to all", got)
	}
}

// TestAwait_PresenceError_Failed — Redis-check error during barrier →
// step failed (presence-source unavailable, B1-strict cannot confirm
// quorum → fail, not silent success).
func TestAwait_PresenceError_Failed(t *testing.T) {
	fs := newFakeStore()
	p := newFakePresence()
	p.err = errors.New("redis down")

	m := newAwaitModule(t, fs, p, "")
	stream := internaltest.NewApplyStream()
	if err := m.Apply(&pluginv1.ApplyRequest{
		State: "registered",
		Params: mustStruct(t, map[string]any{
			"sid":                 "h1.example.com",
			"coven":               []any{"redis"},
			"await_online":        true,
			"await_timeout":       "30ms",
			"await_poll_interval": "1ms",
		}),
	}, stream); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if !stream.Last().Failed {
		t.Fatal("expected failed on persistent presence error")
	}
}

// TestAwait_NoChecker_Failed — await_online requested, but presence-checker not
// configured (nil) → failed (barrier cannot work without presence source;
// silent success not allowed).
func TestAwait_NoChecker_Failed(t *testing.T) {
	fs := newFakeStore()
	m := coremodsoul.New(fs) // without WithPresence
	stream := internaltest.NewApplyStream()
	if err := m.Apply(&pluginv1.ApplyRequest{
		State: "registered",
		Params: mustStruct(t, map[string]any{
			"sid":           "h1.example.com",
			"coven":         []any{"redis"},
			"await_online":  true,
			"await_timeout": "5s",
		}),
	}, stream); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if !stream.Last().Failed {
		t.Fatal("expected failed: await_online without presence-checker configured")
	}
}

// TestAwait_RefreshSoulprint_WaitsForFacts — with refresh_soulprint+await_online
// barrier requires FIRST typed soulprint, not just presence-lease: SID online
// immediately, but facts arrive later → barrier waits for facts, success after arrival.
// Guard on 7-th wall of live-create (next Passage render reads
// soulprint.self.* — facts must be in PG before barrier pass).
func TestAwait_RefreshSoulprint_WaitsForFacts(t *testing.T) {
	fs := newFakeStore()
	p := newFakePresence()
	p.online["h1.example.com"] = struct{}{}
	fs.factsAfter["h1.example.com"] = 3 // facts "arrive" by 3rd facts-poll

	m := newAwaitModule(t, fs, p, "")
	stream := internaltest.NewApplyStream()
	if err := m.Apply(&pluginv1.ApplyRequest{
		State: "registered",
		Params: mustStruct(t, map[string]any{
			"sid":                 "h1.example.com",
			"coven":               []any{"redis"},
			"await_online":        true,
			"await_timeout":       "5s",
			"await_poll_interval": "1ms",
			"refresh_soulprint":   true,
		}),
	}, stream); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	ev := stream.Last()
	if ev == nil || ev.Failed {
		t.Fatalf("expected success after facts arrive, got %+v", ev)
	}
	if fs.factsCalls < 3 {
		t.Errorf("barrier must poll facts until present (>= 3 polls), got %d", fs.factsCalls)
	}
	if out := ev.Output.AsMap(); out["satisfied"] != true {
		t.Errorf("satisfied=%v, want true", out["satisfied"])
	}
}

// TestAwait_RefreshSoulprint_FactsPresent_ImmediatePass — facts already in PG
// (rerun / create_from_souls path: hosts onboarded earlier) → barrier passes
// on first poll, zero wait.
func TestAwait_RefreshSoulprint_FactsPresent_ImmediatePass(t *testing.T) {
	fs := newFakeStore()
	p := newFakePresence()
	p.online["h1.example.com"] = struct{}{}
	fs.factsBySID["h1.example.com"] = struct{}{}

	m := newAwaitModule(t, fs, p, "")
	stream := internaltest.NewApplyStream()
	if err := m.Apply(&pluginv1.ApplyRequest{
		State: "registered",
		Params: mustStruct(t, map[string]any{
			"sid":                 "h1.example.com",
			"coven":               []any{"redis"},
			"await_online":        true,
			"await_timeout":       "5s",
			"await_poll_interval": "500ms",
			"refresh_soulprint":   true,
		}),
	}, stream); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	ev := stream.Last()
	if ev.Failed {
		t.Fatalf("expected immediate success, got %+v", ev)
	}
	if p.calls != 1 || fs.factsCalls != 1 {
		t.Errorf("expected single poll (presence=%d, facts=%d), want 1/1", p.calls, fs.factsCalls)
	}
}

// TestAwait_RefreshSoulprint_FactlessTimeout_Failed — SID online, but typed
// facts not written by timeout → failed with diagnostics "online but
// factless" (distinguishable from "not online").
func TestAwait_RefreshSoulprint_FactlessTimeout_Failed(t *testing.T) {
	fs := newFakeStore()
	p := newFakePresence()
	p.online["h1.example.com"] = struct{}{} // online, facts won't appear

	m := newAwaitModule(t, fs, p, "")
	stream := internaltest.NewApplyStream()
	if err := m.Apply(&pluginv1.ApplyRequest{
		State: "registered",
		Params: mustStruct(t, map[string]any{
			"sid":                 "h1.example.com",
			"coven":               []any{"redis"},
			"await_online":        true,
			"await_timeout":       "30ms",
			"await_poll_interval": "1ms",
			"refresh_soulprint":   true,
		}),
	}, stream); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	ev := stream.Last()
	if !ev.Failed {
		t.Fatalf("expected failed: online SID without facts must not satisfy barrier, got %+v", ev)
	}
	if !strings.Contains(ev.Message, "online but factless") || !strings.Contains(ev.Message, "h1.example.com") {
		t.Errorf("message must name factless SIDs distinctly, got %q", ev.Message)
	}
}

// TestAwait_RefreshSoulprint_TwoClasses_Diagnostics — timeout on both shortfall
// classes: h1 online+factless, h2 not online → message carries both lists separately.
func TestAwait_RefreshSoulprint_TwoClasses_Diagnostics(t *testing.T) {
	fs := newFakeStore()
	p := newFakePresence()
	p.online["h1.example.com"] = struct{}{} // h2 never comes up

	m := newAwaitModule(t, fs, p, "")
	stream := internaltest.NewApplyStream()
	if err := m.Apply(&pluginv1.ApplyRequest{
		State: "registered",
		Params: mustStruct(t, map[string]any{
			"sid":                 []any{"h1.example.com", "h2.example.com"},
			"coven":               []any{"redis"},
			"await_online":        true,
			"await_timeout":       "30ms",
			"await_poll_interval": "1ms",
			"refresh_soulprint":   true,
		}),
	}, stream); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	ev := stream.Last()
	if !ev.Failed {
		t.Fatalf("expected failed, got %+v", ev)
	}
	if !strings.Contains(ev.Message, "not online") || !strings.Contains(ev.Message, "h2.example.com") {
		t.Errorf("message must carry not-online class with SIDs, got %q", ev.Message)
	}
	if !strings.Contains(ev.Message, "online but factless") || !strings.Contains(ev.Message, "h1.example.com") {
		t.Errorf("message must carry factless class with SIDs, got %q", ev.Message)
	}
}

// TestAwait_BackCompat_NoRefresh_FactsNotChecked — await_online WITHOUT
// refresh_soulprint: old behavior (presence only), facts not polled
// and their absence does not block barrier pass.
func TestAwait_BackCompat_NoRefresh_FactsNotChecked(t *testing.T) {
	fs := newFakeStore()
	p := newFakePresence()
	p.online["h1.example.com"] = struct{}{} // facts absent

	m := newAwaitModule(t, fs, p, "")
	stream := internaltest.NewApplyStream()
	if err := m.Apply(&pluginv1.ApplyRequest{
		State: "registered",
		Params: mustStruct(t, map[string]any{
			"sid":                 "h1.example.com",
			"coven":               []any{"redis"},
			"await_online":        true,
			"await_timeout":       "5s",
			"await_poll_interval": "1ms",
		}),
	}, stream); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if stream.Last().Failed {
		t.Fatalf("expected success without refresh_soulprint (presence-only barrier), got %+v", stream.Last())
	}
	if fs.factsCalls != 0 {
		t.Errorf("facts must not be polled without refresh_soulprint, got %d calls", fs.factsCalls)
	}
}

// TestAwait_RefreshWithoutAwait_NoBarrier — refresh_soulprint: true WITHOUT
// await_online: no barrier at all (Stratify/re-resolve flag function does not
// require facts-wait by itself).
func TestAwait_RefreshWithoutAwait_NoBarrier(t *testing.T) {
	fs := newFakeStore()
	p := newFakePresence()

	m := newAwaitModule(t, fs, p, "")
	stream := internaltest.NewApplyStream()
	if err := m.Apply(&pluginv1.ApplyRequest{
		State: "registered",
		Params: mustStruct(t, map[string]any{
			"sid":               "h1.example.com",
			"coven":             []any{"redis"},
			"refresh_soulprint": true,
		}),
	}, stream); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if stream.Last().Failed {
		t.Fatalf("expected success, got %+v", stream.Last())
	}
	if p.calls != 0 || fs.factsCalls != 0 {
		t.Errorf("no barrier requested: presence=%d facts=%d polls, want 0/0", p.calls, fs.factsCalls)
	}
}

// TestAwait_RefreshSoulprint_FactsError_Failed — persistent facts-read error
// (PG) during barrier → failed (B1-strict does not blindly confirm quorum).
func TestAwait_RefreshSoulprint_FactsError_Failed(t *testing.T) {
	fs := newFakeStore()
	fs.factsErr = errors.New("pg down")
	p := newFakePresence()
	p.online["h1.example.com"] = struct{}{}

	m := newAwaitModule(t, fs, p, "")
	stream := internaltest.NewApplyStream()
	if err := m.Apply(&pluginv1.ApplyRequest{
		State: "registered",
		Params: mustStruct(t, map[string]any{
			"sid":                 "h1.example.com",
			"coven":               []any{"redis"},
			"await_online":        true,
			"await_timeout":       "30ms",
			"await_poll_interval": "1ms",
			"refresh_soulprint":   true,
		}),
	}, stream); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if !stream.Last().Failed {
		t.Fatal("expected failed on persistent facts-read error")
	}
}

// TestAwait_MinCountTooHigh_Validation — await_min_count > SID count →
// validation error (unreachable quorum).
func TestAwait_MinCountTooHigh_Validation(t *testing.T) {
	fs := newFakeStore()
	p := newFakePresence()
	m := newAwaitModule(t, fs, p, "")
	stream := internaltest.NewApplyStream()
	if err := m.Apply(&pluginv1.ApplyRequest{
		State: "registered",
		Params: mustStruct(t, map[string]any{
			"sid":             []any{"h1.example.com", "h2.example.com"},
			"coven":           []any{"redis"},
			"await_online":    true,
			"await_timeout":   "5s",
			"await_min_count": 5, // more than 2 SIDs
		}),
	}, stream); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if !stream.Last().Failed {
		t.Fatal("expected failed: await_min_count exceeds number of SIDs")
	}
}
