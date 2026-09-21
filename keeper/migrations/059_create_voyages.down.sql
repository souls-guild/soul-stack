-- 059_create_voyages.down.sql

DROP INDEX IF EXISTS voyage_targets_batch_idx;
DROP TABLE IF EXISTS voyage_targets;

DROP INDEX IF EXISTS voyages_claim_scan_idx;
DROP INDEX IF EXISTS voyages_pending_pickup_idx;
DROP TABLE IF EXISTS voyages;
