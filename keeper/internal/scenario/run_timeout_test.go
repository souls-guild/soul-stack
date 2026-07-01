package scenario

import (
	"testing"
	"time"

	"github.com/souls-guild/soul-stack/keeper/internal/artifact"
	"github.com/souls-guild/soul-stack/keeper/internal/coremod/bootstrap"
	"github.com/souls-guild/soul-stack/keeper/internal/essence"
	"github.com/souls-guild/soul-stack/keeper/internal/render"
	"github.com/souls-guild/soul-stack/keeper/internal/topology"
	"github.com/souls-guild/soul-stack/shared/cel"
	"github.com/souls-guild/soul-stack/shared/config"
)

// newTimeoutRunner — Runner с заданными runTimeout / maxAwaitTimeoutFn для
// проверки effectiveRunTimeout без подъёма БД (resolver — чистая функция плана).
func newTimeoutRunner(t *testing.T, runTimeout time.Duration, ceilingFn func() time.Duration) *Runner {
	t.Helper()
	engine, err := cel.New()
	if err != nil {
		t.Fatalf("cel.New: %v", err)
	}
	return NewRunner(Deps{
		Loader:            artifact.NewServiceLoader(t.TempDir(), nil),
		Topology:          topology.NewResolver(lazyPool(t), nil, nil),
		Essence:           essence.NewResolver(nil),
		Render:            render.NewPipeline(nil, engine, nil, nil),
		Outbound:          fakeDispatcher{},
		DB:                lazyPool(t),
		RunTimeout:        runTimeout,
		MaxAwaitTimeoutFn: ceilingFn,
	})
}

// refreshEmitterTask — provision-from-zero маркер: keeper-задача
// `core.soul.registered` с refresh_soulprint:true (тот же признак, что распознаёт
// config.HasRefreshEmitter и стратификатор refresh-границы, ADR-0061).
func refreshEmitterTask() config.Task {
	return config.Task{
		On: "keeper",
		Module: &config.ModuleTask{
			Module: "core.soul.registered",
			Params: map[string]any{"refresh_soulprint": true},
		},
	}
}

// hostTask — обычная host-задача (не provision): Soul-side, без refresh-эмиттера.
func hostTask() config.Task {
	return config.Task{
		Module: &config.ModuleTask{
			Module: "core.exec.run",
			Params: map[string]any{"command": "true"},
		},
	}
}

// TestEffectiveRunTimeout_ProvisionExtends — РЕЗОЛВЕР-UNIT (главный guard этого
// бага). План с refresh-эмиттером поднимает потолок до ceiling+deployBudget,
// обычный план держит базу. Без расширения provision-прогон обрывался бы на
// defaultRunTimeout (5m), раньше joinWait (15m) и await_timeout (до 30m).
func TestEffectiveRunTimeout_ProvisionExtends(t *testing.T) {
	const ceiling = 30 * time.Minute // как config.DefaultMaxAwaitTimeout
	ceilingFn := func() time.Duration { return ceiling }

	tests := []struct {
		name  string
		base  time.Duration
		tasks []config.Task
		want  time.Duration
	}{
		{
			// provision-план: eff = 30m + 10m = 40m > base 5m → расширение.
			name:  "provision поднимает потолок до ceiling+deployBudget",
			base:  defaultRunTimeout,
			tasks: []config.Task{refreshEmitterTask(), hostTask()},
			want:  ceiling + deployBudget,
		},
		{
			// non-provision: ровно база (вечный barrier по-прежнему обрывается).
			name:  "non-provision держит базу",
			base:  defaultRunTimeout,
			tasks: []config.Task{hostTask(), hostTask()},
			want:  defaultRunTimeout,
		},
		{
			// max, не replace: оператор поднял base выше eff (50m > 40m) — НЕ урезаем.
			name:  "base выше eff не урезается (max-семантика)",
			base:  50 * time.Minute,
			tasks: []config.Task{refreshEmitterTask()},
			want:  50 * time.Minute,
		},
		{
			// пустой план — не provision, база.
			name:  "пустой план — база",
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

// TestEffectiveRunTimeout_NilCeilingFnFallsBack — без config.Store (unit/L0)
// maxAwaitTimeoutFn==nil: ceiling берётся из config.DefaultMaxAwaitTimeout,
// provision-прогон всё равно получает расширенный потолок (просто без
// hot-reload-override-а оператора).
func TestEffectiveRunTimeout_NilCeilingFnFallsBack(t *testing.T) {
	r := newTimeoutRunner(t, defaultRunTimeout, nil) // ceilingFn == nil
	got := r.effectiveRunTimeout([]config.Task{refreshEmitterTask()})
	want := config.DefaultMaxAwaitTimeout + deployBudget
	if got != want {
		t.Errorf("effectiveRunTimeout(nil-fn) = %s, want %s (DefaultMaxAwaitTimeout+deployBudget)", got, want)
	}
}

// TestEffectiveRunTimeout_HotReloadCeiling — maxAwaitTimeoutFn читается на КАЖДОМ
// резолве (hot-reload): оператор поднял keeper.yml::max_await_timeout → следующий
// provision-прогон видит новый ceiling без рестарта.
func TestEffectiveRunTimeout_HotReloadCeiling(t *testing.T) {
	ceiling := 30 * time.Minute
	r := newTimeoutRunner(t, defaultRunTimeout, func() time.Duration { return ceiling })

	if got := r.effectiveRunTimeout([]config.Task{refreshEmitterTask()}); got != ceiling+deployBudget {
		t.Fatalf("до reload: %s, want %s", got, ceiling+deployBudget)
	}
	ceiling = 60 * time.Minute // оператор переопределил снапшот keeper.yml
	if got := r.effectiveRunTimeout([]config.Task{refreshEmitterTask()}); got != ceiling+deployBudget {
		t.Errorf("после reload: %s, want %s (новый ceiling подхвачен)", got, ceiling+deployBudget)
	}
}

// TestProvisionTimeoutExceedsJoinWait — СТАТИЧЕСКИЙ ГАРД-ИНВАРИАНТ (ADR-0061):
// provision-aware effective run-timeout (минимум ceiling+deployBudget при дефолтном
// ceiling-е) обязан СТРОГО превышать дефолтный joinWait Teleport-join. Иначе
// настройка «мертва»: барьер онбординга `await_online` / join-retry упрутся в обрыв
// прогона раньше, чем дождутся хоста (исходный баг). Ловит будущее увеличение
// joinWait / уменьшение бюджетов, делающее provision-прогон снова недостижимым.
func TestProvisionTimeoutExceedsJoinWait(t *testing.T) {
	provisionFloor := config.DefaultMaxAwaitTimeout + deployBudget
	if provisionFloor <= bootstrap.DefaultJoinWaitTimeout {
		t.Errorf(
			"provision effective run-timeout floor (%s = DefaultMaxAwaitTimeout %s + deployBudget %s) НЕ превышает joinWait (%s) — provision-прогон оборвётся до завершения онбординга (мёртвая настройка)",
			provisionFloor, config.DefaultMaxAwaitTimeout, deployBudget, bootstrap.DefaultJoinWaitTimeout)
	}
}
