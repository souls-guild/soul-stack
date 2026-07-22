-- 004_add_audit_log_operator_fk.up.sql
--
-- Adds the FK `audit_log.archon_aid -> operators(aid)`. In 001 the field was
-- declared as TEXT nullable without an FK (operators didn't exist yet); in
-- 003 the operators table was created -- now the constraint can be attached.
--
-- ON DELETE SET NULL: when an operator is deleted, audit records keep
-- NULL in archon_aid (history isn't lost, but the link to the deleted
-- Archon is severed). Deleting an operator is a rare operation (usually
-- `revoked_at` is used instead), but if it happens -- the audit trail
-- matters more than referential integrity.

ALTER TABLE audit_log
    ADD CONSTRAINT audit_log_archon_aid_fk
    FOREIGN KEY (archon_aid)
    REFERENCES operators (aid)
    ON DELETE SET NULL;
