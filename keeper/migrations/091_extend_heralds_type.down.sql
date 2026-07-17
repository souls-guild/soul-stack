-- 091_extend_heralds_type.down.sql
--
-- Reverts to the `heralds.type` set from migration 071 (only `webhook`).
-- WARNING: the rollback will reject any rows with `type` = `telegram` (or other
-- post-webhook types). The down path only applies if none are present in the registry.

ALTER TABLE heralds DROP CONSTRAINT heralds_type_enum;
ALTER TABLE heralds ADD CONSTRAINT heralds_type_enum
    CHECK (type IN ('webhook'));
