-- 055_create_tides.down.sql

DROP INDEX IF EXISTS apply_runs_tide_idx;
ALTER TABLE apply_runs DROP COLUMN IF EXISTS surge_index;
ALTER TABLE apply_runs DROP COLUMN IF EXISTS tide_id;
DROP TABLE IF EXISTS tides;
