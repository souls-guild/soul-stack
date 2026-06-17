-- 019_create_providers.up.sql
--
-- Реестр Cloud-Provider-ов (ADR-017, docs/keeper/cloud.md). Provider —
-- managed-через-API запись: какой CloudDriver-плагин (`type`), в каком
-- регионе и где брать credentials. Сам secret в БД НЕ хранится — только
-- vault-ref (`credentials_ref` = `vault:<path>`).
--
-- PK — `name` (kebab-case, уникальное в кластере). `type` — имя
-- CloudDriver-плагина из keeper.yml::plugins.cloud_drivers[].name;
-- соответствие проверяется на service-слое (Cloud.CRUD.b), здесь — только
-- формат kebab.
--
-- FK:
--   - created_by_aid → operators(aid) ON DELETE SET NULL (запись Provider-а
--     переживает удаление оператора; симметрично incarnation/apply_runs).

CREATE TABLE providers (
    name            TEXT        PRIMARY KEY,
    type            TEXT        NOT NULL,
    region          TEXT        NOT NULL,
    credentials_ref TEXT        NOT NULL,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    created_by_aid  TEXT,

    CONSTRAINT providers_name_format
        CHECK (name ~ '^[a-z0-9-]{1,63}$'),
    CONSTRAINT providers_type_format
        CHECK (type ~ '^[a-z0-9-]{1,63}$'),
    CONSTRAINT providers_created_by_aid_fk
        FOREIGN KEY (created_by_aid) REFERENCES operators (aid) ON DELETE SET NULL
);

-- Лента Provider-ов конкретного оператора (audit / триаж).
CREATE INDEX providers_created_by_aid_idx
    ON providers (created_by_aid);

COMMENT ON TABLE providers IS
    'Реестр Cloud-Provider-ов (ADR-017). credentials_ref = vault:<path>, secret в БД не пишется.';
