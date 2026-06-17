-- 050_add_incarnation_drift_scan.down.sql
--
-- Reverse 050: убираем поля Scry-background-skана. Partial-индекс умирает
-- автоматически с колонкой.

ALTER TABLE incarnation
    DROP COLUMN IF EXISTS last_drift_summary,
    DROP COLUMN IF EXISTS last_drift_check_at;
