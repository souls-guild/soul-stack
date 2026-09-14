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
// The provision-aware effective run-timeout floor (ceiling + deployBudget at the
// default ceiling) MUST STRICTLY exceed the default connect wait of
// `core.ssh.run` — which since NIM-872 bounds BOTH transports, a Teleport join
// and a fresh VM's sshd alike, off one default. Otherwise the setting is dead:
// the run aborts before the host is ever reachable, so the wait can never be
// spent. It catches a future join-wait increase or budget decrease that would
// make a provision run unreachable again.
func TestProvisionTimeoutExceedsJoinWait(t *testing.T) {
	provisionFloor := config.DefaultMaxAwaitTimeout + deployBudget
	if provisionFloor <= coremodssh.DefaultJoinWaitTimeout {
		t.Errorf(
			"provision effective run-timeout floor (%s = DefaultMaxAwaitTimeout %s + deployBudget %s) does NOT exceed join_wait_timeout (%s) — a provision run would abort before onboarding finishes (a dead setting)",
			provisionFloor, config.DefaultMaxAwaitTimeout, deployBudget, coremodssh.DefaultJoinWaitTimeout)
	}
}
