-- 095_rename_permission_rerun_last.down.sql
--
-- Mirror rename: rollback for a binary predating the catalog rename, which
-- only knows `incarnation.create-rerun`.

UPDATE rbac_role_permissions AS rp
SET permission = 'incarnation.create-rerun'
WHERE rp.permission = 'incarnation.rerun-last'
  AND NOT EXISTS (
      SELECT 1
      FROM rbac_role_permissions dup
      WHERE dup.role_name = rp.role_name
        AND dup.permission = 'incarnation.create-rerun'
  );

DELETE FROM rbac_role_permissions
WHERE permission = 'incarnation.rerun-last';
