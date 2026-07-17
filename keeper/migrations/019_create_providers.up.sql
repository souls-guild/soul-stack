-- 019_create_providers.up.sql
--
-- Registry of Cloud Providers (ADR-017, docs/keeper/cloud.md). A Provider is
-- a managed-via-API record: which CloudDriver plugin (`type`), which
-- region, and where to fetch credentials. The secret itself is NOT stored
-- in the DB -- only a vault-ref (`credentials_ref` = `vault:<path>`).
--
-- PK is `name` (kebab-case, unique in the cluster). `type` is the name
-- of the CloudDriver plugin from keeper.yml::plugins.cloud_drivers[].name;
-- the match is validated at the service layer (Cloud.CRUD.b), here it's
-- only the kebab format.
--
-- FK:
--   - created_by_aid -> operators(aid) ON DELETE SET NULL (the Provider
--     record survives the operator's deletion; symmetric with
--     incarnation/apply_runs).

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

-- Feed of Providers for a specific operator (audit / triage).
CREATE INDEX providers_created_by_aid_idx
    ON providers (created_by_aid);

COMMENT ON TABLE providers IS
    'Registry of Cloud Providers (ADR-017). credentials_ref = vault:<path>, secret is not written to the DB.';
