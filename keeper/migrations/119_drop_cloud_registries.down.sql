-- 119_drop_cloud_registries.down.sql
--
-- Reverse of 119: restore the SCHEMA the two registries had at migration 118 --
-- `id` PRIMARY KEY (118), `label` beside it (117), `fqdn_suffix` on providers
-- (094) -- and nothing else.
--
-- The DATA is deliberately NOT restored, for the same reason migration 114
-- declined to: there is nothing left to restore it from. A keeper rolled back
-- to pre-NIM-761 code starts against a schema it recognizes and two empty
-- registries, so `core.cloud.created` answers "provider not found" until an
-- operator re-creates the rows through the API, where the act is audited. A
-- Provider's `credentials_ref` is a Vault path and the Vault entry was never
-- touched, so re-creating the row against the same path recovers the
-- credential without the secret ever having been in this database.

CREATE TABLE providers (
    id              TEXT        PRIMARY KEY,
    type            TEXT        NOT NULL,
    region          TEXT        NOT NULL,
    credentials_ref TEXT        NOT NULL,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    created_by_aid  TEXT,
    label           TEXT,
    fqdn_suffix     TEXT,

    CONSTRAINT providers_id_format
        CHECK (id ~ '^[a-z0-9-]{1,63}$'),
    CONSTRAINT providers_type_format
        CHECK (type ~ '^[a-z0-9-]{1,63}$'),
    CONSTRAINT providers_fqdn_suffix_format
        CHECK (fqdn_suffix IS NULL OR
               fqdn_suffix ~ '^[a-z0-9]([a-z0-9-]*[a-z0-9])?(\.[a-z0-9]([a-z0-9-]*[a-z0-9])?)*$'),
    CONSTRAINT providers_created_by_aid_fk
        FOREIGN KEY (created_by_aid) REFERENCES operators (aid) ON DELETE SET NULL
);

CREATE INDEX providers_created_by_aid_idx
    ON providers (created_by_aid);

CREATE TABLE profiles (
    id             TEXT        PRIMARY KEY,
    provider       TEXT        NOT NULL,
    params         JSONB       NOT NULL,
    cloud_init     TEXT,
    created_at     TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    created_by_aid TEXT,
    label          TEXT,

    CONSTRAINT profiles_id_format
        CHECK (id ~ '^[a-z0-9-]{1,63}$'),
    CONSTRAINT profiles_provider_fk
        FOREIGN KEY (provider) REFERENCES providers (id) ON DELETE RESTRICT,
    CONSTRAINT profiles_created_by_aid_fk
        FOREIGN KEY (created_by_aid) REFERENCES operators (aid) ON DELETE SET NULL
);

CREATE INDEX profiles_provider_idx
    ON profiles (provider);

CREATE INDEX profiles_created_by_aid_idx
    ON profiles (created_by_aid);
