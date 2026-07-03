-- 095_rename_permission_rerun_last.up.sql
--
-- Data-фикс rename permission `incarnation.create-rerun` → `incarnation.rerun-last`
-- (rbac-каталог, catalog.go). Старое имя удалено из каталога БЕЗ deprecated-alias
-- (в отличие от `incarnation.update` → `incarnation.update-hosts`), поэтому
-- кастомная роль со старой строкой после апгрейда молча получала 403 на
-- rerun-last: мёртвая строка в rbac_role_permissions, пересоздать через API
-- нельзя — каталог отвергает удалённое имя.
--
-- Идемпотентно: повторный прогон не находит строк со старым именем.
-- NOT EXISTS-guard закрывает кейс «у роли уже оба имени» (ручной SQL): иначе
-- UPDATE упал бы на PK (role_name, permission); DELETE добирает остаток.

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
