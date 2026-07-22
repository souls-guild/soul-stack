-- 071_create_heralds_tidings.up.sql
--
-- ADR-052 (Herald + Tiding - notifications about run events), slice S1.
--
-- Two entities managed via API/MCP (Omen/Rite pattern, migrations 032/033):
--   - heralds - registry of notification delivery CHANNELS ("where to send").
--   - tidings - registry of subscription RULES ("what to react to -> with which Herald").
--
-- Delivery/tap/notification-dispatcher are the following slices (S2-S4); here it's only
-- storage + the CRUD layer (keeper/internal/herald).

-- heralds - a delivery channel. PK name (kebab-case). type - closed enum (webhook in
-- MVP; slack/email - additive post-MVP without a breaking change, new CHECK values).
--
-- config (JSONB) - per-type channel configuration: for webhook - `url` (required)
-- + optional `headers` + optional security flags `http_allowed`/`allow_private`
-- (an explicit opt-out of the SSRF guard, the core.url pattern, ADR-052(e)). The shape is validated
-- at the service layer (herald.validateConfig) - JSONB can't be matched against type
-- with a declarative CHECK without a trigger, same as omens.auth_ref / push_providers.params.
--
-- secret_ref (vault-ref, nullable) - the channel's secret (webhook signing token). NOT
-- every webhook needs a signature, hence NULL-able (unlike omens.auth_ref
-- NOT NULL: there a credential to an external system is always required). The master credential is
-- NOT stored in the DB - only a vault-ref (ADR-052(e), the omens.auth_ref / core.url pattern).
-- The vault-ref format is NOT caught by the CHECK - that's done by the service layer (vault.ParseRef).
--
-- FK:
--   - created_by_aid -> operators(aid) ON DELETE SET NULL (a Herald record
--     survives operator deletion; symmetric to omens/providers). NULL-able.

CREATE TABLE heralds (
    name           TEXT        PRIMARY KEY,
    type           TEXT        NOT NULL,
    config         JSONB       NOT NULL DEFAULT '{}'::jsonb,
    secret_ref     TEXT,
    enabled        BOOLEAN     NOT NULL DEFAULT true,
    created_at     TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at     TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    created_by_aid TEXT,

    CONSTRAINT heralds_name_format
        CHECK (name ~ '^[a-z0-9-]{1,63}$'),
    CONSTRAINT heralds_type_enum
        CHECK (type IN ('webhook')),
    CONSTRAINT heralds_created_by_aid_fk
        FOREIGN KEY (created_by_aid) REFERENCES operators (aid) ON DELETE SET NULL
);

COMMENT ON TABLE heralds IS
    'Registry of Herald notification delivery channels (ADR-052). type closed-enum (webhook MVP), config JSONB (webhook: url+headers+opt-out flags), secret_ref = vault-ref (channel secret not stored in the DB).';

-- tidings - a subscription rule. PK name (kebab-case). event_types - non-empty
-- TEXT[] of audit event-types with area-glob support (`scenario_run.*`); the shape
-- (a known type OR a run-area glob) is validated at the service layer - the
-- EventType constant catalog evolves, a DB CHECK against it would be fragile (like
-- omens.auth_ref / rites.allow). The CHECK here only catches the invariant "list
-- non-empty" (cardinality, available declaratively).
--
-- only_failures / only_changes - event filters (ADR-052(c)). incarnation /
-- cadence - optional selectors binding to the run source (NULL = no filter).
--
-- FK:
--   - herald -> heralds(name) ON DELETE CASCADE: a Tiding without a Herald
--     makes no sense, removing the channel atomically takes its subscriptions with it (ADR-052(a),
--     naming-rules.md; symmetric to rites.omen ON DELETE CASCADE).
--   - created_by_aid -> operators(aid) ON DELETE SET NULL (same as heralds).

CREATE TABLE tidings (
    name           TEXT        PRIMARY KEY,
    herald         TEXT        NOT NULL,
    event_types    TEXT[]      NOT NULL,
    only_failures  BOOLEAN     NOT NULL DEFAULT false,
    only_changes   BOOLEAN     NOT NULL DEFAULT false,
    incarnation    TEXT,
    cadence        TEXT,
    enabled        BOOLEAN     NOT NULL DEFAULT true,
    created_at     TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at     TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    created_by_aid TEXT,

    CONSTRAINT tidings_name_format
        CHECK (name ~ '^[a-z0-9-]{1,63}$'),
    CONSTRAINT tidings_event_types_nonempty
        CHECK (cardinality(event_types) > 0),
    CONSTRAINT tidings_herald_fk
        FOREIGN KEY (herald) REFERENCES heralds (name) ON DELETE CASCADE,
    CONSTRAINT tidings_created_by_aid_fk
        FOREIGN KEY (created_by_aid) REFERENCES operators (aid) ON DELETE SET NULL
);

-- Lookup of all Tiding rules for one Herald (CRUD list-by-herald + cascade audit).
CREATE INDEX tidings_herald_idx
    ON tidings (herald);

-- Dispatcher (S2) matches an event only against ENABLED rules. A partial index
-- on enabled=true - the hot match path doesn't scan disabled subscriptions.
CREATE INDEX tidings_enabled_idx
    ON tidings (enabled) WHERE enabled = true;

COMMENT ON TABLE tidings IS
    'Registry of Tiding notification subscription rules (ADR-052). event_types area-glob + only_failures/only_changes filters + incarnation/cadence selectors. herald ON DELETE CASCADE.';
