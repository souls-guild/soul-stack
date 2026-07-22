-- 075_create_purge_voyages.up.sql
--
-- Reaper rule `purge_voyages` (docs/keeper/reaper.md): deletes a batch of
-- finished Voyage runs from the `voyages` registry (migration 059) older than
-- `max_age`. Retention for the growing run history - the deferred
-- implementation of `purge_voyages` from ADR-046 §79 (it was listed there as
-- "if introduced"; on the way to beta, `voyages` growth on the beta fleet made
-- the rule mandatory).
--
-- WHY `voyages` specifically (and not cadences/choirs/errands):
--   * `voyages` is a run HISTORY, growing without a ceiling (every manual run,
--     every Cadence spawn = a new row). The only run-history table WITHOUT
--     retention (apply_runs/audit/errands/seeds/souls/state_history are already
--     covered by their own rules; tides/errand_runs were dropped by migrations
--     061/062; there is no separate `cadence_runs`/`surges` table - the spawn
--     history IS the `voyages` rows with a populated `cadence_id`, and surges
--     are absorbed by `voyage_targets.batch_index`).
--   * `cadences` are ACTIVE schedules (enabled, outlive runs), NOT history.
--     Retention on them would corrupt data (see ADR-046 §9: deleting a Cadence
--     is an operator action, not age-based hygiene). NOT purged.
--   * `incarnation_choirs`/`incarnation_choir_voices` are the active declared
--     topology, NOT history. NOT purged.
--
-- ONLY finished records are deleted (`succeeded`/`failed`/`partial_failed`/
-- `cancelled` with `finished_at IS NOT NULL`). `scheduled`/`pending`/`running`
-- are NEVER purged - these are unfinished/in-progress runs (the terminal state
-- is set for them by VoyageWorker.Finalize or reclaim_voyages, not the Reaper).
-- Mirrors `purge_apply_runs` (021): terminal-only, age measured from
-- `finished_at`.
--
-- Age is measured from `finished_at`: history is measured by time since
-- completion, not start (a long run that finished recently should not be
-- deleted before a short one that finished long ago). Parity with
-- purge_apply_runs.
--
-- CASCADES / DEPENDENCIES (invariant: "leave no dangling references"):
--   * `voyage_targets.voyage_id` -> voyages ON DELETE CASCADE (migration 059):
--     Leg rows of a run are carried away by the cascade. Correct - they are
--     part of the same run.
--   * `voyage_targets.apply_id`/`errand_id` - soft link WITHOUT an FK to
--     apply_runs/errands. Purging a voyage does NOT touch them: apply_runs is
--     cleaned by `purge_apply_runs` on its OWN window, errands by
--     `purge_old_errands`. CORRELATION INVARIANT: the purge_voyages window
--     defaults to the same window as purge_apply_runs (30d), so a "voyage ->
--     its apply_runs" drill does not lose either side (voyage deleted while
--     apply_runs are still needed for correlation - or vice versa). Default
--     alignment lives in keeper.yml/code.
--   * `tidings.voyage_id` - an ephemeral Tiding, soft link WITHOUT an FK
--     (migration 072, intentional: cleanup via the terminal state +
--     `purge_orphan_ephemeral_tidings` with a 5m grace period). By the time
--     purge_voyages runs (30d), ephemeral Tidings have long since been removed
--     by that rule; if an orphan remains, `purge_orphan_ephemeral_tidings`
--     picks it up via the `NOT EXISTS voyages` predicate. DELETE voyage
--     creates no dangling references.
--   * `voyages.cadence_id` -> cadences ON DELETE SET NULL (migration 066):
--     purging the CHILDREN does not touch the SCHEDULE (the FK points from
--     voyage to cadence, not the other way around).
--
-- PK `voyage_id` (059) -> DELETE via single-column WHERE voyage_id IN (...);
-- a CTE with LIMIT batch_size caps the transaction size, as in the other
-- Reaper rules. Is the `voyages_pending_pickup_idx` index used? No, that one
-- is partial on pending; for covering the finished scan, a sequential pass
-- with LIMIT is enough (history purge is a cold background job, not a
-- hot path) - a dedicated finished_at index is not added until real volume
-- shows up (parity with purge_apply_runs, which also has no dedicated
-- finished_at index).

-- NOTE: the LIMIT parameter is named `batch_limit`, NOT `batch_size` (unlike
-- purge_apply_runs 021): the `voyages` table CARRIES a `batch_size` column
-- (migration 059, the run's batch size), and a same-named function parameter
-- in `LIMIT batch_size` produces `column reference "batch_size" is ambiguous`
-- (SQLSTATE 42702) - PG prefers the column from FROM. apply_runs has no such
-- column, so 021 is unaffected. The parameter name is the only difference from
-- the 021 template.
CREATE OR REPLACE FUNCTION purge_voyages(max_age interval, batch_limit integer DEFAULT 1000)
RETURNS BIGINT AS $$
DECLARE
    deleted_count BIGINT;
BEGIN
    WITH expired AS (
        SELECT voyage_id FROM voyages
        WHERE status IN ('succeeded', 'failed', 'partial_failed', 'cancelled')
          AND finished_at IS NOT NULL
          AND finished_at < NOW() - max_age
        ORDER BY finished_at
        LIMIT batch_limit
    )
    DELETE FROM voyages v
    USING expired e
    WHERE v.voyage_id = e.voyage_id;

    GET DIAGNOSTICS deleted_count = ROW_COUNT;
    RETURN deleted_count;
END;
$$ LANGUAGE plpgsql;

COMMENT ON FUNCTION purge_voyages(interval, integer) IS
    'Deletes a batch of finished voyages (succeeded/failed/partial_failed/cancelled) with finished_at older than max_age. Does not touch scheduled/pending/running. voyage_targets are carried away via ON DELETE CASCADE. Returns the number of deleted rows (ADR-046 §79, parity with purge_apply_runs).';
