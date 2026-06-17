-- 070_cadences_fail_threshold_percent.down.sql
--
-- Откат: снять CHECK и колонку fail_threshold_percent (порядок — CHECK первым,
-- хотя DROP COLUMN снёс бы зависимый constraint и сам).

ALTER TABLE cadences
    DROP CONSTRAINT IF EXISTS cadences_fail_threshold_percent_range;

ALTER TABLE cadences
    DROP COLUMN IF EXISTS fail_threshold_percent;
