-- 059_create_voyages.up.sql
--
-- ADR-043 -> S1 schema: foundation PG tables for Voyage (alongside the old
-- tides / errand_runs / apply_runs, which keep working until S7).
--
-- Voyage is a unified batch run absorbing Tide (kind=scenario) +
-- ErrandRun (kind=command). The unit of a batch is a Leg (one "leg of the
-- journey"), identified by voyage_targets.batch_index. Table names are
-- derived from the locked Voyage/Leg entity (the ADR-043 sketch
-- `runs`/`run_targets` was refined to `voyages`/`voyage_targets`, so the
-- name carries the entity instead of being the generic `runs`).
--
-- Failover-resilient via PG-based claim+lease (parity with Tide / ErrandRun /
-- Ward-claim ADR-027(d)): pending -> claimed_by_kid + claim_expires_at ->
-- running; a stale claim is returned to pending by the Reaper rule for
-- re-claim by another Keeper instance (fan-out is post-S1). attempt++ on
-- every claim -- a fencing epoch.
--
-- In S1 the worker does NOT execute real work (config-gated OFF by default):
-- the tables and the claim-loop exist as a foundation, real execution of
-- scenario/command is wired up in S2/S3.
--
-- CHECK invariants for voyages:
--   * voyages_kind_valid:           scenario | command.
--   * voyages_status_valid:         closed-set of 7 statuses (+ scheduled for
--                                   deferred start, S4).
--   * voyages_on_failure_valid:     abort | continue.
--   * voyages_kind_payload_consistency: kind=scenario => scenario_name NOT NULL;
--                                   kind=command => module NOT NULL.
--   * voyages_running_claim_consistency: running => claim fields NOT NULL.
--   * voyages_terminal_finished_at: terminal status => finished_at NOT NULL.
--   * voyages_batch_index_within_total: 0 <= current_batch_index <= total_batches.
--   * voyages_attempt_non_negative / voyages_batch_size_positive /
--     voyages_concurrency_positive -- sane bounds.
--
-- FK:
--   * started_by_aid -> operators(aid) (NOT NULL, no ON DELETE -- parity
--     with tides/errand_runs; a Voyage is always initiated by a specific
--     Archon).

CREATE TABLE voyages (
    voyage_id              TEXT        PRIMARY KEY,
    kind                   TEXT        NOT NULL,
    scenario_name          TEXT,
    module                 TEXT,
    input                  JSONB       NOT NULL DEFAULT '{}',
    target_resolved        JSONB       NOT NULL,
    target_origin          JSONB,
    batch_size             INT,
    concurrency            INT,
    dry_run                BOOLEAN     NOT NULL DEFAULT false,
    schedule_at            TIMESTAMPTZ,
    inter_batch_interval   INTERVAL,
    on_failure             TEXT,
    total_batches          INT         NOT NULL DEFAULT 0,
    current_batch_index    INT         NOT NULL DEFAULT 0,
    status                 TEXT        NOT NULL,
    claimed_by_kid         TEXT,
    last_renewed_at        TIMESTAMPTZ,
    claim_expires_at       TIMESTAMPTZ,
    attempt                INT         NOT NULL DEFAULT 0,
    started_by_aid         TEXT        NOT NULL,
    created_at             TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    started_at             TIMESTAMPTZ,
    finished_at            TIMESTAMPTZ,
    summary                JSONB,

    CONSTRAINT voyages_kind_valid
        CHECK (kind IN ('scenario', 'command')),
    CONSTRAINT voyages_status_valid
        CHECK (status IN ('scheduled', 'pending', 'running', 'succeeded', 'failed', 'partial_failed', 'cancelled')),
    CONSTRAINT voyages_on_failure_valid
        CHECK (on_failure IS NULL OR on_failure IN ('abort', 'continue')),
    CONSTRAINT voyages_kind_payload_consistency
        CHECK (
            (kind = 'scenario' AND scenario_name IS NOT NULL)
            OR (kind = 'command' AND module IS NOT NULL)
        ),
    CONSTRAINT voyages_batch_size_positive
        CHECK (batch_size IS NULL OR batch_size > 0),
    CONSTRAINT voyages_concurrency_positive
        CHECK (concurrency IS NULL OR concurrency > 0),
    CONSTRAINT voyages_total_batches_non_negative
        CHECK (total_batches >= 0),
    CONSTRAINT voyages_batch_index_within_total
        CHECK (current_batch_index >= 0 AND current_batch_index <= total_batches),
    CONSTRAINT voyages_attempt_non_negative
        CHECK (attempt >= 0),
    CONSTRAINT voyages_running_claim_consistency
        CHECK (
            (status <> 'running')
            OR (claimed_by_kid IS NOT NULL AND claim_expires_at IS NOT NULL)
        ),
    CONSTRAINT voyages_terminal_finished_at
        CHECK (
            (status NOT IN ('succeeded', 'failed', 'partial_failed', 'cancelled'))
            OR (finished_at IS NOT NULL)
        ),
    CONSTRAINT voyages_started_by_aid_fk
        FOREIGN KEY (started_by_aid) REFERENCES operators (aid)
);

-- Picking up pending Voyages by FIFO created_at
-- (VoyageWorker.ClaimNext, FOR UPDATE SKIP LOCKED).
CREATE INDEX voyages_pending_pickup_idx
    ON voyages (created_at)
    WHERE status = 'pending';

-- Recovery scan: only active running rows with an expired claim
-- (Reaper rule reclaim_voyages -- post-S1).
CREATE INDEX voyages_claim_scan_idx
    ON voyages (claim_expires_at)
    WHERE status = 'running';

-- voyage_targets -- the units of a run (Leg split). For kind=scenario
-- target_kind='incarnation' + back-link apply_id to apply_runs (per-incarnation
-- scenario-run); for kind=command target_kind='sid' + back-link errand_id to
-- errands (per-host exec). Gives a unified All-runs view with two-level drill (S5).
--
-- CHECK invariants for voyage_targets:
--   * voyage_targets_target_kind_valid: incarnation | sid.
--   * voyage_targets_status_valid: closed-set (awaiting/running/succeeded/
--     failed/cancelled/no_match -- the last one for targets that didn't match).
--   * voyage_targets_batch_index_non_negative: batch_index >= 0.
--
-- PK (voyage_id, target_kind, target_id) -- one target is unique within a run.
-- FK voyage_id -> voyages ON DELETE CASCADE: deleting a Voyage removes its targets.
CREATE TABLE voyage_targets (
    voyage_id     TEXT        NOT NULL,
    target_kind   TEXT        NOT NULL,
    target_id     TEXT        NOT NULL,
    batch_index   INT         NOT NULL,
    status        TEXT        NOT NULL DEFAULT 'awaiting',
    apply_id      TEXT,
    errand_id     TEXT,
    finished_at   TIMESTAMPTZ,

    CONSTRAINT voyage_targets_pkey
        PRIMARY KEY (voyage_id, target_kind, target_id),
    CONSTRAINT voyage_targets_target_kind_valid
        CHECK (target_kind IN ('incarnation', 'sid')),
    CONSTRAINT voyage_targets_status_valid
        CHECK (status IN ('awaiting', 'running', 'succeeded', 'failed', 'cancelled', 'no_match')),
    CONSTRAINT voyage_targets_batch_index_non_negative
        CHECK (batch_index >= 0),
    CONSTRAINT voyage_targets_voyage_fk
        FOREIGN KEY (voyage_id) REFERENCES voyages (voyage_id) ON DELETE CASCADE
);

-- Drill by Leg: units of a single batch_index in execution order.
CREATE INDEX voyage_targets_batch_idx
    ON voyage_targets (voyage_id, batch_index);

COMMENT ON TABLE voyages IS
    'Registry of Voyage runs (unified batch run, ADR-043, S1). Discriminator kind=scenario|command absorbs Tide+ErrandRun. PG-based claim+lease for failover resilience: pending->running->terminal; a stale claim is returned to pending by the Reaper. In S1 the worker is config-gated OFF (foundation without real execution).';

COMMENT ON TABLE voyage_targets IS
    'Units of a Voyage run (Leg split by batch_index, ADR-043, S1). target_kind=incarnation (kind=scenario, back-link apply_id) | sid (kind=command, back-link errand_id). Two-level drill for the All-runs view (S5).';
