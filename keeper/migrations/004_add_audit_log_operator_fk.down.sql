-- 004_add_audit_log_operator_fk.down.sql
--
-- Drops the FK added in 004.up. After this, 003.down can safely
-- drop the operators table (no dangling references).

ALTER TABLE audit_log
    DROP CONSTRAINT IF EXISTS audit_log_archon_aid_fk;
