-- 002_create_purge_audit_old.down.sql
--
-- Rollback of the `purge_audit_old` function (see up-migration). The signature
-- `(interval, integer)` must match CREATE - otherwise DROP
-- will not find the target function.

DROP FUNCTION IF EXISTS purge_audit_old(interval, integer);
