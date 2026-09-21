-- 027_seed_cluster_admin.down.sql
--
-- Rollback of the seeded cluster-admin role. The permission is removed via ON DELETE
-- CASCADE when the role is deleted, but we drop it explicitly for symmetry.
-- Membership rows (rbac_role_operators), if any, will also cascade away - expected
-- on down (the tables are fully dropped by migration 026.down).

DELETE FROM rbac_role_permissions WHERE role_name = 'cluster-admin' AND permission = '*';
DELETE FROM rbac_roles WHERE name = 'cluster-admin' AND builtin = true;
