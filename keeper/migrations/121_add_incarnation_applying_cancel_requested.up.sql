-- 121_add_incarnation_applying_cancel_requested.up.sql
--
-- NIM-972 (ADR-027 amend (r)): a run's cluster-wide Cancel lives on the
-- incarnation's applying lock, next to the epoch of migration 082.
--
-- PROBLEM. The Cancel flag lived only on apply_runs rows, and only on non-terminal
-- ones. Under linear dispatch a run has no non-terminal row between one step's
-- barrier and the next step's rows (render, gates, roster refresh), nor before its
-- first row (artifact load, first render). A Cancel reaching a non-owner instance
-- in that window flagged nothing and was lost. The lock is held for the whole run.
--
-- The flag means something only while applying_apply_id names a run: every
-- reader and writer matches on it. It is reset when the lock is taken (lockRun),
-- not when it is released, because a Keeper older than this migration releases
-- the lock without knowing the column; a request made for one run never reaches
-- the next.
--
-- The index serves the two lookups of the flag by apply_id: RequestCancel on any
-- instance, and the owner's Cancel check. The run-status folds and FENCING-1
-- reach the incarnation by its primary key. Partial: only incarnations under a
-- lock carry an apply_id.

ALTER TABLE incarnation
    ADD COLUMN applying_cancel_requested BOOLEAN NOT NULL DEFAULT false;

CREATE INDEX incarnation_applying_apply_id_idx
    ON incarnation (applying_apply_id)
    WHERE applying_apply_id IS NOT NULL;

COMMENT ON COLUMN incarnation.applying_cancel_requested IS
    'cluster-wide Cancel of the run holding the applying lock (ADR-027(r), NIM-972); set by any Keeper instance, read by the owner at every step boundary; meaningful only while applying_apply_id names the run, reset when the lock is taken.';
