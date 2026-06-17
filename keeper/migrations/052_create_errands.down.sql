-- 052_create_errands.down.sql
--
-- Откат реестра Errand-ов (ADR-033). Индексы дропаются каскадом с таблицей.

DROP TABLE IF EXISTS errands;
