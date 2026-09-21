-- 018_create_apply_runs.up.sql
--
-- Registry of apply runs (correlation `apply_id` <-> incarnation/scenario) for
-- the M2.x scenario-runner. Each row is one Soul host within a single
-- run; composite PK `(apply_id, sid)` (apply_id model A: one apply_id
-- per scenario, a different sid for each fan-out host).
--
-- Purpose: when Keeper receives a `RunResult` from a Soul, it doesn't know from the proto
-- which incarnation the run belongs to (RunResult only carries
-- apply_id/status/state_changes). This table closes the correlation:
-- scenario-runner writes a row when dispatching `ApplyRequest`, the RunResult
-- handler reads it by `(apply_id, sid)` and commits the state into the right
-- incarnation.
--
-- task_idx is nullable (PM-decision 2): unknown at dispatch time; filled in
-- on per-task progress (post-MVP), or stays NULL for an aggregated
-- RunResult.
--
-- FK:
--   - incarnation_name -> incarnation(name) ON DELETE CASCADE (runs
--     die together with the incarnation, symmetric to state_history).
--   - started_by_aid   -> operators(aid)   ON DELETE SET NULL (a run's
--     history survives operator deletion; PM-decision 3).
--
-- status is a closed CHECK (PM-decision 1): running/success/failed/cancelled.
-- Retention (Reaper rule `purge_apply_runs`) is backlog (PM-decision 4).

CREATE TABLE apply_runs (
    apply_id          TEXT        NOT NULL,
    sid               TEXT        NOT NULL,
    incarnation_name  TEXT        NOT NULL,
    scenario          TEXT        NOT NULL,
    task_idx          INT,
    status            TEXT        NOT NULL,
    error_summary     TEXT,
    started_at        TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    finished_at       TIMESTAMPTZ,
    started_by_aid    TEXT,

    PRIMARY KEY (apply_id, sid),

    CONSTRAINT apply_runs_status_valid
        CHECK (status IN ('running', 'success', 'failed', 'cancelled')),
    CONSTRAINT apply_runs_incarnation_fk
        FOREIGN KEY (incarnation_name) REFERENCES incarnation (name) ON DELETE CASCADE,
    CONSTRAINT apply_runs_started_by_aid_fk
        FOREIGN KEY (started_by_aid) REFERENCES operators (aid) ON DELETE SET NULL
);

-- Feed of runs for a specific incarnation (triage, history endpoint).
CREATE INDEX apply_runs_incarnation_idx
    ON apply_runs (incarnation_name);

-- Resolves all hosts of a run by apply_id (scenario-runner fan-in,
-- RunResult correlation).
CREATE INDEX apply_runs_apply_idx
    ON apply_runs (apply_id);

-- Partial index for "hanging" runs: the Reaper / triage query "everything that's
-- still running" (terminal statuses are excluded from the index).
CREATE INDEX apply_runs_status_idx
    ON apply_runs (status) WHERE status = 'running';

COMMENT ON TABLE apply_runs IS
    'Correlation apply_id <-> incarnation/scenario for the scenario-runner (M2.x). PK (apply_id, sid).';
