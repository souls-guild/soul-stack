-- 067_rbac_roles_default_scope.down.sql
--
-- Rollback of ADR-047 S1: drop the rbac_roles.default_scope column. Roles revert to
-- S0 semantics (bare permissions unrestricted, scope only per-perm).

ALTER TABLE rbac_roles
    DROP COLUMN default_scope;
