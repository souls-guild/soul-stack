-- 003_create_operators.up.sql
--
-- Реестр Архонтов (операторов Soul Stack) под ADR-014. PK — `aid`
-- (kebab-case, `archon-<...>`), уникальный partial unique index по
-- `created_by_aid IS NULL` гарантирует ровно одного bootstrap-Archon-а
-- (ADR-013 + ADR-014).
--
-- FK `created_by_aid` ссылается на эту же таблицу (рекурсивно): первый
-- Архонт имеет `created_by_aid = NULL`, все последующие создаются
-- кем-то конкретным.
--
-- `audit_log.archon_aid` получит FK на operators(aid) отдельной
-- миграцией 004 (создавать FK до существования таблицы нельзя).

CREATE TABLE operators (
    aid             TEXT        PRIMARY KEY,
    display_name    TEXT        NOT NULL,
    auth_method     TEXT        NOT NULL,                              -- enum: jwt | mtls | combined (MVP: только jwt)
    created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    created_by_aid  TEXT,                                              -- FK на operators(aid); NULL только у первого Archon-а
    revoked_at      TIMESTAMPTZ,                                       -- nullable; non-NULL означает «отозван»
    metadata        JSONB       NOT NULL DEFAULT '{}'::jsonb,

    CONSTRAINT aid_format CHECK (aid ~ '^archon-[a-z0-9-]{1,62}$'),
    CONSTRAINT auth_method_valid CHECK (auth_method IN ('jwt', 'mtls', 'combined')),
    CONSTRAINT self_reference_ok CHECK (created_by_aid IS NULL OR created_by_aid <> aid),
    CONSTRAINT created_by_aid_fk FOREIGN KEY (created_by_aid) REFERENCES operators (aid)
);

-- Partial unique index: только ОДИН Archon может иметь
-- `created_by_aid IS NULL` (первый bootstrap-Archon). Гарантия из
-- ADR-014: повторный bootstrap при non-пустой таблице невозможен.
CREATE UNIQUE INDEX operators_first_archon_idx
    ON operators ((1))
    WHERE created_by_aid IS NULL;

-- Partial index для быстрой выборки активных (не-revoked) операторов:
-- 99% запросов RBAC-проверки идут по «active set».
CREATE INDEX operators_revoked_at_idx
    ON operators (revoked_at)
    WHERE revoked_at IS NULL;

COMMENT ON TABLE operators IS
    'Реестр Архонтов (операторов Soul Stack) — ADR-014. PK = aid (kebab-case).';
