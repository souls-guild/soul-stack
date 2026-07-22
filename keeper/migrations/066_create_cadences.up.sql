-- 066_create_cadences.up.sql
--
-- ADR-046 -> S1 schema: Cadence - a schedule that spawns a regular
-- Voyage run on a timer. A separate entity in Postgres that outlives runs:
-- when a Cadence's time arrives, it spawns a NEW Voyage (Insert into voyages +
-- voyage_targets), preserving the Voyage invariant "one Voyage = one run".
--
-- On S1 - ONLY the table + back-link voyages.cadence_id + CRUD layer. The
-- scheduler (Reaper rule spawn_due_cadence, action `spawn`), next_run_at
-- recalculation (interval/cron), overlap_policy execution, and the API are
-- wired up in S2-S4.
--
-- Cadence stores the run "recipe" (the same set as VoyageCreateRequest,
-- ADR-043) + a repeat rule (interval XOR cron) + an overlap policy. Following
-- the voyages parity style (migration 059): nullable recipe fields with no
-- CHECK on payload consistency, since that is resolved by the orchestrator;
-- XOR invariants that "both NULL" cannot express via CHECK without false
-- rejections live on the CRUD validation side (parity with
-- voyages_batch_size/percent XOR in the handler).
--
-- CHECK invariants on cadences:
--   * cadences_schedule_kind_valid:    interval | cron.
--   * cadences_overlap_policy_valid:   skip | queue | parallel.
--   * cadences_kind_valid:             scenario | command.
--   * cadences_schedule_consistency:   schedule_kind=interval => interval_seconds
--     NOT NULL AND cron_expr NULL; schedule_kind=cron => cron_expr NOT NULL AND
--     interval_seconds NULL (exactly one of the two, depending on schedule kind).
--   * cadences_kind_payload_consistency: kind=scenario => scenario_name NOT NULL;
--     kind=command => module NOT NULL (parity with voyages).
--   * cadences_interval_seconds_positive / cadences_batch_size_positive /
--     cadences_concurrency_positive / cadences_batch_percent_range /
--     cadences_fail_threshold_positive - sane bounds (parity with voyages 059/065).
--
-- FK:
--   * created_by_aid -> operators(aid) (NOT NULL - a Cadence is always created
--     by a specific Archon; the spawn on tick runs under their identity, ADR-046 section 7).

CREATE TABLE cadences (
    id                     TEXT        PRIMARY KEY,
    name                   TEXT        NOT NULL,
    enabled                BOOLEAN     NOT NULL DEFAULT true,

    -- Repeat rule.
    schedule_kind          TEXT        NOT NULL,
    interval_seconds       INT,
    cron_expr              TEXT,
    overlap_policy         TEXT        NOT NULL,

    -- Run recipe (parity with voyages, migration 059 + 064 + 065).
    kind                   TEXT        NOT NULL,
    scenario_name          TEXT,
    module                 TEXT,
    target                 JSONB       NOT NULL,
    input                  JSONB       NOT NULL DEFAULT '{}',
    batch_mode             TEXT,
    batch_size             INT,
    batch_percent          INT,
    concurrency            INT,
    fail_threshold         INT,
    inter_batch_interval   INTERVAL,
    inter_unit_interval    INTERVAL,
    require_alive          BOOLEAN,
    on_failure             TEXT,

    -- Computed timings (scheduler - S2; on S1 CRUD writes/reads them as-is).
    next_run_at            TIMESTAMPTZ,
    last_run_at            TIMESTAMPTZ,

    created_by_aid         TEXT        NOT NULL,
    created_at             TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at             TIMESTAMPTZ NOT NULL DEFAULT NOW(),

    CONSTRAINT cadences_schedule_kind_valid
        CHECK (schedule_kind IN ('interval', 'cron')),
    CONSTRAINT cadences_overlap_policy_valid
        CHECK (overlap_policy IN ('skip', 'queue', 'parallel')),
    CONSTRAINT cadences_kind_valid
        CHECK (kind IN ('scenario', 'command')),
    CONSTRAINT cadences_schedule_consistency
        CHECK (
            (schedule_kind = 'interval' AND interval_seconds IS NOT NULL AND cron_expr IS NULL)
            OR (schedule_kind = 'cron' AND cron_expr IS NOT NULL AND interval_seconds IS NULL)
        ),
    CONSTRAINT cadences_kind_payload_consistency
        CHECK (
            (kind = 'scenario' AND scenario_name IS NOT NULL)
            OR (kind = 'command' AND module IS NOT NULL)
        ),
    CONSTRAINT cadences_interval_seconds_positive
        CHECK (interval_seconds IS NULL OR interval_seconds > 0),
    CONSTRAINT cadences_batch_mode_valid
        CHECK (batch_mode IS NULL OR batch_mode IN ('barrier', 'window')),
    CONSTRAINT cadences_batch_size_positive
        CHECK (batch_size IS NULL OR batch_size > 0),
    CONSTRAINT cadences_batch_percent_range
        CHECK (batch_percent IS NULL OR (batch_percent >= 1 AND batch_percent <= 100)),
    CONSTRAINT cadences_concurrency_positive
        CHECK (concurrency IS NULL OR concurrency > 0),
    CONSTRAINT cadences_fail_threshold_positive
        CHECK (fail_threshold IS NULL OR fail_threshold > 0),
    CONSTRAINT cadences_on_failure_valid
        CHECK (on_failure IS NULL OR on_failure IN ('abort', 'continue')),
    CONSTRAINT cadences_created_by_aid_fk
        FOREIGN KEY (created_by_aid) REFERENCES operators (aid)
);

-- Scheduler scan of due schedules (Reaper rule spawn_due_cadence - S2):
-- enabled AND next_run_at <= NOW(). Partial index on next_run_at among enabled.
CREATE INDEX cadences_due_scan_idx
    ON cadences (next_run_at)
    WHERE enabled;

COMMENT ON TABLE cadences IS
    'Registry of Cadence schedules (ADR-046, S1). Spawns a regular Voyage run on a timer (Insert voyages/voyage_targets, back-link voyages.cadence_id). Stores the run recipe (parity VoyageCreateRequest) + repeat rule (interval XOR cron) + overlap_policy (skip/queue/parallel). On S1 - table + CRUD without a scheduler (Reaper spawn_due_cadence - S2).';

-- Back-link voyages.cadence_id (ADR-046 section 2): manual run => NULL; spawned
-- from a Cadence => populated. ON DELETE SET NULL - deleting a Cadence does NOT
-- take the history of its spawned runs with it (orphaned children remain in the
-- All-runs view). Additive nullable (forward-compat only-add, ADR-012). Partial
-- index for the drill-down "schedule -> its runs" (GET /v1/cadences/{id}/runs - S4).
ALTER TABLE voyages
    ADD COLUMN cadence_id TEXT;

ALTER TABLE voyages
    ADD CONSTRAINT voyages_cadence_id_fk
        FOREIGN KEY (cadence_id) REFERENCES cadences (id) ON DELETE SET NULL;

CREATE INDEX voyages_cadence_id_idx
    ON voyages (cadence_id)
    WHERE cadence_id IS NOT NULL;

COMMENT ON COLUMN voyages.cadence_id IS
    'Back-link to the originating Cadence (ADR-046 section 2). NULL => manual run; populated => spawned from a Cadence. FK ON DELETE SET NULL - deleting a Cadence does not take the history of its children with it.';
