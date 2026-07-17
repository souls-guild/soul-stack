-- 095_rename_permission_rerun_last.up.sql
--
-- Data fix: rename permission `incarnation.create-rerun` -> `incarnation.rerun-last`
-- (rbac catalog, catalog.go). The old name was removed from the catalog WITHOUT a
-- deprecated alias (unlike `incarnation.update` -> `incarnation.update-hosts`), so
-- a custom role with the old string silently got a 403 on rerun-last after the
-- upgrade: a dead row in rbac_role_permissions that can't be recreated via the
-- API - the catalog rejects the removed name.
--
-- Idempotent: a re-run finds no rows with the old name.
-- The NOT EXISTS guard covers the case "the role already has both names" (manual
-- SQL): otherwise UPDATE would fail on the PK (role_name, permission); DELETE
-- cleans up the rest.

UPDATE rbac_role_permissions AS rp
SET permission = 'incarnation.rerun-last'
WHERE rp.permission = 'incarnation.create-rerun'
  AND NOT EXISTS (
      SELECT 1
      FROM rbac_role_permissions dup
      WHERE dup.role_name = rp.role_name
        AND dup.permission = 'incarnation.rerun-last'
  );

DELETE FROM rbac_role_permissions
WHERE permission = 'incarnation.create-rerun';
