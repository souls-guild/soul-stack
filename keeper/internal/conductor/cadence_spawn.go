package conductor

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/souls-guild/soul-stack/keeper/internal/cadence"
	"github.com/souls-guild/soul-stack/keeper/internal/voyage"
	"github.com/souls-guild/soul-stack/shared/audit"
)

// cadenceTxBeginner — узкая поверхность для spawn-тика: открыть tx, в которой
// due-cadences берутся FOR UPDATE SKIP LOCKED, спавнятся дочерние Voyage и
// двигаются расписания — всё атомарно (ADR-046 §4, против задвоения при крэше).
// Реальный *pgxpool.Pool удовлетворяет; unit-тесты подставляют fake-pool.
type cadenceTxBeginner interface {
	Begin(ctx context.Context) (pgx.Tx, error)
}

// CadenceSpawner — concrete-исполнитель due-cadence-спавна, которым ВЛАДЕЕТ
// Conductor (ADR-048 §3, переезд из reaper без изменения логики). Реализует
// [Spawner]. На каждом тике Conductor-лидера: SELECT due-cadences (enabled И
// next_run_at <= NOW()) FOR UPDATE SKIP LOCKED → для каждой применить
// overlap_policy → (если можно) Insert Voyage из рецепта + advance next_run_at/
// last_run_at — ВСЁ в одной PG-tx. Single-executor через Conductor-лидера (Redis-
// lease conductor:leader, ADR-006) + SKIP LOCKED дают ровно-один-спавн на тик без
// гонки.
//
// Сигнатура Run совместима с [Spawner] (`(ctx, duration, batch) → (int64,
// error)`). duration-аргумент в spawn НЕ используется (предикат — next_run_at <=
// NOW() напрямую); batchSize ограничивает число расписаний за тик (anti-lavina
// при долгом downtime). audit nil-safe; resolver-ы обязательны (без них спавн
// невозможен — Run вернёт ошибку).
type CadenceSpawner struct {
	pool      cadenceTxBeginner
	scenarioR cadence.ScenarioResolver
	commandR  cadence.CommandResolver
	audit     audit.Writer
	logger    *slog.Logger
}

// NewCadenceSpawner конструирует spawner. resolver-ы обязательны (production
// wire-up — handlers-PG-resolver-ы через адаптер); audit nil-safe; logger nil-safe.
func NewCadenceSpawner(
	pool *pgxpool.Pool,
	scenarioR cadence.ScenarioResolver,
	commandR cadence.CommandResolver,
	auditW audit.Writer,
	logger *slog.Logger,
) *CadenceSpawner {
	return &CadenceSpawner{
		pool:      pool,
		scenarioR: scenarioR,
		commandR:  commandR,
		audit:     auditW,
		logger:    logger,
	}
}

// newCadenceSpawnerFromBeginner — внутренний конструктор для unit-тестов.
func newCadenceSpawnerFromBeginner(
	pool cadenceTxBeginner,
	scenarioR cadence.ScenarioResolver,
	commandR cadence.CommandResolver,
	auditW audit.Writer,
	logger *slog.Logger,
) *CadenceSpawner {
	return &CadenceSpawner{pool: pool, scenarioR: scenarioR, commandR: commandR, audit: auditW, logger: logger}
}

// spawnedRecord — снимок одного результата тика для audit ПОСЛЕ commit-а (не
// держим события под открытой tx; best-effort эмит после фиксации БД).
type spawnedRecord struct {
	cadenceID    string
	voyageID     string // пусто для skip-записи
	scheduledFor time.Time
	scopeSize    int
	skipped      bool // true → skipped_overlap, false → spawned
}

// Run выполняет одну итерацию due-cadence-спавна. Возвращает число заспавненных
// Voyage (skip/queue-tick-и в счётчик не идут — affected = «сколько прогонов
// реально создано»). Вся работа над due-cadences — в одной tx: при крэше до
// commit-а ничего не зафиксировано, на следующем тике строки снова due
// (next_run_at не сдвинут) — задвоения нет.
//
// batchSize=0 → потолок по умолчанию ([defaultBatch]).
func (s *CadenceSpawner) Run(ctx context.Context, _ time.Duration, batchSize int) (int64, error) {
	if s.pool == nil {
		return 0, fmt.Errorf("conductor.spawn_due_cadence: pool is nil")
	}
	if s.scenarioR == nil || s.commandR == nil {
		return 0, fmt.Errorf("conductor.spawn_due_cadence: resolvers are required")
	}
	if batchSize <= 0 {
		batchSize = defaultBatch
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return 0, fmt.Errorf("conductor.spawn_due_cadence: begin tx: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback(context.Background())
		}
	}()

	due, err := cadence.SelectDueForUpdate(ctx, tx, batchSize)
	if err != nil {
		return 0, fmt.Errorf("conductor.spawn_due_cadence: %w", err)
	}

	var (
		spawned int64
		records []spawnedRecord
	)
	now := time.Now().UTC()
	for _, c := range due {
		rec, didSpawn, perr := s.processOne(ctx, tx, c, now)
		if perr != nil {
			// Ошибка обработки одной due-cadence (резолв/спавн/advance) откатывает
			// весь тик: атомарность важнее частичного прогресса (на следующем тике
			// строки снова due, повторим целиком). Возвращаем без commit-а.
			return spawned, fmt.Errorf("conductor.spawn_due_cadence: cadence %s: %w", c.ID, perr)
		}
		if rec != nil {
			records = append(records, *rec)
		}
		if didSpawn {
			spawned++
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return 0, fmt.Errorf("conductor.spawn_due_cadence: commit: %w", err)
	}
	committed = true

	// Audit ПОСЛЕ commit-а (best-effort): ошибка эмита не отменяет уже
	// зафиксированный спавн.
	for i := range records {
		s.emit(ctx, &records[i])
	}
	return spawned, nil
}

// processOne обрабатывает одну due-cadence в открытой tx: применяет overlap_policy,
// при разрешении спавнит Voyage из рецепта + двигает расписание. Возвращает
// (запись для audit | nil, спавн произошёл, ошибка).
//
// Решения по overlap_policy (ADR-046 §5):
//   - parallel — спавнить всегда (live-проверка не нужна).
//   - skip     — live есть → НЕ спавнить, audit skipped_overlap, next_run_at всё
//     равно пересчитывается (серия не залипает).
//   - queue    — live есть → НЕ спавнить и next_run_at НЕ двигать: следующий тик
//     попробует снова, как только предыдущий ребёнок терминал-ит (простая
//     queue-семантика — «ждём завершения, ближайший due-тик подхватит»).
func (s *CadenceSpawner) processOne(ctx context.Context, tx pgx.Tx, c *cadence.Cadence, now time.Time) (*spawnedRecord, bool, error) {
	scheduledFor := now
	if c.NextRunAt != nil {
		scheduledFor = c.NextRunAt.UTC()
	}

	if c.OverlapPolicy != cadence.OverlapPolicyParallel {
		live, err := cadence.HasLiveChild(ctx, tx, c.ID)
		if err != nil {
			return nil, false, err
		}
		if live {
			switch c.OverlapPolicy {
			case cadence.OverlapPolicyQueue:
				// queue: не двигаем next_run_at — оставляем строку due, следующий
				// тик повторит проверку. Без audit (это не пропуск, а ожидание).
				return nil, false, nil
			case cadence.OverlapPolicySkip:
				// skip: спавн пропущен, но расписание двигаем (серия не залипает).
				// Anchored к плановому слоту (scheduledFor), не к now — иначе skip-
				// серия дрейфовала бы так же, как spawn-путь (ADR-046 §4).
				nextRun, nerr := cadence.NextRunAnchored(c, scheduledFor, now)
				if nerr != nil {
					return nil, false, nerr
				}
				if aerr := cadence.AdvanceSchedule(ctx, tx, c.ID, nextRun, nil); aerr != nil {
					return nil, false, aerr
				}
				return &spawnedRecord{
					cadenceID:    c.ID,
					scheduledFor: scheduledFor,
					skipped:      true,
				}, false, nil
			}
		}
	}

	// Спавн разрешён (parallel, либо skip/queue без живого ребёнка).
	resolved, err := s.resolveScope(ctx, c)
	if err != nil {
		return nil, false, err
	}
	// Anchored к плановому слоту (scheduledFor = next_run_at до пересчёта), не к
	// фактическому now: дрейф тика не накапливается в next_run_at, сетка слотов
	// остаётся выровненной (ADR-046 §4, drift-free).
	nextRun, nerr := cadence.NextRunAnchored(c, scheduledFor, now)
	if nerr != nil {
		return nil, false, nerr
	}

	if len(resolved) == 0 {
		// Пустой резолв: спавнить нечего (нет живых хостов / инкарнаций под target).
		// Не fail — двигаем расписание (last_run_at = now: тик отработан), без
		// Voyage и без audit-spawned. Симметрично handler voyage_empty_target, но в
		// фоне это норма, не ошибка оператора.
		if aerr := cadence.AdvanceSchedule(ctx, tx, c.ID, nextRun, &now); aerr != nil {
			return nil, false, aerr
		}
		if s.logger != nil {
			s.logger.Info("conductor.spawn_due_cadence: пустой резолв target, спавн пропущен",
				slog.String("cadence_id", c.ID))
		}
		return nil, false, nil
	}

	voyageID := cadence.NewVoyageID()
	row, targets := cadence.BuildVoyage(c, voyageID, resolved)
	if err := voyage.Insert(ctx, tx, row); err != nil {
		return nil, false, fmt.Errorf("insert voyage: %w", err)
	}
	if err := voyage.InsertTargets(ctx, tx, voyageID, targets); err != nil {
		return nil, false, fmt.Errorf("insert voyage targets: %w", err)
	}
	if err := cadence.AdvanceSchedule(ctx, tx, c.ID, nextRun, &now); err != nil {
		return nil, false, fmt.Errorf("advance schedule: %w", err)
	}

	return &spawnedRecord{
		cadenceID:    c.ID,
		voyageID:     voyageID,
		scheduledFor: scheduledFor,
		scopeSize:    len(resolved),
	}, true, nil
}

// resolveScope резолвит declarative-target рецепта Cadence в snapshot единиц через
// scenario/command-resolver-ы (тонкая обёртка над cadence.ResolveScope для
// доступа из conductor-пакета к unexported-резолву).
func (s *CadenceSpawner) resolveScope(ctx context.Context, c *cadence.Cadence) ([]string, error) {
	return cadence.ResolveScope(ctx, c, s.scenarioR, s.commandR)
}

// emit пишет cadence.spawned либо cadence.skipped_overlap (ADR-046 §8). source =
// background (ADR-048 §4: новый source `scheduler` НЕ вводится — `background`
// семантически точен для фонового периодического keeper-правила без оператора-
// инициатора, смена исполнителя Reaper→Conductor природу события не меняет).
// archon_aid = NULL: у фонового спавна нет идентифицированного оператора-
// инициатора (семантика SourceBackground). Это НЕ created_by_aid Cadence —
// авторство рецепта живёт в Voyage.started_by_aid (см. cadence.BuildVoyage), а не
// в audit-source. nil-safe.
func (s *CadenceSpawner) emit(ctx context.Context, rec *spawnedRecord) {
	if s.audit == nil {
		return
	}
	var ev *audit.Event
	if rec.skipped {
		ev = &audit.Event{
			EventType:     audit.EventCadenceSkippedOverlap,
			Source:        audit.SourceBackground,
			CorrelationID: rec.cadenceID,
			Payload: map[string]any{
				"cadence_id":    rec.cadenceID,
				"scheduled_for": rec.scheduledFor,
				"reason":        "overlap",
			},
		}
	} else {
		ev = &audit.Event{
			EventType:     audit.EventCadenceSpawned,
			Source:        audit.SourceBackground,
			CorrelationID: rec.voyageID,
			Payload: map[string]any{
				"cadence_id":    rec.cadenceID,
				"voyage_id":     rec.voyageID,
				"scheduled_for": rec.scheduledFor,
				"scope_size":    rec.scopeSize,
			},
		}
	}
	if err := s.audit.Write(ctx, ev); err != nil && s.logger != nil {
		s.logger.Warn("conductor.spawn_due_cadence: audit write failed",
			slog.String("cadence_id", rec.cadenceID),
			slog.String("event", string(ev.EventType)),
			slog.Any("error", err))
	}
}
