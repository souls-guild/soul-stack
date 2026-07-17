-- 067_rbac_roles_default_scope.up.sql
--
-- ADR-047 S1 - Purview default_scope at the role level.
--
-- Column rbac_roles.default_scope (TEXT nullable) - a scope selector,
-- inherited by ALL permissions of the role. Syntax = per-permission selector
-- (`on key=v,...`; the MVP dimension is coven). NULL = dimension NOT introduced ->
-- bare-permissions roles stay unrestricted (BACKCOMPAT: existing roles
-- are not broken - this is exactly default-deny "by introduced dimensions").
--
-- String parsing is done by Go (parseSelector) - the DB stores RAW, same as
-- rbac_role_permissions.permission. No CHECK is introduced here: the grammar is caught
-- by ParseDefaultScope on snapshot load (better error, single source of truth).

ALTER TABLE rbac_roles
    ADD COLUMN default_scope TEXT;

COMMENT ON COLUMN rbac_roles.default_scope IS
    'ADR-047 S1: scope selector (RAW, per-perm-selector syntax), inherited by all permissions of the role. NULL = dimension not introduced = bare-perms unrestricted (backcompat). Parsed by ParseDefaultScope in Go.';
