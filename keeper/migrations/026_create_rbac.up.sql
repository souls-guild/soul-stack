-- 026_create_rbac.up.sql
--
-- RBAC-storage в Postgres (ADR-028, docs/keeper/rbac.md → § Storage).
-- Три таблицы материализуют trio «операторы (Архонты) ↔ роли ↔ permissions»:
--   - rbac_roles            — каталог ролей;
--   - rbac_role_permissions — permissions роли (RAW-строкой, парсятся ParsePermission в Go);
--   - rbac_role_operators   — membership «роль ↔ оператор» (тот слой, отсутствие
--                             которого было причиной BUG-1: membership-у негде было
--                             персистентно жить так, чтобы его видели и keeper init,
--                             и enforcer на всех нодах кластера).
--
-- Seed роли cluster-admin (E1) — отдельная миграция 027 (идемпотентный INSERT).

CREATE TABLE rbac_roles (
    name           TEXT        PRIMARY KEY,
    description    TEXT        NOT NULL DEFAULT '',
    builtin        BOOLEAN     NOT NULL DEFAULT false,                -- builtin=true запрещает role.delete / role.update (Фаза 2)
    created_at     TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    created_by_aid TEXT,                                             -- FK на operators(aid); NULL у seed-ролей без инициатора-Архонта

    CONSTRAINT rbac_roles_name_format CHECK (name ~ '^[a-z][a-z0-9-]*$'),
    CONSTRAINT rbac_roles_created_by_aid_fk
        FOREIGN KEY (created_by_aid) REFERENCES operators (aid)
);

COMMENT ON TABLE rbac_roles IS
    'Каталог RBAC-ролей — ADR-028. PK = name (kebab-case). builtin=true защищает от role.delete/update.';

CREATE TABLE rbac_role_permissions (
    role_name  TEXT NOT NULL,
    permission TEXT NOT NULL,                                        -- хранится RAW-строкой; матчинг делает ParsePermission в Go

    PRIMARY KEY (role_name, permission),
    CONSTRAINT rbac_role_permissions_role_fk
        FOREIGN KEY (role_name) REFERENCES rbac_roles (name) ON DELETE CASCADE
);

COMMENT ON TABLE rbac_role_permissions IS
    'Permissions роли (RAW-строкой) — ADR-028. ON DELETE CASCADE с rbac_roles.';

CREATE TABLE rbac_role_operators (
    role_name      TEXT        NOT NULL,
    aid            TEXT        NOT NULL,
    granted_at     TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    granted_by_aid TEXT,                                             -- FK на operators(aid); NULL у seed-/bootstrap-membership-а

    PRIMARY KEY (role_name, aid),
    CONSTRAINT rbac_role_operators_role_fk
        FOREIGN KEY (role_name) REFERENCES rbac_roles (name) ON DELETE CASCADE,
    CONSTRAINT rbac_role_operators_aid_fk
        FOREIGN KEY (aid) REFERENCES operators (aid),
    CONSTRAINT rbac_role_operators_granted_by_aid_fk
        FOREIGN KEY (granted_by_aid) REFERENCES operators (aid)
);

COMMENT ON TABLE rbac_role_operators IS
    'Membership «роль ↔ оператор» — ADR-028. Отсутствие этого слоя было причиной BUG-1.';

-- Индекс «AID → роли» для построения снимка enforcer-а тремя SELECT-ами:
-- основной запрос membership-а идёт по aid, не по PK-порядку (role_name, aid).
CREATE INDEX rbac_role_operators_aid_idx
    ON rbac_role_operators (aid);
