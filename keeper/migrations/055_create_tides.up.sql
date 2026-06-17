-- 055_create_tides.up.sql
--
-- ADR-040 amendment 2026-05-27 → W-1 schema: Tide PG-таблица + back-link на apply_runs.
--
-- Tide — top-level entity для invocation-time scope chunking: один Tide
-- описывает массовый прогон scenario по большому target-у, разбитому на N
-- последовательных Surge-волн фиксированного размера surge_size. Каждый Surge —
-- один apply_run (back-link apply_runs.tide_id + surge_index).
--
-- Failover-resilient через PG-based claim+lease (parity Ward-claim из
-- ADR-027): pending → claimed_by_kid + claim_expires_at → running; протухший
-- claim возвращается Reaper-правилом `reclaim_tides` обратно в pending для
-- пере-claim другим Keeper-инстансом.
--
-- CHECK-инварианты:
--   * tides_running_claim_consistency: running ⇒ claim-поля NOT NULL.
--   * tides_surge_index_within_total:  current_surge_index ≤ total_surges
--     (=total — все Surge-и отработали, прогон финализирован).
--
-- FK:
--   * started_by_aid → operators(aid) (NOT NULL, без ON DELETE — Tide
--     всегда инициируется конкретным Архонтом; парность apply_runs.started_by_aid
--     с ON DELETE SET NULL не подходит: NOT NULL запрещает SET NULL).

CREATE TABLE tides (
    tide_id                TEXT PRIMARY KEY,
    incarnation_name       TEXT NOT NULL,
    scenario_name          TEXT NOT NULL,
    total_surges           INT NOT NULL,
    current_surge_index    INT NOT NULL DEFAULT 0,
    surge_size             INT NOT NULL,
    target_resolved_souls  JSONB NOT NULL,
    target_coven_override  JSONB,
    target_where_override  TEXT,
    concurrency_override   INT,
    on_surge_failure       TEXT NOT NULL,
    status                 TEXT NOT NULL,
    current_apply_id       TEXT,
    claimed_by_kid         TEXT,
    last_renewed_at        TIMESTAMPTZ,
    claim_expires_at       TIMESTAMPTZ,
    attempt                INT NOT NULL DEFAULT 0,
    started_by_aid         TEXT NOT NULL,
    started_at             TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    finished_at            TIMESTAMPTZ,
    summary                JSONB,

    CONSTRAINT tides_total_surges_positive
        CHECK (total_surges > 0),
    CONSTRAINT tides_current_surge_index_non_negative
        CHECK (current_surge_index >= 0),
    CONSTRAINT tides_surge_size_positive
        CHECK (surge_size > 0),
    CONSTRAINT tides_concurrency_override_positive
        CHECK (concurrency_override IS NULL OR concurrency_override > 0),
    CONSTRAINT tides_on_surge_failure_valid
        CHECK (on_surge_failure IN ('abort', 'continue')),
    CONSTRAINT tides_status_valid
        CHECK (status IN ('pending', 'running', 'succeeded', 'failed', 'partial_failed', 'cancelled')),
    CONSTRAINT tides_running_claim_consistency
        CHECK (
            (status = 'running' AND claimed_by_kid IS NOT NULL AND claim_expires_at IS NOT NULL)
            OR status <> 'running'
        ),
    CONSTRAINT tides_surge_index_within_total
        CHECK (current_surge_index <= total_surges),
    CONSTRAINT tides_started_by_aid_fk
        FOREIGN KEY (started_by_aid) REFERENCES operators (aid)
);

-- Recovery-скан: только активные running с истёкшим claim (Reaper `reclaim_tides`).
CREATE INDEX tides_claim_scan_idx
    ON tides (claim_expires_at)
    WHERE status = 'running' AND claim_expires_at IS NOT NULL;

-- Pickup pending Tides по FIFO started_at (TideWorker.ClaimNext, FOR UPDATE SKIP LOCKED).
CREATE INDEX tides_pending_pickup_idx
    ON tides (started_at)
    WHERE status = 'pending';

-- Back-link apply_runs → tides. nullable: single-run apply_runs (без Tide)
-- остаются с tide_id=NULL; Tide-Surge — (tide_id, surge_index). FK не ставим
-- здесь (ADR-040 amendment 2026-05-27, soft-link parity incarnation_name):
-- удаление Tide не каскадит на apply_runs.
ALTER TABLE apply_runs ADD COLUMN IF NOT EXISTS tide_id TEXT;
ALTER TABLE apply_runs ADD COLUMN IF NOT EXISTS surge_index INT;
CREATE INDEX IF NOT EXISTS apply_runs_tide_idx ON apply_runs (tide_id) WHERE tide_id IS NOT NULL;

COMMENT ON TABLE tides IS
    'Реестр Tide-прогонов (top-level invocation-time chunking, ADR-040 amendment 2026-05-27, W-1). PG-based claim+lease для failover-resilience: pending→running→terminal; протухший claim возвращается Reaper-правилом reclaim_tides в pending.';
