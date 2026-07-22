-- 055_create_tides.up.sql
--
-- ADR-040 amendment 2026-05-27 -> W-1 schema: Tide PG table + back-link to apply_runs.
--
-- Tide - a top-level entity for invocation-time scope chunking: a single Tide
-- describes a mass rollout of a scenario over a large target, split into N
-- sequential Surge waves of fixed size surge_size. Each Surge is
-- one apply_run (back-link apply_runs.tide_id + surge_index).
--
-- Failover-resilient via PG-based claim+lease (parity with the Ward-claim from
-- ADR-027): pending -> claimed_by_kid + claim_expires_at -> running; a stale
-- claim is returned by the Reaper rule `reclaim_tides` back to pending for
-- re-claim by another Keeper instance.
--
-- CHECK invariants:
--   * tides_running_claim_consistency: running => claim fields NOT NULL.
--   * tides_surge_index_within_total:  current_surge_index <= total_surges
--     (=total - all Surges completed, the run is finalized).
--
-- FK:
--   * started_by_aid -> operators(aid) (NOT NULL, without ON DELETE - a Tide
--     is always initiated by a specific Archon; parity with apply_runs.started_by_aid
--     using ON DELETE SET NULL doesn't fit: NOT NULL forbids SET NULL).

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

-- Recovery scan: only active running rows with an expired claim (Reaper `reclaim_tides`).
CREATE INDEX tides_claim_scan_idx
    ON tides (claim_expires_at)
    WHERE status = 'running' AND claim_expires_at IS NOT NULL;

-- Pickup pending Tides by FIFO started_at (TideWorker.ClaimNext, FOR UPDATE SKIP LOCKED).
CREATE INDEX tides_pending_pickup_idx
    ON tides (started_at)
    WHERE status = 'pending';

-- Back-link apply_runs -> tides. nullable: single-run apply_runs (without a Tide)
-- stay with tide_id=NULL; Tide-Surge - (tide_id, surge_index). We don't add an FK
-- here (ADR-040 amendment 2026-05-27, soft-link parity with incarnation_name):
-- deleting a Tide does not cascade to apply_runs.
ALTER TABLE apply_runs ADD COLUMN IF NOT EXISTS tide_id TEXT;
ALTER TABLE apply_runs ADD COLUMN IF NOT EXISTS surge_index INT;
CREATE INDEX IF NOT EXISTS apply_runs_tide_idx ON apply_runs (tide_id) WHERE tide_id IS NOT NULL;

COMMENT ON TABLE tides IS
    'Registry of Tide runs (top-level invocation-time chunking, ADR-040 amendment 2026-05-27, W-1). PG-based claim+lease for failover resilience: pending->running->terminal; a stale claim is returned to pending by the reclaim_tides Reaper rule.';
