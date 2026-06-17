-- 068_cadences_interval_floor.down.sql
--
-- Down: снять floor-CHECK и MIN-индекс. positive-CHECK (066) и due-scan-индекс
-- (066) НЕ трогаются — это объекты предыдущей миграции.

DROP INDEX IF EXISTS cadences_enabled_interval_idx;

ALTER TABLE cadences
    DROP CONSTRAINT IF EXISTS cadences_interval_seconds_floor;
