-- 109_drop_permission_update_hosts.down.sql
--
-- Schema-neutral by construction: the up-migration changes DATA only (it deletes
-- dead grants out of rbac_role_permissions), so there is no structure to undo.
--
-- The grants are deliberately NOT re-granted. Which roles held them, and with
-- which scope, is not recoverable from anything left in the database - and
-- re-granting a right by guess is how an authorization system acquires a grant
-- nobody can account for. A keeper rolled back to pre-NIM-330 code starts
-- cleanly without them: the parse succeeds because the catalog names come back
-- with the binary, and the only effect is that `PATCH .../hosts` answers 403
-- until an operator re-grants the permission deliberately, through the role API,
-- where the act is audited.

SELECT 1;
