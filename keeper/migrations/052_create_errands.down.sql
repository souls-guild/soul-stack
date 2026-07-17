-- 052_create_errands.down.sql
--
-- Rollback of the Errand registry (ADR-033). Indexes are dropped along with the table via cascade.

DROP TABLE IF EXISTS errands;
