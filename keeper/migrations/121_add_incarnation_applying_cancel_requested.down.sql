-- 121_add_incarnation_applying_cancel_requested.down.sql
--
-- Revert of NIM-972: drops the run-level Cancel flag and the apply_id index. A
-- Cancel requested and not yet read by the owner is lost with the column.

DROP INDEX IF EXISTS incarnation_applying_apply_id_idx;

ALTER TABLE incarnation
    DROP COLUMN IF EXISTS applying_cancel_requested;
