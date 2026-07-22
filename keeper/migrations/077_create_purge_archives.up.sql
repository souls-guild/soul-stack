-- 077_create_purge_archives.up.sql
--
-- Reaper retention rules for compliance-class archive data
-- (docs/keeper/reaper.md): three SQL functions that delete old archive rows
-- by their age marker. Closes the backlog item "separate Reaper retention
-- rule for the archive" flagged in migration 039 (incarnation_archive /
-- state_history_archive).
--
-- THREE GROWTH SURFACES (all accumulated without a ceiling before this migration):
--
--   1. incarnation_archive (migration 039) - a snapshot of a torn-down
--      incarnation at the moment of destroy. Grows on every destroy; never
--      cleaned up.
--   2. state_history_archive (migration 039) - a snapshot of the
--      state_history log for a torn-down incarnation. Grows together with
--      (1); never cleaned up.
--   3. state_history with archived_at IS NOT NULL (migration 048) -
--      soft-deleted snapshots in the LIVE state_history table. The
--      archive_state_history rule (049) ONLY sets the flag
--      (archived_at = NOW()) but does not physically delete them - the
--      soft-deleted tail grew without a ceiling, weighing down the live
--      table.
--
-- AGE MARKER (used to compute the retention window):
--   * incarnation_archive.archived_at     - the moment it was written to
--     the archive at destroy time.
--   * state_history_archive.archived_at   - same (written in one
--     transaction).
--   * state_history.archived_at           - the moment of soft-delete by
--     the archive_state_history rule. NULL = active snapshot (NEVER
--     deleted by this rule - it isn't soft-deleted); the
--     archived_at IS NOT NULL filter is mandatory.
--
-- WINDOW: the archive is historical/compliance data, so a conservative
-- default is set by the runner (365 days, see defaultPurgeArchive* in
-- runner.go), configurable via keeper.yml -> reaper.rules.<rule>.max_age.
-- Age is counted from the corresponding archived_at: a row is deleted only
-- if it is older than the window. Anything fresher than the window is left
-- UNTOUCHED (the cost of a mistaken DELETE on the archive is high).
--
-- BATCH: a CTE with LIMIT batch_size caps the transaction size, as in all
-- other Reaper rules (protects against a long DELETE on first enable
-- against an already-accumulated archive). DELETE ... USING expired keyed
-- by the archive PK.
--
-- CASCADES / FK: there are no child tables with an FK to
-- incarnation_archive / state_history_archive (migration 039: the archive
-- is deliberately without referential integrity to the live registry,
-- verified via grep - no REFERENCES *_archive). created_by_aid /
-- changed_by_aid in the archive are value-only columns, NOT FK (039), so
-- DELETE does not touch them. state_history's FK points FROM it TO
-- incarnation (ON DELETE CASCADE, 006) - deleting a parent's
-- soft-deleted snapshot does not touch that. No DELETE here creates
-- dangling references.

CREATE OR REPLACE FUNCTION purge_incarnation_archive(max_age interval, batch_size integer DEFAULT 1000)
RETURNS BIGINT AS $$
DECLARE
    deleted_count BIGINT;
BEGIN
    WITH expired AS (
        SELECT archive_id FROM incarnation_archive
        WHERE archived_at < NOW() - max_age
        ORDER BY archived_at
        LIMIT batch_size
    )
    DELETE FROM incarnation_archive ia
    USING expired e
    WHERE ia.archive_id = e.archive_id;

    GET DIAGNOSTICS deleted_count = ROW_COUNT;
    RETURN deleted_count;
END;
$$ LANGUAGE plpgsql;

COMMENT ON FUNCTION purge_incarnation_archive(interval, integer) IS
    'Deletes a batch of incarnation_archive rows with archived_at older than max_age (compliance retention, default 365d from the runner). No child FKs (039). Returns the number of deleted rows.';

CREATE OR REPLACE FUNCTION purge_state_history_archive(max_age interval, batch_size integer DEFAULT 1000)
RETURNS BIGINT AS $$
DECLARE
    deleted_count BIGINT;
BEGIN
    WITH expired AS (
        SELECT archive_id FROM state_history_archive
        WHERE archived_at < NOW() - max_age
        ORDER BY archived_at
        LIMIT batch_size
    )
    DELETE FROM state_history_archive sha
    USING expired e
    WHERE sha.archive_id = e.archive_id;

    GET DIAGNOSTICS deleted_count = ROW_COUNT;
    RETURN deleted_count;
END;
$$ LANGUAGE plpgsql;

COMMENT ON FUNCTION purge_state_history_archive(interval, integer) IS
    'Deletes a batch of state_history_archive rows with archived_at older than max_age (compliance retention, default 365d from the runner). No child FKs (039). Returns the number of deleted rows.';

-- purge_archived_state_history - a physical DELETE of soft-deleted
-- snapshots (archived_at IS NOT NULL) from the LIVE state_history table,
-- older than max_age. The archived_at IS NOT NULL filter is mandatory:
-- active snapshots (archived_at IS NULL) are the working history feed and
-- this rule must NOT delete them (their soft-delete is a separate rule,
-- archive_state_history, 049). Age is measured from archived_at (the
-- moment of soft-delete), not from at (the moment of the event): the
-- window is meant as "how long the data has sat soft-deleted", for parity
-- with the archive tables.
CREATE OR REPLACE FUNCTION purge_archived_state_history(max_age interval, batch_size integer DEFAULT 1000)
RETURNS BIGINT AS $$
DECLARE
    deleted_count BIGINT;
BEGIN
    WITH expired AS (
        SELECT history_id FROM state_history
        WHERE archived_at IS NOT NULL
          AND archived_at < NOW() - max_age
        ORDER BY archived_at
        LIMIT batch_size
    )
    DELETE FROM state_history sh
    USING expired e
    WHERE sh.history_id = e.history_id;

    GET DIAGNOSTICS deleted_count = ROW_COUNT;
    RETURN deleted_count;
END;
$$ LANGUAGE plpgsql;

COMMENT ON FUNCTION purge_archived_state_history(interval, integer) IS
    'Deletes a batch of soft-deleted snapshots (archived_at IS NOT NULL) from the live state_history older than max_age (compliance retention, default 365d from the runner). Does NOT touch active snapshots (archived_at IS NULL). Returns the number of deleted rows.';
