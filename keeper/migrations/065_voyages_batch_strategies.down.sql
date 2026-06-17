-- 065_voyages_batch_strategies.down.sql
--
-- Down: снять колонки батч-стратегий и их CHECK-и.
ALTER TABLE voyages
    DROP CONSTRAINT IF EXISTS voyages_fail_threshold_positive;

ALTER TABLE voyages
    DROP CONSTRAINT IF EXISTS voyages_batch_percent_range;

ALTER TABLE voyages
    DROP COLUMN IF EXISTS require_alive,
    DROP COLUMN IF EXISTS inter_unit_interval,
    DROP COLUMN IF EXISTS fail_threshold,
    DROP COLUMN IF EXISTS batch_percent;
