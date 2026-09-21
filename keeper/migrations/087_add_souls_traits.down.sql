-- 087_add_souls_traits.down.sql
--
-- Reversible rollback of the ADR-060 read/target pilot: drops the GIN index and the
-- `souls.traits` column. `souls.coven` isn't affected (the migration didn't touch it either).

DROP INDEX IF EXISTS souls_traits_idx;

ALTER TABLE souls
    DROP COLUMN IF EXISTS traits;
