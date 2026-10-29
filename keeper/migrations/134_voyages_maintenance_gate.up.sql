-- 134_voyages_maintenance_gate.up.sql
--
-- NIM-1074 (epic NIM-1069, ADR-0093 §3, §5). A scenario Voyage carries what the
-- claim needs to ask the start gate again when its time comes:
--   * in_maintenance_window — the run asked to start inside the incarnations'
--     maintenance windows and outside every freeze; the claim defers it to the
--     next such moment instead of starting it.
--   * confirmed_freeze_ids — the freezes the operator consented to at request
--     time (`confirm_freeze: true`), the ones covering the start then. A freeze
--     outside the set, added later, defers the row at the claim: nobody
--     consented to it. Empty for a window run, a Cadence spawn and the Reaper.
--
-- The ids are not foreign keys: a freeze deleted after consent leaves its id
-- behind, and an id that names nothing covers nothing.
--
-- Rows written before this migration read as "no window asked, no consent":
-- the claim holds them back during any freeze, which is what a freeze means.

ALTER TABLE voyages
    ADD COLUMN in_maintenance_window BOOLEAN NOT NULL DEFAULT FALSE,
    ADD COLUMN confirmed_freeze_ids  TEXT[]  NOT NULL DEFAULT '{}';
