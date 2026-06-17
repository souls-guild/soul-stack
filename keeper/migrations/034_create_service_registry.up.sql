-- 034_create_service_registry.up.sql
--
-- Реестр Service-ов в Postgres (ADR-028-style managed-через-API registry,
-- симметрично RBAC). Переносит каталог `services[]` из статического keeper.yml
-- в managed-через-OpenAPI/MCP таблицу: запись Service-а появляется/меняется
-- только через явную операцию Архонта, видна всем нодам кластера и переживает
-- рестарт без правки конфига.
--
-- Колонки 1:1 с прежней config.ServiceRegistryEntry:
--   - name    — PK (kebab-case), уникальное имя Service-а в кластере;
--   - git     — git-источник Service-репо (непустой);
--   - ref     — git ref (tag/branch) по ADR-007 (непустой; semver-range нет);
--   - refresh — duration-строка авто-refresh ("5m"); NULL = без авто-refresh.
--               Формат CHECK-ом НЕ ловится — это делает service-слой через
--               config.ParseDuration (как augur token_ttl).
--
-- FK на operators(aid) для created_by_aid / updated_by_aid — ON DELETE SET NULL:
-- запись Service-а переживает offboarding оператора-автора, audit-поле
-- обнуляется (симметрично omens/providers/incarnation). RESTRICT оставлен
-- только у security-critical rbac_roles.

CREATE TABLE service_registry (
    name           TEXT        PRIMARY KEY,
    git            TEXT        NOT NULL,
    ref            TEXT        NOT NULL,
    refresh        TEXT,                                            -- duration-строка ("5m"); NULL = без авто-refresh
    created_at     TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at     TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    created_by_aid TEXT,                                            -- FK на operators(aid); NULL у seed/без инициатора-Архонта
    updated_by_aid TEXT,                                            -- FK на operators(aid); NULL до первого update

    CONSTRAINT service_registry_name_format
        CHECK (name ~ '^[a-z][a-z0-9-]*$'),
    CONSTRAINT service_registry_git_nonempty
        CHECK (git <> ''),
    CONSTRAINT service_registry_ref_nonempty
        CHECK (ref <> ''),
    CONSTRAINT service_registry_created_by_fk
        FOREIGN KEY (created_by_aid) REFERENCES operators (aid) ON DELETE SET NULL,
    CONSTRAINT service_registry_updated_by_fk
        FOREIGN KEY (updated_by_aid) REFERENCES operators (aid) ON DELETE SET NULL
);

COMMENT ON TABLE service_registry IS
    'Managed-через-API реестр Service-ов (перенос services[] из keeper.yml). PK = name (kebab-case), ref = git ref по ADR-007.';
