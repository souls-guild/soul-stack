-- 088_add_incarnation_traits.down.sql
--
-- Reversible rollback of the Trait per-soul -> per-incarnation relocation (R1):
-- drops the GIN index and the `incarnation.traits` column. `souls.traits` (087)
-- is NOT affected (this migration never touched it; the projection target
-- remains).

DROP INDEX IF EXISTS incarnation_traits_idx;

ALTER TABLE incarnation
    DROP COLUMN IF EXISTS traits;
