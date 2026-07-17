-- 037_create_sigil_signing_keys.down.sql
--
-- Rollback of 037: the one_primary partial index is dropped along with the table (cascade).

DROP TABLE IF EXISTS sigil_signing_keys;
