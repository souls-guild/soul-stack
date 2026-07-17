-- 041_create_oracle.up.sql
--
-- Registries of the Oracle circuit (ADR-030, beacons + reactor, slice S2): `vigils`
-- (Soul-side checks, handed to the host via VigilSnapshot) + `decrees`
-- (reactor rules "event -> scenario", default-deny) + `oracle_fires`
-- (per-(decree, subject) cooldown state, loop prevention).
--
-- The storage pattern is borrowed from Augur (migrations 032 omens / 033 rites, ADR-025):
-- records managed via API, subject `coven` XOR `sid` (like Rite), FK
-- created_by_aid -> operators ON DELETE SET NULL (the record survives deletion
-- of the operator). OpenAPI/MCP CRUD + RBAC perms are a separate slice S3, here only
-- the schema + repository.

-- ---------------------------------------------------------------------------
-- vigils - registry of Soul-side checks (beacon definitions).
--
-- PK - `name` (kebab-case, unique cluster-wide; travels back in
-- PortentEvent.beacon_name). `check_addr` - the core-beacon address
-- (`core.beacon.file_changed` / `core.beacon.service_down` / ...; maps to
-- VigilDef.check; the column is NOT named `check` - that's a reserved PG keyword).
-- `interval_spec` - duration convention (config.ParseDuration), validated at the
-- service layer (the column is NOT named `interval` - that's a PG type name, requiring
-- quoting; maps to VigilDef.interval). `params` - JSONB parameters
-- of the check (path / service-name / threshold), shape depends on check_addr,
-- validated at the service layer (like rites.allow against source_type).
--
-- Subject - strictly XOR (like Rite): exactly one of coven / sid is non-empty. coven -
-- a text[] of labels (the Vigil is handed to every Soul with any of these labels); sid -
-- one specific host. coven labels in kebab-case are validated at the
-- service layer (a per-element CHECK for text[] without a trigger can't be
-- expressed declaratively).
--
-- enabled - toggle (managed via API, S3); default true. Read-only by
-- Vigil's construction is guaranteed on the Soul side (S1), not by the schema.
CREATE TABLE vigils (
    name           TEXT        PRIMARY KEY,
    coven          TEXT[],
    sid            TEXT,
    interval_spec  TEXT        NOT NULL,
    check_addr     TEXT        NOT NULL,
    params         JSONB       NOT NULL DEFAULT '{}'::jsonb,
    enabled        BOOLEAN     NOT NULL DEFAULT true,
    created_at     TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at     TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    created_by_aid TEXT,

    CONSTRAINT vigils_name_format
        CHECK (name ~ '^[a-z0-9-]{1,63}$'),
    -- Subject - strictly XOR: exactly one of coven / sid is non-empty. coven=NULL and
    -- coven='{}' are both treated as "not set" (an empty array is meaningless as a
    -- subject): require a non-empty array when coven is set.
    CONSTRAINT vigils_subject_xor
        CHECK ((coven IS NOT NULL AND array_length(coven, 1) > 0) <> (sid IS NOT NULL)),
    CONSTRAINT vigils_created_by_aid_fk
        FOREIGN KEY (created_by_aid) REFERENCES operators (aid) ON DELETE SET NULL
);

-- Lookup active Vigils by SID subject (VigilSnapshot resolution on connect).
CREATE INDEX vigils_sid_idx
    ON vigils (sid) WHERE sid IS NOT NULL;

-- Lookup active Vigils by coven labels (GIN over text[] for the && operator).
CREATE INDEX vigils_coven_idx
    ON vigils USING GIN (coven) WHERE coven IS NOT NULL;

COMMENT ON TABLE vigils IS
    'Registry of Soul-side checks for the Oracle circuit (ADR-030). Subject coven XOR sid; the active set travels to the host via VigilSnapshot (ReplaceAll).';

-- ---------------------------------------------------------------------------
-- decrees - registry of reactor rules (Decree). Default-deny: no matching
-- Decree -> the event triggers no action.
--
-- PK - `name` (kebab-case). `on_beacon` - the name of the Vigil whose Portent the rule
-- reacts to (matched against PortentEvent.beacon_name). `where_cel` - an optional
-- predicate over the event payload (event.data); empty -> always matches (the subject
-- has already filtered).
--
-- Subject - strictly XOR (like Rite): subject_coven (text[]) OR subject_sid.
-- Restricts which hosts can trigger the rule at all (security layer,
-- ADR-030(b)).
--
-- incarnation_name - the target incarnation of the reaction (DECISION #1, variant b):
-- the scenario's ServiceRef is resolved FROM it at enqueue time (incarnation.service ->
-- service registry), instead of being duplicated in the Decree. Same format as
-- incarnation.name (migration 005, CHECK incarnation_name_format) - it's also
-- the root Coven label (ADR-008): the subject membership check at enqueue time
-- boils down to incarnation_name being a member of the sender's covens. WITHOUT an FK to incarnation -
-- Decree is a managed registry that can outlive incarnation recreation; existence
-- is checked at enqueue fail-closed (incarnation not found -> skip + warn).
-- No index needed: the hot path goes by on_beacon, incarnation_name is only read
-- for Decrees that have already matched.
--
-- action_scenario - a named scenario (whitelist; a raw command was rejected as an
-- RCE vector, ADR-030(b)). action_input - the JSONB input to the scenario (vault-ref AS
-- IS, invariant A ADR-027). cooldown - duration convention, the minimum
-- interval between firings per-(decree, subject); validated at the
-- service layer.
CREATE TABLE decrees (
    name             TEXT        PRIMARY KEY,
    on_beacon        TEXT        NOT NULL,
    where_cel        TEXT,
    subject_coven    TEXT[],
    subject_sid      TEXT,
    incarnation_name TEXT        NOT NULL,
    action_scenario  TEXT        NOT NULL,
    action_input     JSONB       NOT NULL DEFAULT '{}'::jsonb,
    cooldown         TEXT        NOT NULL DEFAULT '0s',
    enabled          BOOLEAN     NOT NULL DEFAULT true,
    created_at       TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at       TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    created_by_aid   TEXT,

    CONSTRAINT decrees_name_format
        CHECK (name ~ '^[a-z0-9-]{1,63}$'),
    CONSTRAINT decrees_incarnation_name_format
        CHECK (incarnation_name ~ '^[a-z0-9][a-z0-9-]{0,62}$'),
    CONSTRAINT decrees_scenario_format
        CHECK (action_scenario ~ '^[a-z][a-z0-9_]*$'),
    -- Subject - strictly XOR (like Rite): exactly one of subject_coven /
    -- subject_sid is non-empty.
    CONSTRAINT decrees_subject_xor
        CHECK ((subject_coven IS NOT NULL AND array_length(subject_coven, 1) > 0) <> (subject_sid IS NOT NULL)),
    CONSTRAINT decrees_created_by_aid_fk
        FOREIGN KEY (created_by_aid) REFERENCES operators (aid) ON DELETE SET NULL
);

-- Lookup Decree by on_beacon (hot path of the match flow: Oracle does a
-- SELECT ... WHERE on_beacon = $1 AND enabled for every Portent).
CREATE INDEX decrees_on_beacon_idx
    ON decrees (on_beacon) WHERE enabled;

COMMENT ON TABLE decrees IS
    'Registry of Oracle reactor rules (ADR-030). Default-deny; subject coven XOR sid; target incarnation_name (ServiceRef is resolved from it); action = named scenario (whitelist).';

-- ---------------------------------------------------------------------------
-- oracle_fires - cooldown state per-(decree, subject), loop prevention
-- (ADR-030(a)).
--
-- A last_fired_at column on decrees doesn't fit: a single Decree fires for
-- MANY subjects (a coven-Decree covers dozens of hosts), cooldown is needed
-- per-(decree, subject), not per-decree. A separate table with PK
-- (decree, subject) - minimal structure: exactly one row per pair
-- (UPSERT ON CONFLICT, NOT append-only), read by the match flow before enqueue,
-- written after firing.
--
-- subject here - the authoritative SID of the sending host (from the mTLS peer cert, NOT
-- a payload echo): cooldown is tied to the specific host the
-- scenario is being applied to.
--
-- decree -> decrees(name) ON DELETE CASCADE: cooldown state without a Decree
-- is meaningless; deleting a Decree atomically clears its firing history.
CREATE TABLE oracle_fires (
    decree     TEXT        NOT NULL,
    subject    TEXT        NOT NULL,
    fired_at   TIMESTAMPTZ NOT NULL DEFAULT NOW(),

    CONSTRAINT oracle_fires_pk
        PRIMARY KEY (decree, subject),
    CONSTRAINT oracle_fires_decree_fk
        FOREIGN KEY (decree) REFERENCES decrees (name) ON DELETE CASCADE
);

COMMENT ON TABLE oracle_fires IS
    'Oracle cooldown state per-(decree, subject) (ADR-030(a), loop-prevention). UPSERT, one row per pair; decree ON DELETE CASCADE.';
