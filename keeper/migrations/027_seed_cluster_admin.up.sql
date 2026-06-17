-- 027_seed_cluster_admin.up.sql
--
-- Seed-миграция E1 (ADR-028(b), docs/keeper/rbac.md → § Встроенные роли).
-- Встроенная роль cluster-admin с единственным permission `*` существует в БД
-- ДО любого keeper init. keeper init в своей advisory-lock-транзакции лишь
-- добавляет membership-строку (cluster-admin, <aid>) в rbac_role_operators —
-- это фикс BUG-1.
--
-- builtin=true защищает роль от role.delete / role.update (Фаза 2).
-- created_by_aid = NULL — seed-роль без инициатора-Архонта.
--
-- ON CONFLICT DO NOTHING делает миграцию идемпотентной и безопасной при любом
-- порядке относительно keeper init (роль уже могла быть вставлена).

INSERT INTO rbac_roles (name, description, builtin, created_by_aid)
VALUES ('cluster-admin', 'Встроенная роль полного доступа (permissions: *)', true, NULL)
ON CONFLICT (name) DO NOTHING;

INSERT INTO rbac_role_permissions (role_name, permission)
VALUES ('cluster-admin', '*')
ON CONFLICT (role_name, permission) DO NOTHING;
