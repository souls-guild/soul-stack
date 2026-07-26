package scenario

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/souls-guild/soul-stack/keeper/internal/render"
	"github.com/souls-guild/soul-stack/shared/config"
)

// Required-set derivation from a rendered plan (ADR-0076(i)) — the input half of
// the per-host gate. Each case fixes one attribution rule; getting these wrong
// either rejects a run that would have worked or lets the silent-ignore through.

func TestRequiredSoulCapabilities_PerHostByTarget(t *testing.T) {
	tasks := []*render.RenderedTask{
		{Index: 0, Module: "core.pkg.installed"},
		{Index: 1, Module: "core.service.running"},
	}
	plans := []render.DispatchPlan{
		{TaskIndex: 0, TargetSIDs: []string{"host-a", "host-b"}},
		{TaskIndex: 1, TargetSIDs: []string{"host-b"}},
	}

	got := requiredSoulCapabilities(tasks, plans)
	want := map[string][]string{
		"host-a": {"module:core.pkg"},
		"host-b": {"module:core.pkg", "module:core.service"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("required = %v, want %v (a host is only asked for what targets IT)", got, want)
	}
}

// A module capability drops the state suffix: the registry key is the module,
// states are dispatched inside its implementation.
func TestRequiredSoulCapabilities_StateSuffixDropped(t *testing.T) {
	got := requiredSoulCapabilities(
		[]*render.RenderedTask{{Index: 0, Module: "core.file.rendered"}},
		[]render.DispatchPlan{{TaskIndex: 0, TargetSIDs: []string{"host-a"}}},
	)
	if want := []string{"module:core.file"}; !reflect.DeepEqual(got["host-a"], want) {
		t.Fatalf("required = %v, want %v", got["host-a"], want)
	}
}

// Soul-side DSL features are required from the host that runs the task carrying
// them — keeper threads them through as data, the Soul is what enforces them.
func TestRequiredSoulCapabilities_SoulSideFeatures(t *testing.T) {
	tasks := []*render.RenderedTask{
		{Index: 0, Module: "core.exec.run", When: "register.probe.changed"},
		{Index: 1, Module: "core.exec.run", ChangedWhen: "register.self.rc == 0"},
		{Index: 2, Module: "core.exec.run", FailedWhen: "register.self.rc != 0"},
		{Index: 3, Module: "core.exec.run", RetryCount: 3},
		{Index: 4, Module: "core.exec.run", Until: "register.self.rc == 0"},
		{Index: 5, Module: "core.exec.run", RetryCount: 1},
	}
	plans := []render.DispatchPlan{
		{TaskIndex: 0, TargetSIDs: []string{"when"}},
		{TaskIndex: 1, TargetSIDs: []string{"changed-when"}},
		{TaskIndex: 2, TargetSIDs: []string{"failed-when"}},
		{TaskIndex: 3, TargetSIDs: []string{"retry"}},
		{TaskIndex: 4, TargetSIDs: []string{"until"}},
		{TaskIndex: 5, TargetSIDs: []string{"single-attempt"}},
	}

	got := requiredSoulCapabilities(tasks, plans)
	for _, sid := range []string{"when", "changed-when", "failed-when"} {
		if !hasCap(got[sid], config.CapabilityFlowControl) {
			t.Errorf("host %q: required = %v, want flow_control", sid, got[sid])
		}
	}
	for _, sid := range []string{"retry", "until"} {
		if !hasCap(got[sid], config.CapabilityRetry) {
			t.Errorf("host %q: required = %v, want retry", sid, got[sid])
		}
	}
	// retry:{count: 1} is one attempt — the same thing a binary without the loop
	// does, so it must not be required.
	if hasCap(got["single-attempt"], config.CapabilityRetry) {
		t.Errorf("single-attempt: required = %v, want no retry (count 1 == one attempt)", got["single-attempt"])
	}
}

// A plugin module is deliberately NOT gated: core.module.installed can install it
// mid-run (ADR-065), long after the host announced its set at connect time.
func TestRequiredSoulCapabilities_PluginModuleNotGated(t *testing.T) {
	got := requiredSoulCapabilities(
		[]*render.RenderedTask{
			{Index: 0, Module: "core.module.installed"},
			{Index: 1, Module: "acme.widget.present"},
		},
		[]render.DispatchPlan{
			{TaskIndex: 0, TargetSIDs: []string{"host-a"}},
			{TaskIndex: 1, TargetSIDs: []string{"host-a"}},
		},
	)
	want := []string{"module:core.module"}
	if !reflect.DeepEqual(got["host-a"], want) {
		t.Fatalf("required = %v, want %v (the plugin module must not be gated)", got["host-a"], want)
	}
}

// Keeper-side tasks run locally against keeper's own registry — a Soul is never
// asked for them. A targetless plan (statically-skipped task / future-Passage
// placeholder) requires nothing either.
func TestRequiredSoulCapabilities_KeeperAndTargetlessSkipped(t *testing.T) {
	got := requiredSoulCapabilities(
		[]*render.RenderedTask{
			{Index: 0, Module: "core.cloud.created"},
			{Index: 1, Module: "core.pkg.installed", When: "false"},
			{Index: 2, Module: "core.git.cloned", Passage: 1},
		},
		[]render.DispatchPlan{
			{TaskIndex: 0, TargetSIDs: []string{"keeper"}, Keeper: true},
			{TaskIndex: 1},
			{TaskIndex: 2},
		},
	)
	if len(got) != 0 {
		t.Fatalf("required = %v, want empty", got)
	}
}

func TestWithRunCapability_AddsToEveryHost(t *testing.T) {
	base := map[string][]string{"host-a": {"module:core.pkg"}}
	got := withRunCapability(base, []string{"host-a", "host-b"}, config.CapabilityDryRun)
	want := map[string][]string{
		"host-a": {"dry_run", "module:core.pkg"},
		"host-b": {"dry_run"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("required = %v, want %v", got, want)
	}
	if !reflect.DeepEqual(base, map[string][]string{"host-a": {"module:core.pkg"}}) {
		t.Fatalf("input map was mutated: %v", base)
	}
}

// --- gate (ADR-0076(i)) ---

func TestGateSoulCapabilities_NilCheckerFailsClosed(t *testing.T) {
	r := &Runner{}
	err := r.gateSoulCapabilities(context.Background(), "redis-prod", "converge",
		map[string][]string{"host-a": {"module:core.pkg"}})
	if err == nil {
		t.Fatal("nil checker with a non-empty requirement must reject (no presence source to confirm support)")
	}
	if !strings.Contains(err.Error(), "fail-closed") {
		t.Fatalf("error = %v, want a fail-closed explanation", err)
	}
}

// Nothing required (an all-keeper scenario) → nothing to confirm, so a missing
// checker is not a reason to reject.
func TestGateSoulCapabilities_NilCheckerNoRequirementPasses(t *testing.T) {
	r := &Runner{}
	if err := r.gateSoulCapabilities(context.Background(), "redis-prod", "converge", nil); err != nil {
		t.Fatalf("empty requirement must pass: %v", err)
	}
}

func TestGateSoulCapabilities_CheckerErrorFailsClosed(t *testing.T) {
	r := &Runner{soulCap: stubSoulCap{err: errors.New("redis down")}}
	err := r.gateSoulCapabilities(context.Background(), "redis-prod", "converge",
		map[string][]string{"host-a": {"module:core.pkg"}})
	if err == nil || !strings.Contains(err.Error(), "redis down") {
		t.Fatalf("error = %v, want the Redis failure to reject the run", err)
	}
}

// The message names every host that needs the binary bumped, and the capability
// it is missing — one upgrade round, not one host discovered per retry.
func TestGateSoulCapabilities_LackingHostsNamed(t *testing.T) {
	r := &Runner{soulCap: stubSoulCap{lacking: []string{"host-b", "host-c"}}}
	err := r.gateSoulCapabilities(context.Background(), "redis-prod", "converge",
		map[string][]string{
			"host-a": {"module:core.pkg"},
			"host-b": {"module:core.pkg"},
			"host-c": {"module:core.line", config.CapabilityRetry},
		})
	if err == nil {
		t.Fatal("hosts lacking a required capability must reject the run")
	}
	msg := err.Error()
	for _, want := range []string{"host-b", "host-c", "update the soul binary", "module:core.pkg", "module:core.line", "retry"} {
		if !strings.Contains(msg, want) {
			t.Errorf("error %q does not mention %q", msg, want)
		}
	}
	if strings.Contains(msg, "host-a:") {
		t.Errorf("error %q blames host-a, which announced everything asked of it", msg)
	}
}

func TestGateSoulCapabilities_AllAnnouncedPasses(t *testing.T) {
	r := &Runner{soulCap: stubSoulCap{}}
	if err := r.gateSoulCapabilities(context.Background(), "redis-prod", "converge",
		map[string][]string{"host-a": {"module:core.pkg", config.CapabilityFlowControl}}); err != nil {
		t.Fatalf("a fleet announcing everything must pass: %v", err)
	}
}

// The gate asks about a host ONLY for the capabilities that host's own tasks
// need: a per-capability query carrying the whole roster would reject a host over
// a module it was never targeted with.
func TestGateSoulCapabilities_QueriesOnlyTargetedHosts(t *testing.T) {
	r := &Runner{soulCap: stubSoulCap{lackingByCap: map[string][]string{"module:core.line": {"host-a"}}}}
	err := r.gateSoulCapabilities(context.Background(), "redis-prod", "converge",
		map[string][]string{
			"host-a": {"module:core.pkg"},
			"host-b": {"module:core.line"},
		})
	if err != nil {
		t.Fatalf("host-a lacks core.line but is never targeted with it: %v", err)
	}
}

func hasCap(haystack []string, needle string) bool {
	for _, s := range haystack {
		if s == needle {
			return true
		}
	}
	return false
}

// stubSoulCap — a controllable [SoulCapabilityChecker] for tests (used by the
// integration guards too, hence the untagged file). lacking — SIDs that announce
// NOTHING (default nil → every host supports everything asked of it, like a
// single-version beta fleet); lackingByCap — per-capability override for a fleet
// where only one capability is missing. err — simulates a Redis failure.
type stubSoulCap struct {
	lacking      []string
	lackingByCap map[string][]string
	err          error
}

func (s stubSoulCap) SoulsLackingCapability(_ context.Context, sids []string, capability string) ([]string, error) {
	if s.err != nil {
		return nil, s.err
	}
	lacking := s.lacking
	if byCap, ok := s.lackingByCap[capability]; ok {
		lacking = byCap
	}
	// Intersect with the queried SIDs — the real checker only ever reports back
	// hosts it was asked about.
	asked := make(map[string]struct{}, len(sids))
	for _, sid := range sids {
		asked[sid] = struct{}{}
	}
	var out []string
	for _, sid := range lacking {
		if _, ok := asked[sid]; ok {
			out = append(out, sid)
		}
	}
	return out, nil
}
