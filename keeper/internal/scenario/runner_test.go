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

// fakeDispatcher — no-op ApplyDispatcher для lifecycle-тестов.
type fakeDispatcher struct{}

func (fakeDispatcher) SendApply(context.Context, string, *keeperv1.ApplyRequest) error { return nil }

// lazyPool строит *pgxpool.Pool без фактического коннекта (pgxpool ленив:
// New не открывает соединений до первого запроса). Нужен лишь как non-nil
// зависимость NewRunner — тесты ниже не доходят до запросов в БД.
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
	NewRunner(Deps{}) // все nil
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
	// Shutdown без активных прогонов завершается сразу.
	if err := r.Shutdown(context.Background()); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	// Валидный spec, но runner в shutting-down → ErrShuttingDown БЕЗ спавна
	// goroutine (проверка раньше go func).
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

// TestRequestCancel_EmptyApplyID — пустой apply_id отвергается ДО обращения к
// БД и до локального Cancel: cluster-wide отмена не имеет смысла без apply_id.
// Проверка раньше PG-вызова — иначе RequestCancel ушёл бы в lazyPool (нет
// коннекта) и упал на сетевой ошибке вместо валидационной.
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

// TestAllKeeperTasks — условие bypass-а no_hosts-гейта (ADR-0061 §контекст):
// все задачи keeper-side → true (provision-from-zero), любая host-задача / пустой
// сценарий → false. keeper-side форма — скаляр `on: keeper`; host-формы — опущен
// on: (nil) или список ковенов.
func TestAllKeeperTasks(t *testing.T) {
	keeper := config.Task{On: "keeper"}
	hostOmitted := config.Task{}                      // on: опущен → Soul-side (весь incarnation)
	hostCoven := config.Task{On: []any{"redis-prod"}} // on: список ковенов → Soul-side

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

// TestFailureStatus_ByMode — терминал провала прогона по режиму финала (S-D2b):
// обычный прогон → error_locked; teardown → destroy_failed (НЕ error_locked).
func TestFailureStatus_ByMode(t *testing.T) {
	if got := failureStatus(TerminalCommitState); got != incarnation.StatusErrorLocked {
		t.Errorf("failureStatus(CommitState) = %q, want error_locked", got)
	}
	if got := failureStatus(TerminalDestroy); got != incarnation.StatusDestroyFailed {
		t.Errorf("failureStatus(Destroy) = %q, want destroy_failed", got)
	}
}

// TestStart_ConvergeAcceptedByGate — guard (amend ADR-031, 2026-06-10):
// `converge` — operational-сценарий, запускаемый обычным run-ом. Старт-приёмка
// [Runner.Start] / гейт [Runner.lockRun] (TerminalCommitState) НЕ дискриминируют
// по имени сценария — converge проходит ровно тот же путь, что любой
// operational-сценарий, и НЕ отвергается из ready как «спец-имя».
//
// Проверяем на shutdown-пути: после Shutdown Start возвращает ErrShuttingDown
// (а НЕ валидационную ошибку имени) — значит converge принят приёмкой как
// валидное имя, как `create`. Полный happy-path-старт из ready проверяется
// docker-gated integration-флоу (gate status-driven: ready/drift → applying,
// без сверки имени).
func TestStart_ConvergeAcceptedByGate(t *testing.T) {
	r := newTestRunner(t)
	if err := r.Shutdown(context.Background()); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	err := r.Start(context.Background(), RunSpec{
		ApplyID:         "a",
		IncarnationName: "i",
		ScenarioName:    ConvergeScenarioName, // operational run, не спец-имя
	})
	if !errors.Is(err, ErrShuttingDown) {
		t.Errorf("Start(converge) = %v, want ErrShuttingDown (имя converge принято приёмкой, не отвергнуто)", err)
	}
}

// TestStartDestroy_RefusedAfterShutdown — StartDestroy форсит ScenarioName=
// `destroy` (caller может оставить поле пустым) и проходит ту же приёмку, что
// Start: после Shutdown отклоняется ErrShuttingDown БЕЗ спавна goroutine (и без
// обращения к lazyPool-DB). Проверяет, что StartDestroy не падает на пустом
// ScenarioName — он его проставляет сам.
func TestStartDestroy_RefusedAfterShutdown(t *testing.T) {
	r := newTestRunner(t)
	if err := r.Shutdown(context.Background()); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	err := r.StartDestroy(context.Background(), RunSpec{
		ApplyID:         "a",
		IncarnationName: "i",
		// ScenarioName намеренно пуст — StartDestroy обязан проставить `destroy`.
	})
	if !errors.Is(err, ErrShuttingDown) {
		t.Errorf("StartDestroy after Shutdown = %v, want ErrShuttingDown", err)
	}
}
