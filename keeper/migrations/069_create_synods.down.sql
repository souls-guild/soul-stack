-- 069_create_synods.down.sql
--
-- Rollback of Synod storage (ADR-049). Child tables are dropped first for clarity --
-- ON DELETE CASCADE plus the DROP TABLE statements would resolve dependencies in any order anyway.

DROP TABLE IF EXISTS synod_roles;
DROP TABLE IF EXISTS synod_operators;
DROP TABLE IF EXISTS synods;
