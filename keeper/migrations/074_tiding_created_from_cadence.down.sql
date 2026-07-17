-- 074_tiding_created_from_cadence.down.sql
--
-- Revert of ADR-052 paragraph m: removal of the additive column created_from_cadence_id (and its
-- FK/index cascade via DROP COLUMN). Returns `tidings` to the form from 073.

ALTER TABLE tidings
    DROP COLUMN IF EXISTS created_from_cadence_id;
