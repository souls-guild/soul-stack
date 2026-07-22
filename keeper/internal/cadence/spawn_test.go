package cadence

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/souls-guild/soul-stack/keeper/internal/voyage"
)

// fakeScenarioResolver / fakeCommandResolver — stubs of resolvers for the cadence
// interfaces. Capture arguments, return a programmed snapshot/err.
type fakeScenarioResolver struct {
	gotIncs    []string
	gotService string
	gotCoven   string
	out        []string
	err        error
}

func (f *fakeScenarioResolver) ResolveIncarnations(_ context.Context, incs []string, service, coven string) ([]string, error) {
	f.gotIncs, f.gotService, f.gotCoven = incs, service, coven
	return f.out, f.err
}

type fakeCommandResolver struct {
	gotSIDs   []string
	gotCovens []string
	gotWhere  string
	gotAlive  bool
	out       []string
	err       error
}

func (f *fakeCommandResolver) ResolveSIDs(_ context.Context, sids, covens []string, where string, alive bool) ([]string, error) {
	f.gotSIDs, f.gotCovens, f.gotWhere, f.gotAlive = sids, covens, where, alive
	return f.out, f.err
}

func TestResolveScope_Scenario(t *testing.T) {
	t.Parallel()
	c := intervalCadence()
	c.Kind = KindScenario
	c.ScenarioName = strptr("converge")
	c.Target = json.RawMessage(`{"incarnations":["x"],"service":"redis","coven":["prod-eu","dev"]}`)
	sr := &fakeScenarioResolver{out: []string{"x", "y"}}
	cr := &fakeCommandResolver{}

	got, err := ResolveScope(context.Background(), c, sr, cr)
	if err != nil {
		t.Fatalf("ResolveScope: %v", err)
	}
	if len(got) != 2 {
		t.Errorf("scope = %v, want 2", got)
	}
	if sr.gotService != "redis" || sr.gotCoven != "prod-eu" { // first coven
		t.Errorf("scenario filter: service=%q coven=%q", sr.gotService, sr.gotCoven)
	}
}

func TestResolveScope_Command(t *testing.T) {
	t.Parallel()
	c := cronCadence()
	c.Kind = KindCommand
	c.Module = strptr("core.cmd.shell")
	c.RequireAlive = boolptr(true)
	c.Target = json.RawMessage(`{"sids":["a.example"],"coven":["prod"],"where":"x"}`)
	sr := &fakeScenarioResolver{}
	cr := &fakeCommandResolver{out: []string{"a.example"}}

	got, err := ResolveScope(context.Background(), c, sr, cr)
	if err != nil {
		t.Fatalf("ResolveScope: %v", err)
	}
	if len(got) != 1 {
		t.Errorf("scope = %v, want 1", got)
	}
	if !cr.gotAlive {
		t.Error("require_alive was not passed to the command resolver")
	}
	if cr.gotWhere != "x" || len(cr.gotCovens) != 1 {
		t.Errorf("command filter: where=%q covens=%v", cr.gotWhere, cr.gotCovens)
	}
}

func TestResolveScope_PropagatesResolverError(t *testing.T) {
	t.Parallel()
	c := intervalCadence()
	c.Target = json.RawMessage(`{"service":"redis"}`)
	want := errors.New("pg down")
	got, err := ResolveScope(context.Background(), c, &fakeScenarioResolver{err: want}, &fakeCommandResolver{})
	if !errors.Is(err, want) {
		t.Errorf("err = %v, want %v (scope %v)", err, want, got)
	}
}

func TestBuildVoyage_ScenarioBarrier(t *testing.T) {
	t.Parallel()
	c := intervalCadence()
	c.Kind = KindScenario
	c.ScenarioName = strptr("converge")
	c.BatchSize = intptr(2)
	resolved := []string{"i1", "i2", "i3"}

	v, targets := BuildVoyage(c, "VOY01", resolved)

	if v.VoyageID != "VOY01" {
		t.Errorf("voyage_id = %q", v.VoyageID)
	}
	if v.CadenceID == nil || *v.CadenceID != c.ID {
		t.Errorf("cadence_id back-link = %v, want %q", v.CadenceID, c.ID)
	}
	if v.StartedByAID != c.CreatedByAID {
		t.Errorf("started_by_aid = %q, want %q (Cadence creator)", v.StartedByAID, c.CreatedByAID)
	}
	if v.Kind != voyage.KindScenario {
		t.Errorf("kind = %q", v.Kind)
	}
	if v.TotalBatches != 2 { // ceil(3/2)
		t.Errorf("total_batches = %d, want 2", v.TotalBatches)
	}
	if len(targets) != 3 {
		t.Fatalf("targets = %d, want 3", len(targets))
	}
	// batch_index: i1,i2 → Leg 0; i3 → Leg 1.
	wantIdx := []int{0, 0, 1}
	for i, tg := range targets {
		if tg.BatchIndex != wantIdx[i] {
			t.Errorf("target[%d].batch_index = %d, want %d", i, tg.BatchIndex, wantIdx[i])
		}
		if tg.TargetKind != voyage.TargetKindIncarnation {
			t.Errorf("target[%d].kind = %q", i, tg.TargetKind)
		}
		if tg.Status != voyage.TargetStatusAwaiting {
			t.Errorf("target[%d].status = %q", i, tg.Status)
		}
	}
	// target_resolved — a JSON array of names.
	var names []string
	if err := json.Unmarshal(v.TargetResolved, &names); err != nil {
		t.Fatalf("target_resolved unmarshal: %v", err)
	}
	if len(names) != 3 {
		t.Errorf("target_resolved = %v", names)
	}
}

func TestBuildVoyage_CommandWindow(t *testing.T) {
	t.Parallel()
	c := cronCadence()
	c.Kind = KindCommand
	c.Module = strptr("core.cmd.shell")
	wm := BatchModeWindow
	c.BatchMode = &wm
	c.Concurrency = intptr(10)
	resolved := []string{"a", "b", "c"}

	v, targets := BuildVoyage(c, "VOY02", resolved)

	if v.BatchMode == nil || *v.BatchMode != voyage.BatchModeWindow {
		t.Errorf("batch_mode = %v, want window", v.BatchMode)
	}
	if v.BatchSize != nil {
		t.Errorf("window: batch_size must be nil, got %v", v.BatchSize)
	}
	if v.TotalBatches != 1 { // window → one wave
		t.Errorf("total_batches = %d, want 1", v.TotalBatches)
	}
	for i, tg := range targets {
		if tg.BatchIndex != 0 { // window → all in Leg 0
			t.Errorf("target[%d].batch_index = %d, want 0", i, tg.BatchIndex)
		}
		if tg.TargetKind != voyage.TargetKindSID {
			t.Errorf("target[%d].kind = %q", i, tg.TargetKind)
		}
	}
}

func TestBuildVoyage_BatchPercent(t *testing.T) {
	t.Parallel()
	c := intervalCadence()
	c.BatchSize = nil
	c.BatchPercent = intptr(50) // 50% of 4 = 2
	resolved := []string{"a", "b", "c", "d"}

	v, _ := BuildVoyage(c, "VOY03", resolved)
	if v.BatchSize == nil || *v.BatchSize != 2 {
		t.Errorf("effective batch_size = %v, want 2 (50%% of 4)", v.BatchSize)
	}
	if v.TotalBatches != 2 { // ceil(4/2)
		t.Errorf("total_batches = %d, want 2", v.TotalBatches)
	}
}

// TestBuildVoyage_FailThresholdPercent — key late-binding case (ADR-043 amendment
// 2026-06-09, Cadence-recipe S3): the Cadence recipe's fail_threshold_percent resolves
// to an ABSOLUTE voyage.FailThreshold at SPAWN-scope (len(resolved)), not at
// create-time. The spawned Voyage doesn't carry a percent column — it gets the
// already-absolute threshold. ceil(scope*pct/100): 25% of 10 = 3.
func TestBuildVoyage_FailThresholdPercent(t *testing.T) {
	t.Parallel()
	c := intervalCadence()
	c.FailThreshold = nil
	c.FailThresholdPercent = intptr(25) // 25% of spawn-scope
	resolved := []string{"i1", "i2", "i3", "i4", "i5", "i6", "i7", "i8", "i9", "i10"}

	v, _ := BuildVoyage(c, "VOY04", resolved)
	if v.FailThreshold == nil || *v.FailThreshold != 3 { // ceil(10*25/100)=3
		t.Errorf("effective fail_threshold = %v, want 3 (25%% of 10)", v.FailThreshold)
	}
}

// TestBuildVoyage_FailThresholdPercent_DifferentScopes — the same recipe percent gives
// a DIFFERENT absolute threshold at a different spawn-scope (proves the resolve happens
// at spawn-scope, not at a fixed create-scope). 30%: of 3 → 1, of 100 → 30.
func TestBuildVoyage_FailThresholdPercent_DifferentScopes(t *testing.T) {
	t.Parallel()
	mk := func() *Cadence {
		c := intervalCadence()
		c.FailThreshold = nil
		c.FailThresholdPercent = intptr(30)
		return c
	}
	cases := []struct {
		scope int
		want  int
	}{
		{3, 1},    // ceil(3*30/100)=1
		{10, 3},   // ceil(10*30/100)=3
		{100, 30}, // ceil(100*30/100)=30
	}
	for _, tc := range cases {
		resolved := make([]string, tc.scope)
		for i := range resolved {
			resolved[i] = "u"
		}
		v, _ := BuildVoyage(mk(), "VOYX", resolved)
		if v.FailThreshold == nil || *v.FailThreshold != tc.want {
			t.Errorf("scope=%d: fail_threshold = %v, want %d (30%%)", tc.scope, v.FailThreshold, tc.want)
		}
	}
}

// TestBuildVoyage_FailThresholdAbsolute — an absolute fail_threshold passes through
// as-is, percent has no effect (nil). backcompat: recipes without percent work as
// before.
func TestBuildVoyage_FailThresholdAbsolute(t *testing.T) {
	t.Parallel()
	c := intervalCadence()
	c.FailThreshold = intptr(5)
	c.FailThresholdPercent = nil
	resolved := []string{"a", "b", "c"}

	v, _ := BuildVoyage(c, "VOY05", resolved)
	if v.FailThreshold == nil || *v.FailThreshold != 5 {
		t.Errorf("fail_threshold = %v, want 5 (absolute as-is)", v.FailThreshold)
	}
}

// TestEffectiveFailThreshold — pure function resolving the threshold: clamp [1, scope],
// nil cases. Mirrors effectiveBatchSize.
func TestEffectiveFailThreshold(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name      string
		threshold *int
		percent   *int
		scope     int
		want      *int
	}{
		{"percent nil → absolute as-is", intptr(7), nil, 10, intptr(7)},
		{"both nil → nil (no threshold)", nil, nil, 10, nil},
		{"percent resolves → ceil", nil, intptr(50), 8, intptr(4)},
		{"small percent round-up → clamp 1", nil, intptr(1), 5, intptr(1)}, // ceil(5*1/100)=1
		{"percent 100 → full scope", nil, intptr(100), 6, intptr(6)},
		{"scope=0 → absolute (cannot resolve)", intptr(3), intptr(50), 0, intptr(3)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := effectiveFailThreshold(tc.threshold, tc.percent, tc.scope)
			switch {
			case tc.want == nil && got != nil:
				t.Errorf("got %d, want nil", *got)
			case tc.want != nil && got == nil:
				t.Errorf("got nil, want %d", *tc.want)
			case tc.want != nil && got != nil && *got != *tc.want:
				t.Errorf("got %d, want %d", *got, *tc.want)
			}
		})
	}
}
