-- 091_extend_heralds_type.down.sql
--
-- Возврат к набору `heralds.type` из миграции 071 (только `webhook`).
-- ВНИМАНИЕ: откат отвергнет любые строки с `type` = `telegram` (и прочими
-- пост-webhook типами). Down-путь применим только при их отсутствии в реестре.

ALTER TABLE heralds DROP CONSTRAINT heralds_type_enum;
ALTER TABLE heralds ADD CONSTRAINT heralds_type_enum
    CHECK (type IN ('webhook'));
