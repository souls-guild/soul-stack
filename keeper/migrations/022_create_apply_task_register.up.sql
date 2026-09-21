-- 022_create_apply_task_register.up.sql
--
-- Accumulator of task register data for a run (state_changes full grammar,
-- slice 2 "register-into-sets"). Each row is the register result of one
-- probe task (`register: X`) on one Soul host within a single run.
--
-- Purpose: TaskEvent.register_data reaches Keeper (events_taskevent.go),
-- but previously it only landed in audit/SSE and was not aggregated for CEL. Now it
-- accumulates here, and after the cross-host barrier scenario-runner reads
-- the per-host register map and passes it into RenderStateChanges -> `sets:
-- ${ register.<task>.<field> }` gets rendered.
--
-- Storage is Postgres (NOT in-memory): on multi-Keeper (ADR-002 stateless)
-- a TaskEvent can arrive on a different instance than the one holding the run-goroutine. An in-memory
-- map would collect an incomplete picture -> incorrect incarnation.state commit. The shared
-- table survives cross-Keeper routing.
--
-- register_name is NOT stored here: at TaskEvent time the handler only knows
-- task_idx (the proto does not carry the register name, ADR-012(d) - orchestrator-only).
-- Resolving task_idx -> register_name is done by scenario-runner on read: it holds
-- []RenderedTask with the Register field on the instance that initiated the run.
--
-- PK `(apply_id, sid, task_idx)` - one row per (run, host, task).
-- A repeated TaskEvent for the same task (retry on the Soul side) overwrites
-- register_data (upsert ON CONFLICT in the store) - the last result wins.
--
-- FK to `apply_runs(apply_id, sid)` ON DELETE CASCADE: register data dies
-- together with the run row (the Reaper rule purge_apply_runs cleans it up
-- via cascade, default 30d).
--
-- register is additionally cleaned up MORE AGGRESSIVELY by a separate Reaper rule
-- purge_apply_task_register (migration 023, default grace 1h after the apply_run
-- reaches a terminal state): register_data is plaintext JSONB with potential secrets,
-- transient run-state needed by scenario-runner only up to the cross-host
-- barrier. Keeping it for the full 30d apply-history retention would be an unnecessary window
-- of plaintext storage; the rule clears register earlier, leaving apply_run
-- for history/triage.

CREATE TABLE apply_task_register (
    apply_id      TEXT        NOT NULL,
    sid           TEXT        NOT NULL,
    task_idx      INT         NOT NULL,
    register_data JSONB       NOT NULL,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT NOW(),

    PRIMARY KEY (apply_id, sid, task_idx),

    CONSTRAINT apply_task_register_apply_run_fk
        FOREIGN KEY (apply_id, sid) REFERENCES apply_runs (apply_id, sid) ON DELETE CASCADE
);

-- Loading the full run's register map by scenario-runner after the barrier
-- (per (apply_id) -> all hosts and tasks).
CREATE INDEX apply_task_register_apply_idx
    ON apply_task_register (apply_id);

COMMENT ON TABLE apply_task_register IS
    'Accumulator of task register data for a run, feeding state_changes.sets (slice 2). PK (apply_id, sid, task_idx); FK to apply_runs ON DELETE CASCADE.';
