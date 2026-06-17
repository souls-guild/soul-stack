-- 057_create_errand_runs.up.sql
--
-- ADR-041 → E6-1 schema: ErrandRun PG-таблица + back-link на errands.
--
-- ErrandRun — top-level multi-target invocation-time entity для массового
-- pull-ad-hoc exec одиночного модуля (`POST /v1/errand-runs`, MCP
-- `keeper.errand-run.start`). Один ErrandRun = N единичных Errand-ов (parity
-- ADR-033 single-host), запускаемых параллельно с семафор-cap `concurrency`
-- по resolved-снапшоту таргета. Отличие от Tide (ADR-040): нет scenario /
-- incarnation / surge-волн — это плоский fan-out одиночного модуля.
--
-- Failover-resilient через PG-based claim+lease (parity Tide / Ward-claim
-- ADR-027): pending → claimed_by_kid + claim_expires_at → running; протухший
-- claim возвращается Reaper-правилом (тираж в E6-x) обратно в pending для
-- пере-claim другим Keeper-инстансом.
--
-- CHECK-инварианты:
--   * errand_runs_status_valid: closed-set 6 терминалов
--     (pending/running/succeeded/failed/partial_failed/cancelled).
--   * errand_runs_on_failure_valid: abort | continue.
--   * errand_runs_concurrency_positive: 1 ≤ concurrency ≤ 500
--     (верхний cap — защита от исчерпания semaphore-ёмкости Keeper-а).
--   * errand_runs_total_positive: total_errands ≥ 1
--     (пустой target отвергается на этапе resolve до INSERT).
--   * errand_runs_done_bounds: 0 ≤ current_done ≤ total_errands
--     (счётчик терминальных Errand-ов в рамках прогона).
--   * errand_runs_attempt_positive: attempt ≥ 0.
--   * errand_runs_running_claim_consistency:
--     running ⇒ claimed_by_kid IS NOT NULL AND claim_expires_at IS NOT NULL.
--   * errand_runs_terminal_finished_at:
--     terminal-статус ⇒ finished_at IS NOT NULL.
--
-- FK:
--   * started_by_aid → operators(aid) (NOT NULL, без ON DELETE — парность
--     tides; ErrandRun всегда инициируется конкретным Архонтом, revoked
--     остаётся в реестре).
--
-- Back-link errands.errand_run_id (NULLABLE, FK CASCADE):
--   * single-host pull-ad-hoc `POST /v1/souls/{sid}/exec` (ADR-033)
--     сохраняет errand_run_id IS NULL — Errand живёт сам по себе.
--   * multi-target ErrandRun создаёт N Errand-ов с errand_run_id =
--     <run_id>; удаление ErrandRun каскадно сносит свои Errand-ы.

CREATE TABLE errand_runs (
    errand_run_id          TEXT        PRIMARY KEY,
    module                 TEXT        NOT NULL,
    input                  JSONB       NOT NULL,
    target_resolved_souls  JSONB       NOT NULL,
    target_origin          JSONB,
    concurrency            INT         NOT NULL DEFAULT 50,
    on_failure             TEXT        NOT NULL DEFAULT 'continue',
    total_errands          INT         NOT NULL,
    current_done           INT         NOT NULL DEFAULT 0,
    status                 TEXT        NOT NULL DEFAULT 'pending',
    claimed_by_kid         TEXT,
    last_renewed_at        TIMESTAMPTZ,
    claim_expires_at       TIMESTAMPTZ,
    attempt                INT         NOT NULL DEFAULT 0,
    started_by_aid         TEXT        NOT NULL,
    started_at             TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    finished_at            TIMESTAMPTZ,
    summary                JSONB,

    CONSTRAINT errand_runs_status_valid
        CHECK (status IN ('pending', 'running', 'succeeded', 'failed', 'partial_failed', 'cancelled')),
    CONSTRAINT errand_runs_on_failure_valid
        CHECK (on_failure IN ('abort', 'continue')),
    CONSTRAINT errand_runs_concurrency_positive
        CHECK (concurrency >= 1 AND concurrency <= 500),
    CONSTRAINT errand_runs_total_positive
        CHECK (total_errands >= 1),
    CONSTRAINT errand_runs_done_bounds
        CHECK (current_done >= 0 AND current_done <= total_errands),
    CONSTRAINT errand_runs_attempt_positive
        CHECK (attempt >= 0),
    CONSTRAINT errand_runs_running_claim_consistency
        CHECK (
            (status <> 'running')
            OR (claimed_by_kid IS NOT NULL AND claim_expires_at IS NOT NULL)
        ),
    CONSTRAINT errand_runs_terminal_finished_at
        CHECK (
            (status NOT IN ('succeeded', 'failed', 'partial_failed', 'cancelled'))
            OR (finished_at IS NOT NULL)
        ),
    CONSTRAINT errand_runs_started_by_aid_fk
        FOREIGN KEY (started_by_aid) REFERENCES operators (aid)
);

-- Pickup pending ErrandRun-ов по FIFO started_at
-- (ErrandRunWorker.ClaimNext, FOR UPDATE SKIP LOCKED — E6-x).
CREATE INDEX errand_runs_pending_pickup_idx
    ON errand_runs (started_at)
    WHERE status = 'pending';

-- Recovery-скан: только активные running с истёкшим claim
-- (Reaper-правило reclaim_errand_runs — E6-x).
CREATE INDEX errand_runs_claim_scan_idx
    ON errand_runs (claim_expires_at)
    WHERE status = 'running';

-- Back-link errands → errand_runs. NULLABLE: single-host pull-ad-hoc
-- (POST /v1/souls/{sid}/exec, ADR-033) остаётся с errand_run_id=NULL.
-- FK CASCADE: удаление ErrandRun сносит ассоциированные Errand-ы.
ALTER TABLE errands ADD COLUMN errand_run_id TEXT;
ALTER TABLE errands ADD CONSTRAINT errands_errand_run_id_fkey
    FOREIGN KEY (errand_run_id) REFERENCES errand_runs (errand_run_id) ON DELETE CASCADE;

CREATE INDEX errands_errand_run_id_idx
    ON errands (errand_run_id)
    WHERE errand_run_id IS NOT NULL;

COMMENT ON TABLE errand_runs IS
    'Реестр ErrandRun-прогонов (top-level multi-target pull-ad-hoc invocation, ADR-041). PG-based claim+lease для failover-resilience: pending→running→terminal; протухший claim возвращается Reaper в pending.';
