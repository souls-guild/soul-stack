-- 020_create_profiles.up.sql
--
-- Registry of Cloud Profiles (ADR-017, docs/keeper/cloud.md). Profile - a VM spec
-- layered on top of a specific Provider: `params` (jsonb, an arbitrary VM spec,
-- validated against CloudDriver.Schema at the service layer - Cloud.CRUD.b) +
-- optional `cloud_init` (raw userdata).
--
-- FK:
--   - provider → providers(name) ON DELETE RESTRICT (PM decision: not
--     CASCADE - protects against data loss; deleting a Provider with
--     dependent Profiles requires explicitly deleting the profiles first).
--   - created_by_aid → operators(aid) ON DELETE SET NULL (the record survives
--     the operator being removed).

CREATE TABLE profiles (
    name           TEXT        PRIMARY KEY,
    provider       TEXT        NOT NULL,
    params         JSONB       NOT NULL,
    cloud_init     TEXT,
    created_at     TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    created_by_aid TEXT,

    CONSTRAINT profiles_name_format
        CHECK (name ~ '^[a-z0-9-]{1,63}$'),
    CONSTRAINT profiles_provider_fk
        FOREIGN KEY (provider) REFERENCES providers (name) ON DELETE RESTRICT,
    CONSTRAINT profiles_created_by_aid_fk
        FOREIGN KEY (created_by_aid) REFERENCES operators (aid) ON DELETE SET NULL
);

-- Resolves Profiles belonging to a given Provider (SelectByProvider, dependency
-- check before deleting a Provider).
CREATE INDEX profiles_provider_idx
    ON profiles (provider);

-- Feed of Profiles created by a given operator (audit / triage).
CREATE INDEX profiles_created_by_aid_idx
    ON profiles (created_by_aid);

COMMENT ON TABLE profiles IS
    'Registry of Cloud Profiles (ADR-017). provider FK ON DELETE RESTRICT, params jsonb (VM spec).';
