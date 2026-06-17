package reaper

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/souls-guild/soul-stack/shared/audit"
)

// voyagesQuerier — узкая поверхность pgxpool.Pool, нужная правилу
// `reclaim_voyages`: один Query (UPDATE ... RETURNING) для per-row audit.
// Сужение позволяет fake в unit-тестах без поднятия Postgres; реальный
// *pgxpool.Pool удовлетворяет автоматически (паттерн errandsExecer).
type voyagesQuerier interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}

// reclaimVoyagesSQL — recovery-скан протухших Voyage-claim-ов (ADR-043 S4,
// docs/keeper/reaper.md → §reclaim_voyages). Возвращает Voyage со статусом
// `running` и истёкшим `claim_expires_at` обратно в `pending` для пере-claim
// другим Keeper-инстансом; `attempt++` (fencing-epoch, parity
// reclaim_apply_runs). `current_batch_index` сбрасывается в 0 — reclaim
// переисполняет прогон с нуля (idempotent re-apply executor-а с legs[0]);
// resume-from-batch (продолжение с N) — отдельный эпик.
//
// Reclaim возвращает в `pending` (НЕ в исходный `scheduled`): к моменту running
// schedule_at заведомо наступил, и строка должна быть немедленно подбираема.
//
// Использует partial-индекс `voyages_claim_scan_idx` (миграция 059,
// `WHERE status = 'running'`). Параметр `lease` правила в предикат НЕ входит:
// lease уже зашит в `claim_expires_at` при захвате через voyage.ClaimNext —
// предикат сравнивает `claim_expires_at < NOW()` напрямую (parity
// reclaim_apply_runs). FOR UPDATE SKIP LOCKED во вложенном
// SELECT защищает от гонки с конкурентным claim/renew.
//
// CTE `picked` снимает дореклеймное `last_renewed_at` (audit нуждается в
// значении ДО сброса в NULL) + защищает SKIP LOCKED; `UPDATE ... RETURNING`
// отдаёт новый `attempt`. Итог Query: voyage_id + last_renewed_at(до) +
// attempt(после) — payload per-row `voyage.reclaimed` (kind-agnostic, ADR-043
// A3): SQL не разбирает kind, событие единое для scenario/command.
const reclaimVoyagesSQL = `
WITH picked AS (
    SELECT voyage_id, last_renewed_at
    FROM voyages
    WHERE status = 'running' AND claim_expires_at < NOW()
    FOR UPDATE SKIP LOCKED
), updated AS (
    UPDATE voyages v
    SET status           = 'pending',
        claimed_by_kid   = NULL,
        last_renewed_at  = NULL,
        claim_expires_at = NULL,
        attempt          = attempt + 1,
        current_batch_index = 0
    FROM picked p
    WHERE v.voyage_id = p.voyage_id
    RETURNING v.voyage_id, v.attempt
)
SELECT u.voyage_id, p.last_renewed_at, u.attempt
FROM updated u
JOIN picked p ON p.voyage_id = u.voyage_id
`

// VoyageReclaimer — реализация правила `reclaim_voyages` (ADR-043 S4,
// docs/keeper/reaper.md). Один батч-проход = один Query (UPDATE ... RETURNING)
// по partial-индексу `voyages_claim_scan_idx`. Сигнатура Run совместима с
// runDurationRule-вызовом Runner-а.
//
// Per-row audit (ADR-043 A3): на каждую реклеймнутую строку эмитится
// `voyage.reclaimed` (область `voyage.*`, kind-agnostic — SQL не разбирает kind).
// audit nil-safe: dev-без-audit живёт, эмит только при audit != nil.
//
// Параметр `lease` в предикат НЕ входит (см. docstring [reclaimVoyagesSQL]),
// `batchSize` тоже: UPDATE режет одним statement-ом. Аргументы сохранены в
// сигнатуре для совместимости с общим duration-runner-ом.
type VoyageReclaimer struct {
	pool   voyagesQuerier
	audit  audit.Writer
	logger *slog.Logger
}

// NewVoyageReclaimer конструирует reclaimer. logger nil-safe (warn-ы
// подавляются), audit nil-safe (эмит skip-ается). Публичный конструктор
// фиксирует *pgxpool.Pool, чтобы caller-ы не цеплялись за расширение интерфейса
// (паттерн NewErrandsPurger).
func NewVoyageReclaimer(pool *pgxpool.Pool, audit audit.Writer, logger *slog.Logger) *VoyageReclaimer {
	return &VoyageReclaimer{pool: pool, audit: audit, logger: logger}
}

// newVoyageReclaimerFromQuerier — внутренний конструктор для unit-тестов.
func newVoyageReclaimerFromQuerier(pool voyagesQuerier, audit audit.Writer, logger *slog.Logger) *VoyageReclaimer {
	return &VoyageReclaimer{pool: pool, audit: audit, logger: logger}
}

// Run выполняет одну итерацию правила: возвращает протухшие running-Voyage-и
// обратно в pending и эмитит per-row `voyage.reclaimed`. Возвращает
// (affected, err): affected — число перезахваченных строк (callers сложат в
// keeper_reaper_*-метрики).
//
// Сигнатура совместима с runDurationRule (`(ctx, duration, batch) → (int64, error)`),
// аргументы lease/batchSize игнорируются (см. doc-comment типа).
func (r *VoyageReclaimer) Run(ctx context.Context, _ time.Duration, _ int) (int64, error) {
	if r.pool == nil {
		return 0, fmt.Errorf("reaper.reclaim_voyages: pool is nil")
	}
	rows, err := r.pool.Query(ctx, reclaimVoyagesSQL)
	if err != nil {
		return 0, fmt.Errorf("reaper.reclaim_voyages: %w", err)
	}
	defer rows.Close()

	var (
		reclaimed []reclaimedVoyage
		affected  int64
	)
	for rows.Next() {
		var (
			voyageID    string
			lastRenewed *time.Time
			attempt     int
		)
		if scanErr := rows.Scan(&voyageID, &lastRenewed, &attempt); scanErr != nil {
			return affected, fmt.Errorf("reaper.reclaim_voyages: scan: %w", scanErr)
		}
		affected++
		reclaimed = append(reclaimed, reclaimedVoyage{
			voyageID:    voyageID,
			lastRenewed: lastRenewed,
			attempt:     attempt,
		})
	}
	if rowsErr := rows.Err(); rowsErr != nil {
		return affected, fmt.Errorf("reaper.reclaim_voyages: rows: %w", rowsErr)
	}
	rows.Close()

	// Per-row audit ПОСЛЕ закрытия rows (не держим курсор открытым во время
	// I/O-эмита). Best-effort: ошибка одного события не отменяет реклейм.
	for _, rv := range reclaimed {
		r.emitReclaimed(ctx, rv)
	}
	return affected, nil
}

// reclaimedVoyage — снимок одной реклеймнутой строки для per-row audit.
type reclaimedVoyage struct {
	voyageID    string
	lastRenewed *time.Time
	attempt     int
}

// emitReclaimed пишет `voyage.reclaimed` (ADR-043 A3, область `voyage.*`,
// kind-agnostic). source=keeper_internal, archon_aid="" (NULL). nil-safe.
func (r *VoyageReclaimer) emitReclaimed(ctx context.Context, rv reclaimedVoyage) {
	if r.audit == nil {
		return
	}
	payload := map[string]any{
		"voyage_id":     rv.voyageID,
		"attempt_after": rv.attempt,
	}
	if rv.lastRenewed != nil {
		payload["last_renewed_at"] = rv.lastRenewed.UTC()
	}
	ev := &audit.Event{
		EventType: audit.EventVoyageReclaimed,
		Source:    audit.SourceKeeperInternal,
		Payload:   payload,
	}
	if err := r.audit.Write(ctx, ev); err != nil && r.logger != nil {
		r.logger.Warn("reaper.reclaim_voyages: audit write failed",
			slog.String("voyage_id", rv.voyageID), slog.Any("error", err))
	}
}
