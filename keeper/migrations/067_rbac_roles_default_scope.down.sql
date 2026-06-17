-- 067_rbac_roles_default_scope.down.sql
--
-- Откат ADR-047 S1: снять колонку rbac_roles.default_scope. Роли вернутся к
-- S0-семантике (bare-permissions unrestricted, scope только per-perm).

ALTER TABLE rbac_roles
    DROP COLUMN default_scope;
