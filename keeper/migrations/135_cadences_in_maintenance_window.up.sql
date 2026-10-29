-- 135_cadences_in_maintenance_window.up.sql
--
-- NIM-1075 (epic NIM-1069, ADR-0093 §4). The Cadence recipe gains the Voyage
-- create field `in_maintenance_window`: each spawn is deferred to the first
-- moment every target incarnation is inside a maintenance window and outside a
-- freeze. Without it a scenario spawn still waits out a freeze; that needs no
-- column.
--
-- The CHECK holds it to kind = 'scenario': a command recipe addresses hosts,
-- and a host has no window, so the flag there would be accepted and do nothing.
--
-- Additive with a default, so existing schedules keep spawning at their tick.

ALTER TABLE cadences
    ADD COLUMN in_maintenance_window BOOLEAN NOT NULL DEFAULT false;

ALTER TABLE cadences
    ADD CONSTRAINT cadences_in_maintenance_window_scenario_only
        CHECK (NOT in_maintenance_window OR kind = 'scenario');
