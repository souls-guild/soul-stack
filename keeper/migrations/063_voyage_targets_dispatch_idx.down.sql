-- 063_voyage_targets_dispatch_idx.down.sql
--
-- Down: drop the partial indexes for voyage_targets dispatch references.
DROP INDEX IF EXISTS voyage_targets_apply_id_idx;
DROP INDEX IF EXISTS voyage_targets_errand_id_idx;
