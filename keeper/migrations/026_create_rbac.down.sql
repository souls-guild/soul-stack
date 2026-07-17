-- 026_create_rbac.down.sql
--
-- Revert of RBAC storage. The order of DROPs does not matter - ON DELETE CASCADE
-- and the DROP TABLE statements themselves remove dependencies; but we drop child tables first
-- for clarity.

DROP TABLE IF EXISTS rbac_role_operators;
DROP TABLE IF EXISTS rbac_role_permissions;
DROP TABLE IF EXISTS rbac_roles;
