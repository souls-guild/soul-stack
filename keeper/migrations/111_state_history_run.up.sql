-- 111_state_history_run.up.sql
--
-- NIM-408. A run's replayable input, and what became of it, move into the row
-- that already records the attempt.
--
-- WHY HERE. `rerun-last` recovers the failed run's input from two different
-- places depending on which branch produced it (`UnlockForRerun`): the create
-- path reads `incarnation.spec.input`, the day-2 path reads
-- `apply_runs.recipe` by the apply_id of the last history row. The two have
-- different lifetimes — spec is forever, recipe is purged after 30 days — so a
-- day-2 rerun already dies in `ErrRerunInputUnavailable` once the recipe is
-- gone, with no way back. `state_history` is read in that same transaction
-- already, as the POINTER to the failed run, and it is kept for a year.
--
-- WHY NOT JUST `input`. The input alone is not replayable. Rendering needs the
-- git coordinates of the service code the run used, and an upgrade moves the
-- incarnation's pin (`UPDATE incarnation SET service_version = …`) — so a rerun
-- reconstructed from a bare input renders with the CURRENT version, not the one
-- the attempt failed on. The rerun handler does exactly that today
-- (`serviceRef.Ref = inc.ServiceVersion`), which is quieter and worse than the
-- defect it would be fixing. `recipe` pins the ref for precisely this reason,
-- and `run` carries the same shape.
--
-- WHY NOT MOVE `recipe` HERE. It cannot move: an `apply_runs` row is written at
-- dispatch and the Acolyte reads its recipe at CLAIM time, to render the task
-- just-in-time. A history row is written in the run's TERMINAL. At claim time
-- there is no history row yet. The two answer different questions at different
-- moments; `recipe` stays operational and is still purged with its run.
--
-- SHAPE. Two things, deliberately not one column:
--
--   * `run` (jsonb) — the replayable snapshot, in the shape of
--     `applyrun.Recipe` so the existing type and its Marshal/Unmarshal are
--     reused rather than mirrored. jsonb because that struct has already grown
--     twice (`dry_run`, `from_upgrade`) and absence-in-jsonb is how it stays
--     forward-compatible. Moving to scalar columns later remains an ordinary
--     migration; the door is not one-way.
--   * the outcome — `run_status` / `finished_at` / `error_summary`, as columns.
--     `run_status` drives a read filter (`GET /v1/incarnations/{name}/history`
--     hides rows for runs still in flight), and a predicate the API applies on
--     every read belongs in a column rather than inside a document.
--
-- Both are NULLABLE and both stay NULL on existing rows. That is not laziness
-- about backfill — there is nothing to backfill FROM. A history row records that
-- an attempt happened; the input that produced it lived somewhere else, and for
-- day-2 runs older than the purge it no longer exists anywhere. A NULL `run`
-- means "this attempt cannot be replayed from history", which the rerun path
-- answers with a 422 asking for the input rather than a 409 dead end.
--
-- `run_status` is deliberately NOT constrained to an enum. It records the
-- terminal status of the ATTEMPT, and the incarnation status vocabulary it draws
-- from is defined in code and has been extended several times; a CHECK here
-- would turn each such extension into a migration that only exists to let the
-- history table keep up.

ALTER TABLE state_history
    ADD COLUMN run           JSONB,
    ADD COLUMN run_status    TEXT,
    ADD COLUMN finished_at   TIMESTAMPTZ,
    ADD COLUMN error_summary TEXT;

-- The read filter of GET /v1/incarnations/{name}/history: newest-first within
-- one incarnation, skipping rows whose run has not reached a terminal. Extends
-- the existing (incarnation_name, at DESC) index rather than replacing it — that
-- one still serves the unfiltered feed and the `ORDER BY history_id DESC LIMIT 1`
-- probe of rerun-last.
CREATE INDEX state_history_incarnation_run_status_idx
    ON state_history (incarnation_name, at DESC)
    WHERE run_status IS NOT NULL;
