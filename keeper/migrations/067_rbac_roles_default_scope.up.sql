-- 067_rbac_roles_default_scope.up.sql
--
-- ADR-047 S1 — Purview default_scope на уровне роли.
--
-- Колонка rbac_roles.default_scope (TEXT nullable) — scope-селектор,
-- наследуемый ВСЕМИ permission-ами роли. Синтаксис = per-permission селектор
-- (`on key=v,...`; MVP-измерение — coven). NULL = измерение НЕ введено →
-- bare-permissions роли остаются unrestricted (BACKCOMPAT: существующие роли
-- не ломаются — это и есть default-deny «по введённым измерениям»).
--
-- Парсинг строки делает Go (parseSelector) — БД хранит RAW, как и
-- rbac_role_permissions.permission. CHECK здесь не вводим: грамматику ловит
-- ParseDefaultScope при load снимка (better error, единый источник правды).

ALTER TABLE rbac_roles
    ADD COLUMN default_scope TEXT;

COMMENT ON COLUMN rbac_roles.default_scope IS
    'ADR-047 S1: scope-селектор (RAW, синтаксис per-perm-селектора), наследуемый всеми permission-ами роли. NULL = измерение не введено = bare-perms unrestricted (backcompat). Парсится ParseDefaultScope в Go.';
