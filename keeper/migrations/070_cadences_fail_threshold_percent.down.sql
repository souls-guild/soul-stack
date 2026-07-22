-- 070_cadences_fail_threshold_percent.down.sql
--
-- Rollback: drop the CHECK and the fail_threshold_percent column (order --
-- CHECK first, even though DROP COLUMN would take the dependent constraint
-- with it anyway).

ALTER TABLE cadences
    DROP CONSTRAINT IF EXISTS cadences_fail_threshold_percent_range;

ALTER TABLE cadences
    DROP COLUMN IF EXISTS fail_threshold_percent;
