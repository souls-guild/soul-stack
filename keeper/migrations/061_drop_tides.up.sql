-- 061_drop_tides.up.sql
--
-- Wave 5 Pass 1: complete removal of Tide (ADR-040). Tide has been folded into incarnation.run
-- (Voyage / ErrandRun cover batch scenarios), the separate entity is dropped without
-- backward compatibility - the `tides` registry and the apply_runs back-link columns are
-- no longer used by any consumer.
--
-- apply_runs.tide_id / surge_index are soft-links WITHOUT an FK (migration 055): dropping
-- the columns is safe, there are no external references to them. The partial index apply_runs_tide_idx
-- is removed together with the column.

DROP INDEX IF EXISTS apply_runs_tide_idx;
ALTER TABLE apply_runs DROP COLUMN IF EXISTS surge_index;
ALTER TABLE apply_runs DROP COLUMN IF EXISTS tide_id;
DROP TABLE IF EXISTS tides;
