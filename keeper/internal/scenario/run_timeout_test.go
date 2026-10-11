package scenario

import (
	"testing"
	"time"

	"github.com/souls-guild/soul-stack/keeper/internal/artifact"
	coremodssh "github.com/souls-guild/soul-stack/keeper/internal/coremod/ssh"
	"github.com/souls-guild/soul-stack/keeper/internal/render"
	"github.com/souls-guild/soul-stack/keeper/internal/servicevars"
	"github.com/souls-guild/soul-stack/keeper/internal/topology"
	"github.com/souls-guild/soul-stack/shared/cel"
	"github.com/souls-guild/soul-stack/shared/config"
)

// newTimeoutRunner builds a Runner with the given runTimeout /
// maxAwaitTimeoutFn to test effectiveRunTimeout without a DB (the resolver
// is a pure function of the plan).
func newTimeoutRunner(t *testing.T, runTimeout time.Duration, ceilingFn func() time.Duration) *Runner {
	t.Helper()
	engine, err := cel.New()
	if err != nil {
		t.Fatalf("cel.New: %v", err)
	}
	return NewRunner(Deps{
		Loader:            artifact.NewServiceLoader(t.TempDir(), nil),
		Topology:          topology.NewResolver(lazyPool(t), nil, nil),
		ServiceVars:       servicevars.NewResolver(nil),
		Render:            render.NewPipeline(nil, engine, nil, nil),
		Outbound:          fakeDispatcher{},
		DB:                lazyPool(t),
		RunTimeout:        runTimeout,
		MaxAwaitTimeoutFn: ceilingFn,
	})
}

// refreshEmitterTask is the provision-from-zero marker: a keeper task
// `core.soul.registered` with refresh_soulprint:true (the same signal
// recognized by config.HasRefreshEmitter and the refresh-boundary
// stratifier, ADR-0061).
func refreshEmitterTask() config.Task {
	return config.Task{
		On: "keeper",
		Module: &config.ModuleTask{
			Module: "core.soul.registered",
			Params: map[string]any{"refresh_soulprint": true},
		},
	}
}

// hostTask is a plain host task (not provision): Soul-side, no refresh emitter.
func hostTask() config.Task {
	return config.Task{
		Module: &config.ModuleTask{
			Module: "core.exec.run",
			Params: map[string]any{"command": "true"},
		},
	}
}

// TestEffectiveRunTimeout_ProvisionExtends is the RESOLVER UNIT test (the
// main guard for this bug). A plan with a refresh emitter raises the ceiling
// to ceiling+deployBudget; a regular plan keeps the base. Without the
// extension, a provision run would time out at defaultRunTimeout (5m), before
// await_timeout (up to 30m).
func TestEffectiveRunTimeout_ProvisionExtends(t *testing.T) {
	const ceiling = 30 * time.Minute // same as config.DefaultMaxAwaitTimeout
	ceilingFn := func() time.Duration { return ceiling }

	tests := []struct {
		name  string
		base  time.Duration
		tasks []config.Task
		want  time.Duration
	}{
		{
			// provision plan: eff = 30m + 10m = 40m > base 5m → extension.
			name:  "provision raises ceiling to ceiling+deployBudget",
			base:  defaultRunTimeout,
			tasks: []config.Task{refreshEmitterTask(), hostTask()},
			want:  ceiling + deployBudget,
		},
		{
			// non-provision: exactly the base (the eternal barrier still fires).
			name:  "non-provision keeps the base",
			base:  defaultRunTimeout,
			tasks: []config.Task{hostTask(), hostTask()},
			want:  defaultRunTimeout,
		},
		{
			// max, not replace: the operator raised base above eff (50m > 40m) — we do NOT clamp it down.
			name:  "base above eff is not clamped down (max semantics)",
			base:  50 * time.Minute,
			tasks: []config.Task{refreshEmitterTask()},
			want:  50 * time.Minute,
		},
		{
			// empty plan — not provision, base.
			name:  "empty plan -- base",
			base:  defaultRunTimeout,
			tasks: nil,
			want:  defaultRunTimeout,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := newTimeoutRunner(t, tt.base, ceilingFn)
			if got := r.effectiveRunTimeout(tt.tasks); got != tt.want {
				t.Errorf("effectiveRunTimeout = %s, want %s", got, tt.want)
			}
		})
	}
}

// TestEffectiveRunTimeout_NilCeilingFnFallsBack — without config.Store
// (unit/L0), maxAwaitTimeoutFn==nil: the ceiling comes from
// config.DefaultMaxAwaitTimeout, and a provision run still gets the
// extended ceiling (just without the operator's hot-reload override).
func TestEffectiveRunTimeout_NilCeilingFnFallsBack(t *testing.T) {
	r := newTimeoutRunner(t, defaultRunTimeout, nil) // ceilingFn == nil
	got := r.effectiveRunTimeout([]config.Task{refreshEmitterTask()})
	want := config.DefaultMaxAwaitTimeout + deployBudget
	if got != want {
		t.Errorf("effectiveRunTimeout(nil-fn) = %s, want %s (DefaultMaxAwaitTimeout+deployBudget)", got, want)
	}
}

// TestEffectiveRunTimeout_HotReloadCeiling — maxAwaitTimeoutFn is read on
// EVERY resolve (hot-reload): the operator raises
// keeper.yml::max_await_timeout → the next provision run sees the new
// ceiling with no restart.
func TestEffectiveRunTimeout_HotReloadCeiling(t *testing.T) {
	ceiling := 30 * time.Minute
	r := newTimeoutRunner(t, defaultRunTimeout, func() time.Duration { return ceiling })

	if got := r.effectiveRunTimeout([]config.Task{refreshEmitterTask()}); got != ceiling+deployBudget {
		t.Fatalf("before reload: %s, want %s", got, ceiling+deployBudget)
	}
	ceiling = 60 * time.Minute // operator overrode the keeper.yml snapshot
	if got := r.effectiveRunTimeout([]config.Task{refreshEmitterTask()}); got != ceiling+deployBudget {
		t.Errorf("after reload: %s, want %s (new ceiling picked up)", got, ceiling+deployBudget)
	}
}

// TestProvisionTimeoutExceedsJoinWait is a STATIC GUARD INVARIANT (ADR-0061),
// removed with `core.bootstrap.delivered` (NIM-834) and back with the transport
// that replaced it (NIM-849) — exactly as that removal note predicted, because
// anything that waits for a host to become reachable reintroduces this class of
// dead setting.
//
// A provision run's bound at its defaults MUST STRICTLY exceed what the run waits
// for before the hosts exist: the default connect wait of `core.ssh.run` — which
// since NIM-872 bounds BOTH transports, a Teleport join and a fresh VM's sshd
// alike, off one default — and, in a linear run, the `await_online` ceiling after
// it. Otherwise the setting is dead: the run aborts before the host is ever
// reachable, so the wait can never be spent. A linear run's bound is its
// scenario_timeout (NIM-949); a run under the deprecated staged rollback keeps
// ceiling + deployBudget.
func TestProvisionTimeoutExceedsJoinWait(t *testing.T) {
	r := newTimeoutRunner(t, defaultRunTimeout, nil)
	plan := &config.ScenarioManifest{Tasks: []config.Task{refreshEmitterTask(), hostTask()}}
	for model, waits := range map[ExecutionModel]time.Duration{
		ExecutionStaged: coremodssh.DefaultJoinWaitTimeout,
		ExecutionLinear: coremodssh.DefaultJoinWaitTimeout + config.DefaultMaxAwaitTimeout,
	} {
		if bound := r.runDeadline(RunSpec{executionModel: model}, plan); bound <= waits {
			t.Errorf("%s provision run bound %s does NOT exceed what it waits for before its hosts exist (%s) — the run would abort before onboarding finishes (a dead setting)",
				model, bound, waits)
		}
	}
}

// ★ GUARD (NIM-949). A linear run is bounded by its scenario_timeout alone: 24h by
// default, the scenario's own value when it writes one — never the 5-minute run
// ceiling nor the provision floor read from keeper.yml. A staged run keeps both.
//
// Mutation: runDeadline returns r.effectiveRunTimeout for a linear run → red.
func TestRunDeadline_LinearIsScenarioTimeout(t *testing.T) {
	r := newTimeoutRunner(t, defaultRunTimeout, func() time.Duration { return 30 * time.Minute })
	twoHours := 2 * time.Hour
	plain := &config.ScenarioManifest{Tasks: []config.Task{hostTask()}}
	provision := &config.ScenarioManifest{Tasks: []config.Task{refreshEmitterTask(), hostTask()}}
	own := &config.ScenarioManifest{Tasks: []config.Task{hostTask()}, Settings: &config.ScenarioSettings{ScenarioTimeout: &twoHours}}

	for _, tc := range []struct {
		name  string
		model ExecutionModel
		scn   *config.ScenarioManifest
		want  time.Duration
	}{
		{"linear default", ExecutionLinear, plain, 24 * time.Hour},
		{"linear provision", ExecutionLinear, provision, 24 * time.Hour},
		{"linear own scenario_timeout", ExecutionLinear, own, twoHours},
		{"staged keeps the run ceiling", ExecutionStaged, own, defaultRunTimeout},
		{"staged keeps the provision floor", ExecutionStaged, provision, 30*time.Minute + deployBudget},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := r.runDeadline(RunSpec{executionModel: tc.model}, tc.scn); got != tc.want {
				t.Errorf("runDeadline = %s, want %s", got, tc.want)
			}
		})
	}
}

// ★ GUARD (NIM-949). soul_timeout is read from the step's barrier owner, so a
// step that writes its own differs from the scenario default beside it.
//
// Mutation: read stepSettings[0] for every step → step 1 answers 1m → red.
func TestStepSoulTimeout_PerStep(t *testing.T) {
	fiveMin := 5 * time.Minute
	tasks := []config.Task{hostTask(), hostTask()}
	tasks[1].Settings = &config.ScenarioSettings{SoulTimeout: &fiveMin}
	spec := RunSpec{executionModel: ExecutionLinear, stepSettings: config.StepSettings(tasks, nil)}
	for passage, want := range []time.Duration{time.Minute, fiveMin} {
		if got := stepSoulTimeout(spec, passage); got != want {
			t.Errorf("step %d soul_timeout = %s, want %s", passage, got, want)
		}
	}
	if got := stepSoulTimeout(RunSpec{}, 3); got != time.Minute {
		t.Errorf("no step settings: soul_timeout = %s, want the 1m default", got)
	}
}
