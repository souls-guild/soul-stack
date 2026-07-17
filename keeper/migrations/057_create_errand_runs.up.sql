-- 057_create_errand_runs.up.sql
--
-- ADR-041 → E6-1 schema: ErrandRun PG table + back-link to errands.
--
-- ErrandRun - top-level multi-target invocation-time entity for mass
-- pull-ad-hoc exec of a single module (`POST /v1/errand-runs`, MCP
-- `keeper.errand-run.start`). One ErrandRun = N individual Errands (parity
-- ADR-033 single-host), run in parallel with a semaphore-cap `concurrency`
-- over a resolved snapshot of the target. Difference from Tide (ADR-040): no scenario /
-- incarnation / surge waves - this is a flat fan-out of a single module.
--
-- Failover-resilient via PG-based claim+lease (parity Tide / Ward-claim
-- ADR-027): pending → claimed_by_kid + claim_expires_at → running; a stale
-- claim is returned by a Reaper rule (rolled out in E6-x) back to pending for
-- re-claim by another Keeper instance.
--
-- CHECK invariants:
--   * errand_runs_status_valid: closed-set of 6 terminals
--     (pending/running/succeeded/failed/partial_failed/cancelled).
--   * errand_runs_on_failure_valid: abort | continue.
--   * errand_runs_concurrency_positive: 1 ≤ concurrency ≤ 500
--     (the upper cap - protection against exhausting the Keeper's semaphore capacity).
--   * errand_runs_total_positive: total_errands ≥ 1
--     (an empty target is rejected at the resolve stage before INSERT).
--   * errand_runs_done_bounds: 0 ≤ current_done ≤ total_errands
--     (counter of terminal Errands within the run).
--   * errand_runs_attempt_positive: attempt ≥ 0.
--   * errand_runs_running_claim_consistency:
--     running ⇒ claimed_by_kid IS NOT NULL AND claim_expires_at IS NOT NULL.
--   * errand_runs_terminal_finished_at:
--     terminal status ⇒ finished_at IS NOT NULL.
--
-- FK:
--   * started_by_aid → operators(aid) (NOT NULL, without ON DELETE - parity
--     with tides; ErrandRun is always initiated by a specific Archon, revoked
--     stays in the registry).
--
-- Back-link errands.errand_run_id (NULLABLE, FK CASCADE):
--   * single-host pull-ad-hoc `POST /v1/souls/{sid}/exec` (ADR-033)
--     keeps errand_run_id IS NULL - the Errand lives on its own.
--   * multi-target ErrandRun creates N Errands with errand_run_id =
--     <run_id>; deleting the ErrandRun cascades to remove its Errands.

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

-- Pick up pending ErrandRuns by FIFO started_at
-- (ErrandRunWorker.ClaimNext, FOR UPDATE SKIP LOCKED — E6-x).
CREATE INDEX errand_runs_pending_pickup_idx
    ON errand_runs (started_at)
    WHERE status = 'pending';

-- Recovery scan: only active running rows with an expired claim
-- (Reaper rule reclaim_errand_runs - E6-x).
CREATE INDEX errand_runs_claim_scan_idx
    ON errand_runs (claim_expires_at)
    WHERE status = 'running';

-- Back-link errands → errand_runs. NULLABLE: single-host pull-ad-hoc
-- (POST /v1/souls/{sid}/exec, ADR-033) stays with errand_run_id=NULL.
-- FK CASCADE: deleting the ErrandRun removes associated Errands.
ALTER TABLE errands ADD COLUMN errand_run_id TEXT;
ALTER TABLE errands ADD CONSTRAINT errands_errand_run_id_fkey
    FOREIGN KEY (errand_run_id) REFERENCES errand_runs (errand_run_id) ON DELETE CASCADE;

CREATE INDEX errands_errand_run_id_idx
    ON errands (errand_run_id)
    WHERE errand_run_id IS NOT NULL;

COMMENT ON TABLE errand_runs IS
    'Registry of ErrandRun runs (top-level multi-target pull-ad-hoc invocation, ADR-041). PG-based claim+lease for failover-resilience: pending->running->terminal; a stale claim is returned to pending by the Reaper.';
