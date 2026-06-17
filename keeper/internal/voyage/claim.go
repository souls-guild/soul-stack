package voyage

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/souls-guild/soul-stack/keeper/internal/pgutil"
)

// claimReturning — RETURNING-список ClaimNext. Совпадает по порядку с
// [selectColumns] (включая EXTRACT EPOCH для inter_batch_interval), чтобы общий
// [scanVoyage] читал захваченную строку.
const claimReturning = `
    voyage_id, kind, scenario_name, module, input,
    target_resolved, target_origin,
    batch_size, concurrency, batch_mode, dry_run,
    schedule_at, EXTRACT(EPOCH FROM inter_batch_interval)::float8, on_failure,
    total_batches, current_batch_index, status,
    claimed_by_kid, last_renewed_at, claim_expires_at, attempt,
    started_by_aid, created_at, started_at, finished_at, summary,
    batch_percent, fail_threshold, EXTRACT(EPOCH FROM inter_unit_interval)::float8, require_alive,
    cadence_id
`

// claimNextSQL — атомарный захват одного claimable Voyage. Идиома work-queue
// parity tide/errandrun.ClaimNext: внутренний SELECT FOR UPDATE SKIP LOCKED
// блокирует одну строку и пропускает заблокированные конкурентами — два
// VoyageWorker-а разных инстансов никогда не захватят один Voyage. Внешний
// UPDATE переводит claimable → running, проставляет владельца/lease, выставляет
// started_at при первом claim и инкрементит attempt (fencing-epoch ADR-027(g)).
//
// Claimable = `pending` ИЛИ (`scheduled` И schedule_at <= NOW()): отложенный
// старт (S4) становится подбираемым, как только наступило время. scheduled с
// schedule_at в будущем игнорируется (ждёт своего часа). После захвата статус
// всегда `running` — отдельной ветки по исходному статусу не требуется.
//
//	$1 kid   — KID Keeper-владельца
//	$2 lease — interval до claim_expires_at (NOW() + $2)
const claimNextSQL = `
UPDATE voyages AS v
SET status           = 'running',
    claimed_by_kid   = $1,
    last_renewed_at  = NOW(),
    claim_expires_at = NOW() + $2::interval,
    started_at       = COALESCE(v.started_at, NOW()),
    attempt          = v.attempt + 1
WHERE v.voyage_id = (
    SELECT c.voyage_id
    FROM voyages AS c
    WHERE c.status = 'pending'
       OR (c.status = 'scheduled' AND c.schedule_at <= NOW())
    ORDER BY c.created_at ASC
    FOR UPDATE SKIP LOCKED
    LIMIT 1
)
RETURNING ` + claimReturning

// ClaimNext атомарно захватывает один claimable Voyage: (pending ИЛИ
// scheduled-наступивший) → running, claim-поля + attempt+1 + started_at. FIFO
// по created_at. Возвращает nil без ошибки, если claimable-Voyage-ов нет
// (caller спит PollInterval и ретраит).
func ClaimNext(ctx context.Context, db ExecQueryRower, kid string, leaseTTL time.Duration) (*Voyage, error) {
	if kid == "" {
		return nil, fmt.Errorf("voyage: empty kid")
	}
	if leaseTTL <= 0 {
		return nil, fmt.Errorf("voyage: non-positive lease %s", leaseTTL)
	}

	row := db.QueryRow(ctx, claimNextSQL, kid, pgutil.Interval(leaseTTL))
	v, err := scanVoyage(row)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("voyage: claim next: %w", err)
	}
	return v, nil
}

const renewLeaseSQL = `
UPDATE voyages
SET claim_expires_at = NOW() + $3::interval,
    last_renewed_at  = NOW()
WHERE voyage_id      = $1
  AND claimed_by_kid = $2
  AND claim_expires_at > NOW()
RETURNING voyage_id
`

// RenewLease CAS-extends текущий lease. Условие UPDATE-а: строка владеется этим
// KID-ом И claim ещё не протух. 0 строк → lease уже не наш ([ErrLeaseLost]):
// протух → Reaper вернул в pending → другой Keeper подобрал. Caller (renewLoop)
// закрывает leaseLost-канал — executeVoyage graceful-exit-ит без финализации.
func RenewLease(ctx context.Context, db ExecQueryRower, id, kid string, leaseTTL time.Duration) error {
	if id == "" {
		return fmt.Errorf("voyage: empty voyage_id")
	}
	if kid == "" {
		return fmt.Errorf("voyage: empty kid")
	}
	if leaseTTL <= 0 {
		return fmt.Errorf("voyage: non-positive lease %s", leaseTTL)
	}

	var returned string
	err := db.QueryRow(ctx, renewLeaseSQL, id, kid, pgutil.Interval(leaseTTL)).Scan(&returned)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrLeaseLost
		}
		return fmt.Errorf("voyage: renew lease: %w", err)
	}
	return nil
}

const updateBatchProgressSQL = `
UPDATE voyages
SET current_batch_index = $4
WHERE voyage_id      = $1
  AND claimed_by_kid = $2
  AND attempt        = $3
`

// UpdateBatchProgress продвигает current_batch_index Voyage-а на число
// ЗАВЕРШЁННЫХ батчей (Leg-ов) — UI-индикатор «Batch N/total». Вызывается
// оркестратором после завершения каждого Leg-а с completedBatches = legIdx+1
// (прогрессия 0→1→…→total_batches; терминал == total_batches = 100%).
//
// Ownership-guard в WHERE (parity [RenewLease]/[VerifyOwnership]): пишем только в
// СВОЙ claim — claimed_by_kid + attempt-epoch. После Reaper-reclaim (attempt++,
// другой владелец) UPDATE не сматчится — 0 строк, чужой current_batch_index не
// затрагивается.
//
// Best-effort: 0 строк (lease потеряна / реклейм) и I/O-ошибка возвращаются
// caller-у, но прогресс — лишь подсказка для UI; источник правды о ходе прогона —
// voyage_targets. Caller логирует warn и продолжает Leg-цикл, не валит прогон.
func UpdateBatchProgress(ctx context.Context, db ExecQueryRower, id, kid string, attempt, completedBatches int) error {
	if id == "" {
		return fmt.Errorf("voyage: empty voyage_id")
	}
	if kid == "" {
		return fmt.Errorf("voyage: empty kid")
	}
	if _, err := db.Exec(ctx, updateBatchProgressSQL, id, kid, attempt, completedBatches); err != nil {
		return fmt.Errorf("voyage: update batch progress: %w", err)
	}
	return nil
}

const releaseLeaseSQL = `
UPDATE voyages
SET status           = 'pending',
    claimed_by_kid   = NULL,
    claim_expires_at = NULL,
    last_renewed_at  = NULL
WHERE voyage_id      = $1
  AND claimed_by_kid = $2
  AND status         = 'running'
`

// ReleaseLease добровольно снимает lease на graceful-shutdown VoyageWorker-а:
// running → pending для немедленного re-pickup другим Keeper-ом (без ожидания
// истечения lease через Reaper).
//
// WHERE сужено до status='running' + ownership: если строка уже terminal либо
// lease lost — UPDATE no-op-ит RowsAffected=0, caller трактует как нормальный
// exit (parity errandrun.ReleaseLease).
func ReleaseLease(ctx context.Context, db ExecQueryRower, id, kid string) error {
	if id == "" {
		return fmt.Errorf("voyage: empty voyage_id")
	}
	if kid == "" {
		return fmt.Errorf("voyage: empty kid")
	}
	if _, err := db.Exec(ctx, releaseLeaseSQL, id, kid); err != nil {
		return fmt.Errorf("voyage: release lease: %w", err)
	}
	return nil
}
