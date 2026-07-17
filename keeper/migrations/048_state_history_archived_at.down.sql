-- 048_state_history_archived_at.down.sql
--
-- Reversible: drops the partial index and the column. Soft-deleted snapshots
-- physically remain in the table (archived_at is dropped, so they become
-- indistinguishable from active ones) - this is intentional, so down does not
-- destroy data. After a down-up cycle ALL snapshots become active; the next
-- run of the rule archives the "extra" ones again.

DROP INDEX IF EXISTS state_history_active_idx;

ALTER TABLE state_history
    DROP COLUMN IF EXISTS archived_at;
