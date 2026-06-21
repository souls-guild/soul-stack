package reaper

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/souls-guild/soul-stack/keeper/internal/incarnation"
	"github.com/souls-guild/soul-stack/keeper/internal/redis"
	"github.com/souls-guild/soul-stack/shared/audit"
)

// orphanApplyingCandidatesSQL — фаза 1 правила `reconcile_orphan_applying`
// (ADR-027 amend (m)): stale-кандидаты на снятие осиротевшего applying-lock.
//
// Предикат (использует partial-индекс `incarnation_applying_scan_idx`, миграция
// 082):
//   - `status='applying'`        — lock держится;
//   - `applying_since < cutoff`  — lock взят дольше stale_after назад (cutoff =
//     NOW()-stale_after передаётся параметром $1 для тестируемости clock-а);
//   - `applying_by_kid IS NOT NULL` — epoch ИЗВЕСТЕН. NULL-epoch (legacy/pre-082
//     либо applying не через S1-lockRun) НЕ реклеймится — без applying_by_kid нет
//     presence-свидетеля смерти владельца (документированный known-gap, чинится
//     ручным Unlock оператором).
//
// Возвращает (name, applying_by_kid, applying_apply_id) — фаза 2 спрашивает
// Conclave про applying_by_kid, фаза 3 снимает по applying_apply_id.
const orphanApplyingCandidatesSQL = `
SELECT name, applying_by_kid, applying_apply_id
FROM incarnation
WHERE status = 'applying'
  AND applying_since < $1
  AND applying_by_kid IS NOT NULL
`

// orphanApplyingReleaser — узкая поверхность снятия осиротевшего applying-lock
// (incarnation.ReleaseApplyingOrphan). Интерфейс, а не прямой вызов package-
// функции, держит правило unit-тестируемым без поднятия Postgres: fake фиксирует
// аргументы и программирует исход (снят / no-op / ошибка). Реальный wire-up —
// тонкая обёртка над incarnation.ReleaseApplyingOrphan (без изменения её
// сигнатуры — реюз as-is, ADR-027 amend (m-S1)).
type orphanApplyingReleaser interface {
	ReleaseApplyingOrphan(ctx context.Context, name, orphanApplyID, historyID string) error
}

// orphanApplyingPresence — узкая поверхность presence-чека keeper-инстанса в
// Conclave (redis.InstanceAlive). Интерфейс держит правило тестируемым без
// поднятия Redis. Реальный wire-up — обёртка над redis.InstanceAlive поверх
// общего redis.Client.
type orphanApplyingPresence interface {
	InstanceAlive(ctx context.Context, kid string) (bool, error)
}

// orphanApplyingQuerier — узкая поверхность pgxpool.Pool для фазы 1 (SELECT
// кандидатов). Сужение позволяет fake в unit-тестах (паттерн voyagesQuerier).
type orphanApplyingQuerier interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}

// OrphanApplyingReconciler — реализация Reaper-правила `reconcile_orphan_applying`
// (ADR-027 amend (m)): снимает осиротевший applying-lock инкарнации, оставшийся
// от ПРЯМОГО (standalone, не под Voyage) scenario-run крашнувшегося Keeper-
// владельца. Voyage-путь закрыт amend (l) через back-link voyage_targets; у
// прямого run-а back-link нет — этот шов закрывает его симметрично, но детектит
// orphan по epoch-колонкам incarnation (applying_by_kid/applying_since/
// applying_apply_id, миграция 082).
//
// Трёхфазно за один Run:
//   - (1) SQL-кандидаты — stale applying-строки с НЕпустым epoch
//     (orphanApplyingCandidatesSQL);
//   - (2) presence — для каждого кандидата InstanceAlive(applying_by_kid). Жив →
//     skip (прогон реально идёт). Мёртв → фаза 3. Ошибка presence-чека →
//     fail-safe skip (неизвестно ⇒ НЕ реклеймить, чтобы не сорвать живой прогон
//     при флапе Redis);
//   - (3) снятие — ReleaseApplyingOrphan as-is (FENCING-1 no-live-rival +
//     single-winner CAS applying→ready внутри). ErrOrphanLockNotReleased /
//     ErrIncarnationNotFound → no-op (гонка с честным финалом / снос инкарнации).
//
// presence-death (фаза 2) — прямое доказательство смерти владельца в Conclave,
// замена Voyage-FENCING-3 VerifyOwnership (у standalone нет Voyage-claim,
// voyage.VerifyOwnership НЕ зовётся).
//
// Per-row audit (reaper.reconcile_orphan_applying.executed) на КАЖДОЕ успешное
// снятие. audit nil-safe (dev-без-audit живёт). Сигнатура Run совместима с
// runDurationRule-вызовом Runner-а.
type OrphanApplyingReconciler struct {
	pool     orphanApplyingQuerier
	presence orphanApplyingPresence
	releaser orphanApplyingReleaser
	audit    audit.Writer
	logger   *slog.Logger
}

// poolReleaser — production-обёртка incarnation.ReleaseApplyingOrphan поверх
// *pgxpool.Pool. Реюз as-is — сигнатура package-функции НЕ меняется.
type poolReleaser struct {
	pool *pgxpool.Pool
}

func (r poolReleaser) ReleaseApplyingOrphan(ctx context.Context, name, orphanApplyID, historyID string) error {
	return incarnation.ReleaseApplyingOrphan(ctx, r.pool, name, orphanApplyID, historyID)
}

// clientPresence — production-обёртка redis.InstanceAlive поверх redis.Client.
type clientPresence struct {
	client *redis.Client
}

func (p clientPresence) InstanceAlive(ctx context.Context, kid string) (bool, error) {
	return redis.InstanceAlive(ctx, p.client, kid)
}

// NewOrphanApplyingReconciler конструирует правило для production-wire-up-а
// (daemon.setupReaper). pool/client обязательны; audit nil-safe (эмит skip-ается),
// logger nil-safe (warn-ы подавляются). Презенс и releaser оборачивают общие
// redis.Client / *pgxpool.Pool — incarnation.ReleaseApplyingOrphan реюзится без
// изменения сигнатуры.
func NewOrphanApplyingReconciler(pool *pgxpool.Pool, client *redis.Client, aud audit.Writer, logger *slog.Logger) *OrphanApplyingReconciler {
	return &OrphanApplyingReconciler{
		pool:     pool,
		presence: clientPresence{client: client},
		releaser: poolReleaser{pool: pool},
		audit:    aud,
		logger:   logger,
	}
}

// newOrphanApplyingReconcilerForTest — внутренний конструктор для unit-тестов
// (fake presence / releaser / querier без поднятия PG+Redis).
func newOrphanApplyingReconcilerForTest(pool orphanApplyingQuerier, presence orphanApplyingPresence, releaser orphanApplyingReleaser, aud audit.Writer, logger *slog.Logger) *OrphanApplyingReconciler {
	return &OrphanApplyingReconciler{
		pool:     pool,
		presence: presence,
		releaser: releaser,
		audit:    aud,
		logger:   logger,
	}
}

// orphanApplyingCandidate — снимок одной stale applying-строки (фаза 1).
type orphanApplyingCandidate struct {
	name    string
	prevKID string
	applyID string
}

// Run выполняет одну итерацию правила: собирает stale-кандидатов, для мёртвых
// (presence) владельцев снимает applying-lock и эмитит per-row audit. Возвращает
// (affected, err): affected — число РЕАЛЬНО снятых lock-ов (presence-skip /
// defensive-skip / honest-terminal-гонка в счёт не идут; callers сложат в
// keeper_reaper_*-метрики).
//
// Сигнатура совместима с runDurationRule (`(ctx, duration, batch) → (int64, error)`):
// staleAfter — `stale_after` правила (cutoff = NOW()-staleAfter), batchSize
// игнорируется (число applying-строк на кластер — единицы/десятки, режем не
// батчем, а partial-индексом).
//
// nil-presence — test-affordance: в prod NewOrphanApplyingReconciler ВСЕГДА
// оборачивает non-nil clientPresence (reaper-ветка гейтится non-nil Redis в
// daemon.go), поэтому эта ветка недостижима в проде. Реальный presence-gate
// против недоступного Redis — на уровне InstanceAlive: ошибка presence-чека ⇒
// fail-safe skip кандидата (reconcileOne), а не no-op всего правила.
func (r *OrphanApplyingReconciler) Run(ctx context.Context, staleAfter time.Duration, _ int) (int64, error) {
	if r.pool == nil {
		return 0, fmt.Errorf("reaper.reconcile_orphan_applying: pool is nil")
	}
	if r.presence == nil {
		// Test-affordance: недостижимо в проде (см. docstring Run). Реальный
		// presence-gate — InstanceAlive→error fail-safe skip в reconcileOne, не
		// эта ветка. Без presence-клиента правило не может доказать смерть
		// владельца — graceful no-op (НЕ ошибка).
		if r.logger != nil {
			r.logger.Info("reaper.reconcile_orphan_applying: пропущено — presence-клиент не задан (test-affordance)")
		}
		return 0, nil
	}

	cutoff := time.Now().Add(-staleAfter)
	rows, err := r.pool.Query(ctx, orphanApplyingCandidatesSQL, cutoff)
	if err != nil {
		return 0, fmt.Errorf("reaper.reconcile_orphan_applying: query candidates: %w", err)
	}

	// Считываем кандидатов ДО presence/release I/O — не держим курсор открытым во
	// время EXISTS-чека Redis и CAS-tx PG (паттерн VoyageReclaimer).
	var candidates []orphanApplyingCandidate
	for rows.Next() {
		var (
			name    string
			prevKID *string
			applyID *string
		)
		if scanErr := rows.Scan(&name, &prevKID, &applyID); scanErr != nil {
			rows.Close()
			return 0, fmt.Errorf("reaper.reconcile_orphan_applying: scan: %w", scanErr)
		}
		c := orphanApplyingCandidate{name: name}
		if prevKID != nil {
			c.prevKID = *prevKID
		}
		if applyID != nil {
			c.applyID = *applyID
		}
		candidates = append(candidates, c)
	}
	if rowsErr := rows.Err(); rowsErr != nil {
		rows.Close()
		return 0, fmt.Errorf("reaper.reconcile_orphan_applying: rows: %w", rowsErr)
	}
	rows.Close()

	var affected int64
	for _, c := range candidates {
		if r.reconcileOne(ctx, c) {
			affected++
		}
	}
	return affected, nil
}

// reconcileOne обрабатывает одного кандидата: presence-чек → (мёртв) снятие.
// Возвращает true ТОЛЬКО при реальном снятии lock-а (для affected-счётчика).
func (r *OrphanApplyingReconciler) reconcileOne(ctx context.Context, c orphanApplyingCandidate) bool {
	// defensive-skip: пустой epoch недостижим при корректном lockRun (фильтр SQL
	// уже отсёк applying_by_kid IS NULL; applying_apply_id пишется атомарно с ним),
	// но fail-safe от legacy / ручных правок строки — НЕ снимаем без полного epoch.
	if c.prevKID == "" || c.applyID == "" {
		if r.logger != nil {
			r.logger.Warn("reaper.reconcile_orphan_applying: defensive-skip — неполный epoch",
				slog.String("incarnation", c.name),
				slog.String("prev_kid", c.prevKID),
				slog.String("apply_id", c.applyID))
		}
		return false
	}

	// Фаза 2 — presence: жив ли владелец lock-а в Conclave.
	alive, err := r.presence.InstanceAlive(ctx, c.prevKID)
	if err != nil {
		// fail-safe: presence неизвестен (флап Redis) → НЕ объявляем мёртвым, НЕ
		// реклеймим (живой прогон может идти). Warn для триажа.
		if r.logger != nil {
			r.logger.Warn("reaper.reconcile_orphan_applying: presence-чек провален — skip (fail-safe)",
				slog.String("incarnation", c.name),
				slog.String("prev_kid", c.prevKID),
				slog.Any("error", err))
		}
		return false
	}
	if alive {
		// Владелец жив — прогон реально идёт, lock НЕ осиротел.
		return false
	}

	// Фаза 3 — снятие: владелец мёртв в Conclave. ReleaseApplyingOrphan as-is
	// (FENCING-1 no-live-rival + single-winner CAS внутри). historyID генерим
	// здесь (идентично Voyage-адаптеру).
	historyID := audit.NewULID()
	if relErr := r.releaser.ReleaseApplyingOrphan(ctx, c.name, c.applyID, historyID); relErr != nil {
		switch {
		case errors.Is(relErr, incarnation.ErrOrphanLockNotReleased):
			// no-op: честный финал прошлого владельца уже вывел строку из applying
			// (single-winner) ЛИБО живой rival держит чужой apply_id (FENCING-1).
			return false
		case errors.Is(relErr, incarnation.ErrIncarnationNotFound):
			// Инкарнация снесена между фазой 1 и снятием — нечего снимать.
			return false
		default:
			if r.logger != nil {
				r.logger.Error("reaper.reconcile_orphan_applying: снятие lock-а провалено",
					slog.String("incarnation", c.name),
					slog.String("prev_kid", c.prevKID),
					slog.Any("error", relErr))
			}
			return false
		}
	}

	r.emitExecuted(ctx, c)
	if r.logger != nil {
		r.logger.Info("reaper.reconcile_orphan_applying: осиротевший applying-lock снят",
			slog.String("incarnation", c.name),
			slog.String("prev_kid", c.prevKID),
			slog.String("apply_id", c.applyID))
	}
	return true
}

// emitExecuted пишет reaper.reconcile_orphan_applying.executed (ADR-027 amend (m),
// область reaper.*). source=keeper_internal, archon_aid="" (NULL). nil-safe.
func (r *OrphanApplyingReconciler) emitExecuted(ctx context.Context, c orphanApplyingCandidate) {
	if r.audit == nil {
		return
	}
	ev := &audit.Event{
		EventType: audit.EventReconcileOrphanApplyingExecuted,
		Source:    audit.SourceKeeperInternal,
		Payload: map[string]any{
			"incarnation": c.name,
			"prev_kid":    c.prevKID,
			"apply_id":    c.applyID,
		},
	}
	if err := r.audit.Write(ctx, ev); err != nil && r.logger != nil {
		r.logger.Warn("reaper.reconcile_orphan_applying: audit write failed",
			slog.String("incarnation", c.name), slog.Any("error", err))
	}
}
