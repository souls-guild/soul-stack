-- 069_create_synods.up.sql
--
-- Synod — группа архонов (ADR-049, docs/architecture.md → ADR-049).
-- Промежуточный уровень между «оператор» и «роль»: модель Архон → Synod → Роли.
-- Три таблицы тем же паттерном rbac_* (ADR-028, миграция 026):
--   - synods           — каталог групп (симметрия rbac_roles: каталог + builtin);
--   - synod_operators  — membership «Synod ↔ архон» (симметрия rbac_role_operators);
--   - synod_roles      — bundle «Synod ↔ роль» (новый уровень — набор ролей группы).
--
-- Эффективные роли архона = прямые (rbac_role_operators) ∪ роли через все его
-- Synod-ы — объединение собирается в snapshot-сборке enforcer-а (ADR-049(e)).
--
-- ВАЖНО (ADR-049(f)): least-privilege subset и self-lockout ОБЯЗАНЫ учитывать
-- роли через Synod. Это слайс S2 (security-SQL) — на момент этой миграции
-- subset/self-lockout ещё считают только прямые роли (известный gap).

CREATE TABLE synods (
    name           TEXT        PRIMARY KEY,
    description    TEXT        NOT NULL DEFAULT '',
    builtin        BOOLEAN     NOT NULL DEFAULT false,                -- builtin=true запрещает synod.delete (симметрия rbac_roles.builtin)
    created_at     TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    created_by_aid TEXT,                                             -- FK на operators(aid); NULL у seed-групп без инициатора-Архонта

    CONSTRAINT synods_name_format CHECK (name ~ '^[a-z][a-z0-9-]*$'),
    CONSTRAINT synods_created_by_aid_fk
        FOREIGN KEY (created_by_aid) REFERENCES operators (aid)
);

COMMENT ON TABLE synods IS
    'Каталог Synod-групп — ADR-049. PK = name (kebab-case). builtin=true защищает от synod.delete.';

CREATE TABLE synod_operators (
    synod_name   TEXT        NOT NULL,
    aid          TEXT        NOT NULL,
    added_at     TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    added_by_aid TEXT,                                               -- FK на operators(aid); NULL у seed-/bootstrap-membership-а

    PRIMARY KEY (synod_name, aid),
    CONSTRAINT synod_operators_synod_fk
        FOREIGN KEY (synod_name) REFERENCES synods (name) ON DELETE CASCADE,
    -- CASCADE сознательно (отличается от rbac_role_operators_aid_fk БЕЗ CASCADE):
    -- удаление operator-а авто-чистит его Synod-membership. Operators реально не
    -- удаляются (revoke = revoked_at, ADR-014), но при hard-delete (тесты/cleanup)
    -- осиротевшие synod_operators-строки недопустимы — FK без CASCADE заблокировал
    -- бы DELETE operator-а. rbac_role_operators такого не имеет, поэтому различие
    -- явное, не случайное.
    CONSTRAINT synod_operators_aid_fk
        FOREIGN KEY (aid) REFERENCES operators (aid) ON DELETE CASCADE,
    CONSTRAINT synod_operators_added_by_aid_fk
        FOREIGN KEY (added_by_aid) REFERENCES operators (aid)
);

COMMENT ON TABLE synod_operators IS
    'Membership «Synod ↔ архон» — ADR-049. ON DELETE CASCADE с synods и operators.';

-- Индекс «AID → Synod-ы» для построения снимка enforcer-а: разворот membership-а
-- архона в его группы идёт по aid, не по PK-порядку (synod_name, aid).
CREATE INDEX synod_operators_aid_idx
    ON synod_operators (aid);

CREATE TABLE synod_roles (
    synod_name     TEXT        NOT NULL,
    role_name      TEXT        NOT NULL,
    granted_at     TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    granted_by_aid TEXT,                                             -- FK на operators(aid); NULL у seed-/bootstrap-bundle-а

    PRIMARY KEY (synod_name, role_name),
    CONSTRAINT synod_roles_synod_fk
        FOREIGN KEY (synod_name) REFERENCES synods (name) ON DELETE CASCADE,
    CONSTRAINT synod_roles_role_fk
        FOREIGN KEY (role_name) REFERENCES rbac_roles (name) ON DELETE CASCADE,
    CONSTRAINT synod_roles_granted_by_aid_fk
        FOREIGN KEY (granted_by_aid) REFERENCES operators (aid)
);

COMMENT ON TABLE synod_roles IS
    'Bundle «Synod ↔ роль» — ADR-049. CASCADE с обеих сторон: DELETE synod чистит bundle, DELETE роли снимает её из всех Synod-ов.';
