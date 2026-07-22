-- 035_create_keeper_settings.up.sql
--
-- Cluster-wide key-value scalars for the Keeper - settings managed through the API,
-- which used to live as top-level scalars in keeper.yml. A single source of truth
-- for the whole cluster (instead of per-node config), visible to all nodes and
-- surviving a restart.
--
-- Storage is flat: key (PK, snake_case) → value (TEXT). The semantics of value and
-- the set of well-known keys live in the Go layer (serviceregistry), not in the
-- schema - the table is deliberately untyped, so adding a new setting doesn't
-- require a migration.
--
-- Well-known keys for MVP:
--   - `default_destiny_source` - the default git source for Destiny.
-- `default_module_source` is NOT introduced: it has no consumer in the keeper code
-- (a dead field from the old config).
--
-- The setting rows themselves are runtime data (written through the API), so the
-- migration does NOT insert a single well-known key: it only creates the table.
--
-- FK updated_by_aid → operators(aid) ON DELETE SET NULL: a setting record
-- survives deletion of the operator who last changed it - the field is nulled out
-- (symmetric with omens/providers; SET NULL is appropriate here since the column
-- is nullable and the setting's value matters more than its author).

CREATE TABLE keeper_settings (
    key            TEXT        PRIMARY KEY,
    value          TEXT        NOT NULL,
    updated_at     TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_by_aid TEXT        REFERENCES operators (aid) ON DELETE SET NULL,

    CONSTRAINT keeper_settings_key_format
        CHECK (key ~ '^[a-z][a-z0-9_]*$')
);

COMMENT ON TABLE keeper_settings IS
    'Cluster-wide key-value scalars for the Keeper (managed through the API). key = snake_case, value = TEXT; well-known keys live in the Go layer.';
