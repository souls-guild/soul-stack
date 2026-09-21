-- 033_create_rites.up.sql
--
-- Registry of Rites -- Augur grants (ADR-025, docs/keeper/augur.md -> §4.2).
-- A Rite links a subject (Coven XOR a specific SID) to an Omen, an allow-list,
-- and a delivery mode (`delegate`): "such-and-such subject can, through Augur,
-- fetch such-and-such values from such-and-such Omen, in such-and-such mode."
--
-- Surrogate PK (`id`, GENERATED ALWAYS AS IDENTITY) -- a single subject can
-- have several Rites for one Omen (different allow-lists / modes), so pairs
-- (omen, subject) are not unique. Precedent for an IDENTITY PK -- plugin_sigils (028).
--
-- Subject is strictly XOR: exactly one of coven / sid is non-empty (CHECK
-- rites_subject_xor). A coven-Rite applies to all Souls with that label;
-- a sid-Rite -- to a single host. The coven format is only checked when
-- coven is set (NULL-tolerant CHECK).
--
-- allow (JSONB) -- the allow-list of permitted values; its shape depends on
-- the Omen's source_type (vault: paths/policies, prometheus: queries, elk:
-- indices, augur.md §4.2). The shape is validated at the service layer
-- (JSONB can't be matched against another Omen's source_type with a
-- declarative CHECK without a trigger).
--
-- delegate -- the boundary between MVP phases: false (broker, MVP-1, data
-- flows through Keeper) / true (delegation, MVP-2, Soul reaches out itself
-- with an ephemeral scoped credential). Default false -- "security first",
-- delegation is an explicit opt-in.
--
-- token_ttl / token_num_uses -- parameters of the minted scoped Vault token,
-- meaningful ONLY for a vault-Omen with delegate=true. CHECK
-- rites_token_fields_vault_only only catches the implication "token fields
-- set => delegate=true" (available within the rites row). The other half of
-- the invariant -- "=> source_type=vault" -- requires a join to omens and is
-- validated at the service layer (augur.md §4.2). token_ttl is a duration
-- string (config.ParseDuration), validated at the service layer, not by a
-- CHECK.
--
-- FK:
--   - omen -> omens(name) ON DELETE CASCADE: a Rite without an Omen is
--     meaningless, deleting an Omen atomically removes all of its grants
--     (augur.md §9 fork).
--   - created_by_aid -> operators(aid) ON DELETE SET NULL (the Rite record
--     survives the operator's deletion; symmetric with omens/providers).

CREATE TABLE rites (
    id             BIGINT      GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    omen           TEXT        NOT NULL,
    coven          TEXT,
    sid            TEXT,
    allow          JSONB       NOT NULL,
    delegate       BOOLEAN     NOT NULL DEFAULT false,
    token_ttl      TEXT,
    token_num_uses INT,
    created_at     TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    created_by_aid TEXT,

    CONSTRAINT rites_omen_fk
        FOREIGN KEY (omen) REFERENCES omens (name) ON DELETE CASCADE,
    -- Subject is strictly XOR: exactly one of coven / sid is non-empty.
    CONSTRAINT rites_subject_xor
        CHECK ((coven IS NOT NULL) <> (sid IS NOT NULL)),
    CONSTRAINT rites_coven_format
        CHECK (coven IS NULL OR coven ~ '^[a-z0-9][a-z0-9-]*$'),
    -- token fields are allowed only when delegate=true (=>vault checked at the service layer).
    CONSTRAINT rites_token_fields_vault_only
        CHECK ((token_ttl IS NULL AND token_num_uses IS NULL) OR delegate = true),
    CONSTRAINT rites_created_by_aid_fk
        FOREIGN KEY (created_by_aid) REFERENCES operators (aid) ON DELETE SET NULL
);

-- Lookup of all Rites for a single Omen (authorization §6, list-by-omen).
CREATE INDEX rites_omen_idx
    ON rites (omen);

-- Lookup of sid-Rites by a specific SID (authorization §6.3). Partial:
-- we index only the populated sid values (XOR => half the rows have sid=NULL).
CREATE INDEX rites_sid_idx
    ON rites (sid) WHERE sid IS NOT NULL;

-- Lookup of coven-Rites by Coven label (authorization §6.3). Partial for the
-- same reason as rites_sid_idx.
CREATE INDEX rites_coven_idx
    ON rites (coven) WHERE coven IS NOT NULL;

COMMENT ON TABLE rites IS
    'Augur grants (ADR-025): subject (coven XOR sid) x omen -> allow + delegate + token parameters. omen ON DELETE CASCADE.';
