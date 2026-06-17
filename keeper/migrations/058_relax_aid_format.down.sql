-- 058_relax_aid_format.down.sql
--
-- Возврат к строгому формату AID из миграции 003 (`^archon-[a-z0-9-]{1,62}$`).
-- ВНИМАНИЕ: откат отвергнет любые AID, заведённые в ослабленном формате
-- (без префикса `archon-` или с символами `. _ @`). Down-путь применим
-- только если такие AID отсутствуют в реестре.

ALTER TABLE operators DROP CONSTRAINT aid_format;
ALTER TABLE operators ADD CONSTRAINT aid_format
    CHECK (aid ~ '^archon-[a-z0-9-]{1,62}$');
