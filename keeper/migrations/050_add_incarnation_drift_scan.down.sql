-- 050_add_incarnation_drift_scan.down.sql
--
-- Reverse 050: drop the Scry background-scan fields. The partial index dies
-- automatically along with the column.

ALTER TABLE incarnation
    DROP COLUMN IF EXISTS last_drift_summary,
    DROP COLUMN IF EXISTS last_drift_check_at;
