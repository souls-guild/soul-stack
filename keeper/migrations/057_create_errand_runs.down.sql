-- 057_create_errand_runs.down.sql

DROP INDEX IF EXISTS errands_errand_run_id_idx;
ALTER TABLE errands DROP CONSTRAINT IF EXISTS errands_errand_run_id_fkey;
ALTER TABLE errands DROP COLUMN IF EXISTS errand_run_id;

DROP INDEX IF EXISTS errand_runs_claim_scan_idx;
DROP INDEX IF EXISTS errand_runs_pending_pickup_idx;
DROP TABLE IF EXISTS errand_runs;
