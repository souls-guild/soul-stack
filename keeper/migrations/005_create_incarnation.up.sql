-- 005_create_incarnation.up.sql
--
-- Реестр incarnation-ов (runtime-инстансов сервисов) под ADR-009.
-- Каждая incarnation — runtime-проекция Service (git) в Postgres со
-- spec (декларированное оператором) + state (актуальное представление
-- после прогонов) + status (узкий MVP-enum: ready/applying/error_locked/
-- migration_failed; provisioning/drift/destroying — пост-MVP).
--
-- PK = `name` (kebab-case, оно же корневая Coven-метка по ADR-008).
-- service_version — git-ref (tag/branch) Service-репо по ADR-007.
-- state_schema_version — версия структуры `state` для миграций по ADR-019.
--
-- FK `created_by_aid` ссылается на `operators(aid)` (ADR-014). При
-- удалении оператора — ON DELETE SET NULL (история incarnation важнее
-- ссылочной целостности; revoke — обычный путь, удаление — редкое).

CREATE TABLE incarnation (
    name                  TEXT        PRIMARY KEY,
    service               TEXT        NOT NULL,
    service_version       TEXT        NOT NULL,
    state_schema_version  INTEGER     NOT NULL DEFAULT 1,
    spec                  JSONB       NOT NULL DEFAULT '{}'::jsonb,
    state                 JSONB       NOT NULL DEFAULT '{}'::jsonb,
    status                TEXT        NOT NULL,
    status_details        JSONB,
    created_by_aid        TEXT,
    created_at            TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at            TIMESTAMPTZ NOT NULL DEFAULT NOW(),

    CONSTRAINT incarnation_name_format
        CHECK (name ~ '^[a-z0-9][a-z0-9-]{0,62}$'),
    CONSTRAINT incarnation_status_valid
        CHECK (status IN ('ready', 'applying', 'error_locked', 'migration_failed')),
    CONSTRAINT incarnation_created_by_aid_fk
        FOREIGN KEY (created_by_aid) REFERENCES operators (aid) ON DELETE SET NULL
);

-- Partial индекс для фильтрации списка по сервису (типовой запрос
-- `GET /v1/incarnations?service=...`).
CREATE INDEX incarnation_service_idx
    ON incarnation (service);

-- Partial индекс для фильтрации по статусу — короткий enum, типичный
-- запрос — «все error_locked / migration_failed для триажа».
CREATE INDEX incarnation_status_idx
    ON incarnation (status);

COMMENT ON TABLE incarnation IS
    'Реестр runtime-инстансов Service (ADR-009). PK = name (Coven-метка).';
