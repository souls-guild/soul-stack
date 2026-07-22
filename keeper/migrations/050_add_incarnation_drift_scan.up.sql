-- 050_add_incarnation_drift_scan.up.sql
--
-- ADR-031 Slice C (Scry background): add two fields to the incarnation registry
-- for tracking background Scry scans performed by the Reaper rule
-- `scry_background` (default OFF, opt-in).
--
-- * `last_drift_check_at` - the completion moment of the last dry_run
--   converge run for this incarnation (background or on-demand from Slice B). Uses
--   the iterator predicate of the `scry_background` rule (ORDER BY last_drift_check_at
--   NULLS FIRST -> new incarnations are always scanned before already-scanned ones),
--   and also as an idempotent throttle against rescanning before
--   `min_interval_per_incarnation`.
--
-- * `last_drift_summary` - a counts aggregate assembled from the last DriftReport
--   (`{hosts_drifted, hosts_clean, hosts_unsupported, hosts_failed,
--   total_hosts, scanned_at}`). Counts-only: the full DriftReport is not
--   stored in the background (deferred to a separate slice; on-demand from Slice B returns
--   it directly in the response). Symmetric with `last_drift_check_at` - written by the
--   same UPDATE in `incarnation.UpdateDriftScanResult`.
--
-- The partial index on `last_drift_check_at IS NOT NULL` supports
-- the iterator ordering ORDER BY last_drift_check_at NULLS FIRST: NULL rows
-- are excluded from the index (a sequential scan on the NULL filter picks them up before the fetch
-- from the index), while for already-scanned rows the index returns ORDER without an
-- in-memory sort. On small tables (dozens of incarnations) it makes no difference, but on
-- hundreds to thousands of incarnations it runs with the same plan.

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
