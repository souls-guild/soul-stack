-- 132_add_apply_runs_handled.up.sql
--
-- A run whose every failure a rescue handled reads `success` (NIM-993,
-- ADR-0091 amendment 2026-10-14 rule 5). The rows of such a run keep the
-- failure they handled: a host whose failed step a later rescue unit handled
-- leaves a `failed` row exactly as a host whose failure ended the run does.
-- This column, beside `dropped` (migration 122), is what tells the two apart.
--
-- TRUE on a failed or orphaned row when every failure it holds was handled:
-- the source's row, and the rows of rescue units that failed before one
-- succeeded. A row that still holds an unhandled failure when the run ends is
-- not marked, and a dropped row never is. A `success` row that handled a
-- failure inside its own message is no failure of the run and needs no mark.

ALTER TABLE apply_runs ADD COLUMN handled BOOLEAN NOT NULL DEFAULT FALSE;
