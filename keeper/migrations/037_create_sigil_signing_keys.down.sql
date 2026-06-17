-- 037_create_sigil_signing_keys.down.sql
--
-- Откат 037: партиал-индекс one_primary дропается каскадом с таблицей.

DROP TABLE IF EXISTS sigil_signing_keys;
