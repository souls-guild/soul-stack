-- 062_drop_errand_runs.up.sql
--
-- Wave 5 Pass 2: complete removal of ErrandRun (ADR-041). The multi-target wrapper
-- around Errand has been folded into Voyage (kind=command covers bulk pull-ad-hoc
-- exec); the separate entity is removed without backward compatibility - the
-- `errand_runs` registry and the errands.errand_run_id back-link are no longer
-- used by any consumer.
--
-- FIRST we drop the back-link from errands (FK + column + index), then DROP TABLE
-- errand_runs (pattern - down.sql of migration 057). The `errands` table (single
-- Errand, ADR-033) STAYS.

DROP INDEX IF EXISTS errands_errand_run_id_idx;
ALTER TABLE errands DROP CONSTRAINT IF EXISTS errands_errand_run_id_fkey;
ALTER TABLE errands DROP COLUMN IF EXISTS errand_run_id;

DROP INDEX IF EXISTS errand_runs_claim_scan_idx;
DROP INDEX IF EXISTS errand_runs_pending_pickup_idx;
DROP TABLE IF EXISTS errand_runs;
