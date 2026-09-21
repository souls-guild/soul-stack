-- 068_cadences_interval_floor.down.sql
--
-- Down: remove the floor CHECK and the MIN index. The positive CHECK (066)
-- and the due-scan index (066) are NOT touched - they belong to the previous
-- migration.

DROP INDEX IF EXISTS cadences_enabled_interval_idx;

ALTER TABLE cadences
    DROP CONSTRAINT IF EXISTS cadences_interval_seconds_floor;
