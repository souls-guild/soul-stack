// Package conductor — Conductor, leader-elected исполнитель Cadence-расписаний
// (ADR-048). Дирижёр задаёт ритм исполнения: на каждом тике лидер спавнит
// созревшие (due) Cadence в дочерние Voyage.
//
// Conductor — keeper-side singleton-подсистема (не отдельный бинарь, как и
// Reaper). Сидит на generic [leaderloop.Loop] со своим Redis-lease
// [LeaseKey] = "conductor:leader" — независимым от reaper-lease: лидер Conductor
// и лидер Reaper могут быть разными инстансами (ADR-048 §1). Свой tick-interval
// (cadence_scheduler.interval, ~15–30s) не зависит от reaper.interval (1h):
// scheduling-домен Cadence и cleanup-домен Reaper имеют разный естественный ритм.
//
// Spawn-семантика (due-выборка FOR UPDATE SKIP LOCKED, три overlap_policy,
// пересчёт next_run_at, single-executor в одной PG-tx) — в [Spawner],
// concrete-реализация [CadenceSpawner] переехала сюда из reaper (C3, ADR-048 §3,
// дословный перенос логики ADR-046 §4). Интерфейс оставлен для тестируемости
// (fakeSpawner в тестах scheduler-а). C4 удалил reaper-исполнителя — Conductor
// единственный исполнитель спавна; двойного/нулевого спавна нет
// (switchover-безопасность: FOR UPDATE SKIP LOCKED + advance next_run_at в одной
// tx страхуют любой транзиент при переключении исполнителя).
package conductor

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/souls-guild/soul-stack/keeper/internal/leaderloop"
	"github.com/souls-guild/soul-stack/keeper/internal/redis"
)

// LeaseKey — Redis-ключ лидерства Conductor (ADR-048 §1). Отдельный от
// "reaper:leader" — независимое лидерство двух фоновых подсистем кластера.
const LeaseKey = "conductor:leader"

// defaultBatch — потолок числа due-Cadence, спавнящихся за один тик, если
// [Config.BatchFn] не задан. Anti-lavina при долгом downtime кластера:
// накопившиеся due-расписания не лавинят флот одним тиком, остаток подберётся на
// следующих тиках. Совпадает с историческим reaper-дефолтом spawn_due_cadence.
const defaultBatch = 100

// Spawner — узкая поверхность исполнителя due-cadence-спавна, которую дёргает
// tick-callback Conductor. Сигнатура: `(ctx, duration, batchSize) →
// (spawnedCount, error)`. duration-аргумент в spawn НЕ используется (предикат —
// next_run_at <= NOW() напрямую) и передаётся нулевым; batchSize ограничивает
// число расписаний за тик.
//
// Production wire-up подаёт [*CadenceSpawner] (переехал в этот пакет, C3).
// Интерфейс оставлен ради тестируемости: тесты scheduler-а подменяют
// fakeSpawner.
type Spawner interface {
	Run(ctx context.Context, _ time.Duration, batchSize int) (int64, error)
}

// Config — параметры Conductor-планировщика. Все поля, кроме [Config.BatchFn],
// [Config.AcquireBackoff] и [Config.OnLeaseChange], обязательны: отсутствие —
// программная ошибка caller-а (wire-up), [New] возвращает error.
type Config struct {
	// Holder — идентификатор инстанса (KID) для lease-ключа и логов.
	Holder string

	// Redis — клиент, через который захватывается lease.
	Redis *redis.Client

	// Logger — slog-логгер.
	Logger *slog.Logger

	// Spawner — исполнитель due-cadence-спавна (см. [Spawner]).
	Spawner Spawner

	// IntervalFn — интервал между тиками (hot-reload: перечитывается на каждом
	// тике). Conductor-специфичный, независимый от reaper.interval (ADR-048 §2).
	IntervalFn func() time.Duration

	// LockTTLFn — TTL Redis-lease-ключа (hot-reload между re-acquire).
	LockTTLFn func() time.Duration

	// BatchFn — потолок числа due-Cadence за тик (hot-reload). Nil или
	// non-positive результат → [defaultBatch].
	BatchFn func() int

	// AcquireBackoff — пауза между попытками захвата lease. Zero → дефолт
	// leaderloop-а.
	AcquireBackoff time.Duration

	// OnLeaseChange — опциональный callback смены статуса лидерства (true при
	// захвате, false при выходе из tick-loop-а). Nil — допустимо. Production
	// wire-up подаёт [ConductorMetrics.SetLeaseHeld] (lease-Gauge, C5).
	OnLeaseChange func(held bool)

	// Metrics — Prometheus-collectors тика спавна (executions / spawned /
	// errors / duration). Nil допустим — [ConductorMetrics.ObserveSpawn] no-op-ит
	// на nil-получателе (unit-тесты scheduler-а без obs-стека).
	Metrics *ConductorMetrics
}

// Scheduler — корневая структура Conductor. Один экземпляр на keeper-процесс.
// Создаётся через [New], запускается через [Scheduler.Run].
type Scheduler struct {
	cfg Config
}

// New валидирует конфиг и возвращает Scheduler. На отсутствующие обязательные
// поля — error: программная ошибка caller-а (wire-up), не runtime-условие.
func New(cfg Config) (*Scheduler, error) {
	if cfg.Holder == "" {
		return nil, errors.New("conductor.New: Holder is required")
	}
	if cfg.Redis == nil {
		return nil, errors.New("conductor.New: Redis is required")
	}
	if cfg.Logger == nil {
		return nil, errors.New("conductor.New: Logger is required")
	}
	if cfg.Spawner == nil {
		return nil, errors.New("conductor.New: Spawner is required")
	}
	if cfg.IntervalFn == nil {
		return nil, errors.New("conductor.New: IntervalFn is required")
	}
	if cfg.LockTTLFn == nil {
		return nil, errors.New("conductor.New: LockTTLFn is required")
	}
	return &Scheduler{cfg: cfg}, nil
}

// Run крутит leader-loop до отмены ctx. Лидерство, renewal, re-acquire и
// graceful-shutdown делегированы generic [leaderloop.Loop]; Conductor — тонкий
// потребитель: tick-callback зовёт [Spawner.Run] над свежими hot-reload-снимками
// interval/lock_ttl/batch.
//
// Возвращает nil на graceful-stop (ctx.Done) и обёрнутую error на fatal-условия
// acquire-фазы.
func (s *Scheduler) Run(ctx context.Context) error {
	loop, err := leaderloop.New(leaderloop.Config{
		LeaseKey:       LeaseKey,
		Holder:         s.cfg.Holder,
		Redis:          s.cfg.Redis,
		Logger:         s.cfg.Logger,
		AcquireBackoff: s.cfg.AcquireBackoff,
		IntervalFn:     s.cfg.IntervalFn,
		LockTTLFn:      s.cfg.LockTTLFn,
		Tick:           s.tick,
		OnLeaseChange:  s.cfg.OnLeaseChange,
	})
	if err != nil {
		// Обязательные поля проверены New-ом → здесь не должно падать.
		// Прокидываем на случай рассинхрона контрактов.
		return err
	}
	return loop.Run(ctx)
}

// tick — tick-callback для leaderloop: спавнит due-cadence через [Spawner].
// Ошибка спавна не роняет loop (best-effort фоновое правило, parity Reaper):
// логируется warn-ом, следующий тик повторит (строки остались due — next_run_at
// при ошибке spawn-tx не сдвигается, см. ADR-046 §4).
func (s *Scheduler) tick(ctx context.Context) {
	batch := defaultBatch
	if s.cfg.BatchFn != nil {
		if b := s.cfg.BatchFn(); b > 0 {
			batch = b
		}
	}
	// duration-аргумент в spawn не используется (предикат — next_run_at <= NOW()).
	start := time.Now()
	spawned, err := s.cfg.Spawner.Run(ctx, 0, batch)
	s.cfg.Metrics.ObserveSpawn(spawned, err, time.Since(start))
	if err != nil {
		s.cfg.Logger.Warn("conductor: spawn due cadence failed",
			slog.Any("error", err))
		return
	}
	if spawned > 0 {
		s.cfg.Logger.Info("conductor: spawned voyages from due cadences",
			slog.Int64("count", spawned))
	}
}
