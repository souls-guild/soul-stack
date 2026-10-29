-- 134_voyages_maintenance_gate.down.sql
--
-- Drops the start-gate columns. A Voyage deferred to a window then starts at its
-- recorded schedule_at as an ordinary scheduled run. Roll the binary back with it.

ALTER TABLE voyages
    DROP COLUMN IF EXISTS confirmed_freeze_ids,
    DROP COLUMN IF EXISTS in_maintenance_window;
