-- 003_create_operators.down.sql
--
-- Reverse migration. The FK `audit_log.archon_aid -> operators(aid)` is created
-- in 004 and must be dropped before DROP TABLE operators - so 004.down
-- runs before 003.down in rollback order (`Steps(-N)` in golang-migrate
-- applies .down.sql files in reverse numeric order, which guarantees
-- the correct sequence automatically).

DROP TABLE IF EXISTS operators;
