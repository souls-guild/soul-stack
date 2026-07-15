package scenario

import (
	"context"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/souls-guild/soul-stack/keeper/internal/artifact"
	"github.com/souls-guild/soul-stack/keeper/internal/essence"
	"github.com/souls-guild/soul-stack/keeper/internal/incarnation"
	"github.com/souls-guild/soul-stack/keeper/internal/render"
	"github.com/souls-guild/soul-stack/keeper/internal/topology"
	keeperv1 "github.com/souls-guild/soul-stack/proto/gen/go/keeper/v1"
	"github.com/souls-guild/soul-stack/shared/cel"
	"github.com/souls-guild/soul-stack/shared/config"
)

// fakeDispatcher is a no-op ApplyDispatcher for lifecycle tests.
type fakeDispatcher struct{}

func (fakeDispatcher) SendApply(context.Context, string, *keeperv1.ApplyRequest) error { return nil }

// lazyPool builds a *pgxpool.Pool without an actual connection (pgxpool is
// lazy: New doesn't open connections until the first query). Needed only as a
// non-nil NewRunner dependency — the tests below never reach a DB query.
func lazyPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	pool, err := pgxpool.New(context.Background(),
		"postgres://x:x@127.0.0.1:1/x?sslmode=disable")
	if err != nil {
		t.Fatalf("lazyPool: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

func newTestRunner(t *testing.T) *Runner {
	t.Helper()
	engine, err := cel.New()
	if err != nil {
		t.Fatalf("cel.New: %v", err)
	}
	return NewRunner(Deps{
		Loader:   artifact.NewServiceLoader(t.TempDir(), nil),
		Topology: topology.NewResolver(lazyPool(t), nil, nil),
		Essence:  essence.NewResolver(nil),
		Render:   render.NewPipeline(nil, engine, nil, nil),
		Outbound: fakeDispatcher{},
		DB:       lazyPool(t),
	})
}

func TestNewRunner_NilDepPanics(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("NewRunner с nil-зависимостью должен паниковать")
		}
	}()
	NewRunner(Deps{}) // all nil
}

func TestStart_Validation(t *testing.T) {
	r := newTestRunner(t)
	ctx := context.Background()

	tests := []struct {
		name string
		spec RunSpec
	}{
		{"empty apply_id", RunSpec{IncarnationName: "i", ScenarioName: "create"}},
		{"empty incarnation", RunSpec{ApplyID: "a", ScenarioName: "create"}},
		{"empty scenario", RunSpec{ApplyID: "a", IncarnationName: "i"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := r.Start(ctx, tt.spec); err == nil {
				t.Errorf("Start(%+v) = nil, want validation error", tt.spec)
			}
		})
	}
}

func TestStart_AfterShutdown_Refused(t *testing.T) {
	r := newTestRunner(t)
	// Shutdown with no active runs completes immediately.
	if err := r.Shutdown(context.Background()); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	// Valid spec, but the runner is shutting down → ErrShuttingDown with NO
	// goroutine spawned (checked before go func).
	err := r.Start(context.Background(), RunSpec{
		ApplyID: "a", IncarnationName: "i", ScenarioName: "create",
	})
	if !errors.Is(err, ErrShuttingDown) {
		t.Errorf("Start after Shutdown = %v, want ErrShuttingDown", err)
	}
}

func TestCancel_UnknownApplyID(t *testing.T) {
	r := newTestRunner(t)
	if r.Cancel("nonexistent") {
		t.Error("Cancel(unknown) = true, want false")
	}
}

// TestRequestCancel_EmptyApplyID — an empty apply_id is rejected BEFORE
// touching the DB or the local Cancel: a cluster-wide cancel is meaningless
// without an apply_id. Checked ahead of the PG call, or RequestCancel would
// hit lazyPool (no connection) and fail with a network error instead of a
// validation one.
func TestRequestCancel_EmptyApplyID(t *testing.T) {
	r := newTestRunner(t)
	found, err := r.RequestCancel(context.Background(), "")
	if err == nil {
		t.Fatal("RequestCancel(\"\") = nil error, want validation error")
	}
	if found {
		t.Error("found = true для пустого apply_id, want false")
	}
}

func TestShutdown_NoActiveRuns(t *testing.T) {
	r := newTestRunner(t)
	if err := r.Shutdown(context.Background()); err != nil {
		t.Errorf("Shutdown(idle) = %v, want nil", err)
	}
}

// TestAllKeeperTasks — the bypass condition for the no_hosts gate (ADR-0061
// §context): all tasks keeper-side → true (provision-from-zero), any host
// task / empty scenario → false. keeper-side form is the scalar `on:
// keeper`; host forms are an omitted on: (nil) or a list of covens.
func TestAllKeeperTasks(t *testing.T) {
	keeper := config.Task{On: "keeper"}
	hostOmitted := config.Task{}                      // on: omitted → Soul-side (whole incarnation)
	hostCoven := config.Task{On: []any{"redis-prod"}} // on: a list of covens → Soul-side

	tests := []struct {
		name  string
		tasks []config.Task
		want  bool
	}{
		{"пусто", nil, false},
		{"пустой-срез", []config.Task{}, false},
		{"один-keeper", []config.Task{keeper}, true},
		{"все-keeper", []config.Task{keeper, keeper}, true},
		{"один-host-опущен", []config.Task{hostOmitted}, false},
		{"один-host-coven", []config.Task{hostCoven}, false},
		{"keeper-плюс-host", []config.Task{keeper, hostOmitted}, false},
		{"host-плюс-keeper", []config.Task{hostCoven, keeper}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := allKeeperTasks(tt.tasks); got != tt.want {
				t.Errorf("allKeeperTasks(%+v) = %v, want %v", tt.tasks, got, tt.want)
			}
		})
	}
}

// TestFailureStatus_ByMode — the failure terminal of a run by finalization
// mode (S-D2b): a regular run → error_locked; teardown → destroy_failed (NOT
// error_locked).
func TestFailureStatus_ByMode(t *testing.T) {
	if got := failureStatus(TerminalCommitState); got != incarnation.StatusErrorLocked {
		t.Errorf("failureStatus(CommitState) = %q, want error_locked", got)
	}
	if got := failureStatus(TerminalDestroy); got != incarnation.StatusDestroyFailed {
		t.Errorf("failureStatus(Destroy) = %q, want destroy_failed", got)
	}
}

// TestStart_ConvergeAcceptedByGate — guard (amend ADR-031, 2026-06-10):
// `converge` is an operational scenario run through the regular run path.
// The start acceptance [Runner.Start] / gate [Runner.lockRun]
// (TerminalCommitState) don't discriminate by scenario name — converge takes
// exactly the same path as any operational scenario and isn't rejected from
// ready as a "special name".
//
// Checked on the shutdown path: after Shutdown, Start returns ErrShuttingDown
// (NOT a name validation error) — meaning converge is accepted as a valid
// name, same as `create`. The full happy-path start from ready is checked by
// the docker-gated integration flow (status-driven gate: ready/drift →
// applying, no name check).
func TestStart_ConvergeAcceptedByGate(t *testing.T) {
	r := newTestRunner(t)
	if err := r.Shutdown(context.Background()); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	err := r.Start(context.Background(), RunSpec{
		ApplyID:         "a",
		IncarnationName: "i",
		ScenarioName:    ConvergeScenarioName, // operational run, not a special name
	})
	if !errors.Is(err, ErrShuttingDown) {
		t.Errorf("Start(converge) = %v, want ErrShuttingDown (имя converge принято приёмкой, не отвергнуто)", err)
	}
}

// TestStartDestroy_RefusedAfterShutdown — StartDestroy forces
// ScenarioName=`destroy` (the caller may leave it empty) and goes through the
// same acceptance as Start: after Shutdown it's rejected with ErrShuttingDown
// with NO goroutine spawned (and no lazyPool DB access). Checks StartDestroy
// doesn't fail on an empty ScenarioName — it sets it itself.
func TestStartDestroy_RefusedAfterShutdown(t *testing.T) {
	r := newTestRunner(t)
	if err := r.Shutdown(context.Background()); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	err := r.StartDestroy(context.Background(), RunSpec{
		ApplyID:         "a",
		IncarnationName: "i",
		// ScenarioName intentionally empty — StartDestroy must set it itself.
	})
	if !errors.Is(err, ErrShuttingDown) {
		t.Errorf("StartDestroy after Shutdown = %v, want ErrShuttingDown", err)
	}
}
