-- 135_cadences_in_maintenance_window.down.sql

ALTER TABLE cadences
    DROP CONSTRAINT IF EXISTS cadences_in_maintenance_window_scenario_only;

ALTER TABLE cadences
    DROP COLUMN IF EXISTS in_maintenance_window;
