-- 004_add_audit_log_operator_fk.down.sql
--
-- Снимает FK, добавленный в 004.up. После этого 003.down может смело
-- дропать таблицу operators (нет dangling references).

ALTER TABLE audit_log
    DROP CONSTRAINT IF EXISTS audit_log_archon_aid_fk;
