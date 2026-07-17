-- 048_state_history_archived_at.up.sql
--
-- ADR-Q19 retention (PM decision, 2026-05): `state_history` rows are NOT deleted
-- physically - old snapshots are marked with the soft-delete flag `archived_at`
-- and kept around (optionally for an external bulk exporter). Default policy -
-- the last N=50 per incarnation, plus always a snapshot of state_schema migration
-- steps (scenario='migration'); see docs/keeper/reaper.md, rule
-- `archive_state_history`.
--
-- Column behavior:
--   * archived_at IS NULL      - active snapshot: visible in the Operator API /
--     MCP / Soul-resolver; counted by the "active" filter during pagination.
--   * archived_at IS NOT NULL  - soft-deleted: the moment the Reaper rule
--     marked it. Hidden from reads by default; read via the separate flag
--     `include_archived=true` (Operator API) when a deeper look is needed.
--
-- Writing new snapshots (INSERT into state_history) doesn't change behavior:
-- fresh rows land with archived_at = NULL via DEFAULT, no INSERT changes
-- are needed.
--
-- Index `state_history_active_idx` - partial on WHERE archived_at IS NULL,
-- covers the typical history-feed query (HistorySelectByName + Operator
-- API GET /v1/incarnations/{name}/history). Without it, the active filter
-- would fall back to the existing `state_history_incarnation_at_idx` with an
-- extra scan over soft-deleted rows - at a retention of 50 live / 1000+ soft-
-- deleted, that grows linearly.

ALTER TABLE state_history
    ADD COLUMN archived_at TIMESTAMPTZ;

CREATE INDEX state_history_active_idx
    ON state_history (incarnation_name, at DESC)
    WHERE archived_at IS NULL;

COMMENT ON COLUMN state_history.archived_at IS
    'Soft-delete flag (ADR-Q19 retention). NULL = active snapshot; NOT NULL = soft-deleted time set by the Reaper rule archive_state_history.';
