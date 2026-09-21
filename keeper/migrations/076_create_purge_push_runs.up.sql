-- 076_create_purge_push_runs.up.sql
--
-- Reaper rule `purge_push_runs` (docs/keeper/reaper.md): deletes a batch of
-- finished push runs from the `push_runs` registry (migration 051) older than
-- `max_age`. Retention for the growing push-run history (default 30d, measured
-- from `finished_at`). Mirror of `purge_apply_runs` (021) / `purge_voyages` (075)
-- for the push side.
--
-- WHY the rule is needed: `push_runs` is a run-history table (one row =
-- one `POST /v1/push/apply`), grows without a ceiling, and until now had NO
-- retention for finished records. The existing rule `purge_orphan_push_runs`
-- (Go-side, push_orphan.go) is about SOMETHING ELSE: terminalizing in-flight zombies
-- (pending/running older than 1h -> cancelled), it does NOT delete finished rows.
-- This rule closes the remaining gap - DELETE of finished ones.
--
-- ONLY finished records are deleted (`status` in `success` / `partial_failed` /
-- `failed` / `cancelled` and `finished_at IS NOT NULL`). `pending` /
-- `running` records are NEVER purged - those are in-flight/hung runs; their triage and
-- terminalization is the `purge_orphan_push_runs` rule's job, not this one's. The terminal
-- `cancelled` state is set by that same orphan rule - this rule picks up such records
-- once its own window has elapsed (parity: orphan terminalizes at 1h,
-- the final DELETE at 30d, same as apply_runs).
--
-- Age is measured from `finished_at`: history is measured by time since
-- completion, not start (a long run that finished recently shouldn't
-- be deleted before a short one that finished long ago). Parity with purge_apply_runs.
--
-- CASCADES / DEPENDENCIES: there are NO child tables with an FK to `push_runs` - per-host
-- results are stored inline in `push_runs.summary` (jsonb), NOT in a separate
-- table (push is a synchronous oneshot, it doesn't go through the apply_runs barrier; see
-- 051). The FK `push_runs.started_by_aid -> operators` points FROM push_run TO
-- operators (ON DELETE SET NULL) - DELETE push_run doesn't affect it. The DELETE
-- doesn't create dangling references.
--
-- PK `apply_id` (051) -> a single-column DELETE WHERE apply_id IN (...);
-- a CTE with LIMIT batch_size caps the transaction size, same as the other
-- Reaper rules. We don't add a dedicated finished_at index until there's
-- real volume (history-purge is a cold background job, not a hot path; parity with
-- purge_apply_runs / purge_voyages, which have no dedicated finished_at index either).

CREATE OR REPLACE FUNCTION purge_push_runs(max_age interval, batch_size integer DEFAULT 1000)
RETURNS BIGINT AS $$
DECLARE
    deleted_count BIGINT;
BEGIN
    WITH expired AS (
        SELECT apply_id FROM push_runs
        WHERE status IN ('success', 'partial_failed', 'failed', 'cancelled')
          AND finished_at IS NOT NULL
          AND finished_at < NOW() - max_age
        ORDER BY finished_at
        LIMIT batch_size
    )
    DELETE FROM push_runs pr
    USING expired e
    WHERE pr.apply_id = e.apply_id;

    GET DIAGNOSTICS deleted_count = ROW_COUNT;
    RETURN deleted_count;
END;
$$ LANGUAGE plpgsql;

COMMENT ON FUNCTION purge_push_runs(interval, integer) IS
    'Deletes a batch of finished push_runs (success/partial_failed/failed/cancelled) with finished_at older than max_age. Does not touch pending/running (that is the purge_orphan_push_runs rule). No child FK tables (per-host results are inline in summary). Returns the number of deleted rows (parity purge_apply_runs).';
