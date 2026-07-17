-- 027_seed_cluster_admin.up.sql
--
-- Seed migration E1 (ADR-028(b), docs/keeper/rbac.md -> the "Built-in roles" section).
-- The built-in cluster-admin role with the single permission `*` exists in the DB
-- BEFORE any keeper init. keeper init, inside its advisory-lock transaction, only
-- adds a membership row (cluster-admin, <aid>) to rbac_role_operators -
-- this is the BUG-1 fix.
--
-- builtin=true protects the role from role.delete / role.update (Phase 2).
-- created_by_aid = NULL - a seed role has no initiating Archon.
--
-- ON CONFLICT DO NOTHING makes the migration idempotent and safe regardless of
-- ordering relative to keeper init (the role may already have been inserted).

INSERT INTO rbac_roles (name, description, builtin, created_by_aid)
VALUES ('cluster-admin', 'Built-in role with full access (permissions: *)', true, NULL)
ON CONFLICT (name) DO NOTHING;

INSERT INTO rbac_role_permissions (role_name, permission)
VALUES ('cluster-admin', '*')
ON CONFLICT (role_name, permission) DO NOTHING;
