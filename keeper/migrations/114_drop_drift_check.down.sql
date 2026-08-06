-- 114_drop_drift_check.down.sql
--
-- Reverse of 114: restore the SCHEMA that migration 050 created, and nothing
-- else. The two columns come back NULL for every row and the partial index is
-- rebuilt, so a keeper rolled back to pre-NIM-446 code starts against a schema
-- it recognizes and rescans every incarnation as never-scanned. That is the
-- correct outcome — the scan results themselves were dropped and there is
-- nothing to recover them from.
--
-- The DATA halves are deliberately NOT undone, for the same reason migration
-- 109 declined to re-grant:
--
--   * RBAC grants — which roles held `incarnation.check-drift`, and with which
--     scope, is not recoverable from anything left in the database, and
--     re-granting a permission by guess is how an authorization system acquires
--     a grant nobody can account for. On rollback the catalog name returns with
--     the binary, so the parse succeeds; the only effect is that check-drift
--     answers 403 until an operator re-grants it deliberately through the role
--     API, where the act is audited.
--
--   * Tiding subscriptions — a deleted rule's herald/filters/selectors are gone
--     and a stripped rule's element cannot be told apart from one an operator
--     removed by hand. Re-creating a notification rule from a guess is worse
--     than its absence: it would deliver to a destination nobody chose.

ALTER TABLE incarnation
    ADD COLUMN last_drift_check_at TIMESTAMPTZ,
    ADD COLUMN last_drift_summary  JSONB;

CREATE INDEX incarnation_last_drift_check_at_idx
    ON incarnation (last_drift_check_at)
    WHERE last_drift_check_at IS NOT NULL;

COMMENT ON COLUMN incarnation.last_drift_check_at IS
    'ADR-031 Slice C: completion time of the last dry_run converge (background or on-demand). NULL for incarnations that have never been scanned.';

COMMENT ON COLUMN incarnation.last_drift_summary IS
    'ADR-031 Slice C: counts aggregate of the last DriftReport (hosts_drifted/clean/unsupported/failed + total + scanned_at). Counts-only - the full report is not stored in the background.';
