-- 005_create_incarnation.up.sql
--
-- Registry of incarnations (runtime instances of services) under ADR-009.
-- Each incarnation is a runtime projection of a Service (git) into Postgres with
-- spec (declared by the operator) + state (the actual representation
-- after runs) + status (a narrow MVP enum: ready/applying/error_locked/
-- migration_failed; provisioning/drift/destroying are post-MVP).
--
-- PK = `name` (kebab-case, also the root Coven label per ADR-008).
-- service_version is the git ref (tag/branch) of the Service repo per ADR-007.
-- state_schema_version is the version of the `state` structure for migrations per ADR-019.
--
-- FK `created_by_aid` references `operators(aid)` (ADR-014). On operator
-- deletion - ON DELETE SET NULL (incarnation history matters more than
-- referential integrity; revoke is the normal path, deletion is rare).

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

-- Partial index for filtering the list by service (a typical
-- `GET /v1/incarnations?service=...` query).
CREATE INDEX incarnation_service_idx
    ON incarnation (service);

-- Partial index for filtering by status - a short enum, a typical
-- query is "all error_locked / migration_failed for triage".
CREATE INDEX incarnation_status_idx
    ON incarnation (status);

COMMENT ON TABLE incarnation IS
    'Registry of runtime instances of Service (ADR-009). PK = name (Coven label).';
