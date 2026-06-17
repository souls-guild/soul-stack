-- 027_seed_cluster_admin.down.sql
--
-- Откат seed-роли cluster-admin. Permission снимается ON DELETE CASCADE при
-- удалении роли, но дропаем явно для симметрии. Membership-строки
-- (rbac_role_operators) при наличии тоже уйдут каскадом — на down это ожидаемо
-- (таблицы дропаются миграцией 026.down полностью).

DELETE FROM rbac_role_permissions WHERE role_name = 'cluster-admin' AND permission = '*';
DELETE FROM rbac_roles WHERE name = 'cluster-admin' AND builtin = true;
