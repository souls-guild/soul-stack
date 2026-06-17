-- 063_voyage_targets_dispatch_idx.down.sql
--
-- Down: снять partial-индексы dispatch-ссылок voyage_targets.
DROP INDEX IF EXISTS voyage_targets_apply_id_idx;
DROP INDEX IF EXISTS voyage_targets_errand_id_idx;
