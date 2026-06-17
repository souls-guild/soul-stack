-- 026_create_rbac.down.sql
--
-- Откат RBAC-storage. Порядок DROP-ов значения не имеет — ON DELETE CASCADE
-- и сами DROP TABLE снимают зависимости; но дочерние таблицы дропаем первыми
-- для явности.

DROP TABLE IF EXISTS rbac_role_operators;
DROP TABLE IF EXISTS rbac_role_permissions;
DROP TABLE IF EXISTS rbac_roles;
