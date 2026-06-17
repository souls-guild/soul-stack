-- 064_voyages_batch_mode.down.sql
--
-- Down: снять batch_mode-колонку и её CHECK.
ALTER TABLE voyages
    DROP CONSTRAINT IF EXISTS voyages_batch_mode_valid;

ALTER TABLE voyages
    DROP COLUMN IF EXISTS batch_mode;
