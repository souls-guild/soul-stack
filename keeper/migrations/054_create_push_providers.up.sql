-- 054_create_push_providers.up.sql
--
-- ADR-032 amendment 2026-05-26 → S7-2: push_providers PG-table.
--
-- Long-term canon replacing keeper.yml::push.providers[] inline (pilot S6 / S7-1).
-- Stores per-provider params for push-flow SSH plugins: keeper serializes
-- `params` to JSON when spawning the plugin and injects it into the
-- env variable `SOUL_SSH_<UPPER_SNAKE
-- (name)>_PARAMS` (ADR-020 amendment l, env-convention).
--
-- Hot-reload: changes via CRUD endpoints POST/PUT/DELETE /v1/push-providers
-- + Redis pub/sub `push-providers:changed` -> SshDispatcher re-spawns the
-- plugin on the next RPC (spawn-on-change).
--
-- Priority: PG > keeper.yml (DB is the source of truth, yml is a fallback
-- under the flag push.allow_legacy_push_providers=true with a 1-release
-- WARN-deprecation; S7-2 PM decision, symmetric with
-- push.allow_legacy_push_targets from S7-1).
--
-- Sensitive params (PM-decision S7-2 #5): `vault_addr`/`role` may go plain
-- in jsonb; actual secrets (`secret_id`/`token`/`password`/`private_key`)
-- MUST be vault-refs (`vault:<path>`) - validated at the service layer
-- (pushprovider.Service.validateSensitive), not a CHECK here (the key
-- allow-list evolves, a DB invariant would be overly rigid).
--
-- FK:
--   - created_by_aid -> operators(aid) ON DELETE NO ACTION (NOT NULL): a
--     PushProvider record always carries its initiator (managed only by
--     an Archon).
--   - updated_by_aid -> operators(aid) ON DELETE SET NULL (nullable: empty
--     on first insert).

CREATE TABLE push_providers (
    name            TEXT        PRIMARY KEY,
    params          JSONB       NOT NULL DEFAULT '{}'::jsonb,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    created_by_aid  TEXT        NOT NULL,
    updated_by_aid  TEXT,

    CONSTRAINT push_providers_name_format
        CHECK (name ~ '^[a-z][a-z0-9-]{0,62}$'),
    CONSTRAINT push_providers_created_by_aid_fk
        FOREIGN KEY (created_by_aid) REFERENCES operators (aid),
    CONSTRAINT push_providers_updated_by_aid_fk
        FOREIGN KEY (updated_by_aid) REFERENCES operators (aid) ON DELETE SET NULL
);

-- Most recent changes on top (triage): UI "recently changed providers".
CREATE INDEX push_providers_updated_idx ON push_providers (updated_at DESC);

COMMENT ON TABLE push_providers IS
    'Per-provider params for push-flow SSH plugins (ADR-032 amendment 2026-05-26, S7-2). Long-term canon replacing keeper.yml::push.providers[]. Hot-reload via Redis pub/sub push-providers:changed.';
