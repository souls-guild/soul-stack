-- 084_operators_created_via.down.sql
--
-- Rollback of 084: drop the CHECK + DROP the column. Applied by the framework AFTER
-- the rollback of 085 (the down-chain runs in reverse order), so by this point
-- the bootstrap index has already been reverted to `created_by_aid IS NULL` - the
-- created_via column is no longer used by anything.

ALTER TABLE operators DROP CONSTRAINT created_via_valid;
ALTER TABLE operators DROP COLUMN created_via;
