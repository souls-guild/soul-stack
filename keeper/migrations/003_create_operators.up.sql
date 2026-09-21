-- 003_create_operators.up.sql
--
-- Archon registry (Soul Stack operators) per ADR-014. PK is `aid`
-- (kebab-case, `archon-<...>`); a unique partial unique index on
-- `created_by_aid IS NULL` guarantees exactly one bootstrap Archon
-- (ADR-013 + ADR-014).
--
-- FK `created_by_aid` references this same table (recursively): the first
-- Archon has `created_by_aid = NULL`, all subsequent ones are created
-- by someone specific.
--
-- `audit_log.archon_aid` will get an FK to operators(aid) in a separate
-- migration 004 (can't create the FK before the table exists).

CREATE TABLE operators (
    aid             TEXT        PRIMARY KEY,
    display_name    TEXT        NOT NULL,
    auth_method     TEXT        NOT NULL,                              -- enum: jwt | mtls | combined (MVP: jwt only)
    created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    created_by_aid  TEXT,                                              -- FK to operators(aid); NULL only for the first Archon
    revoked_at      TIMESTAMPTZ,                                       -- nullable; non-NULL means "revoked"
    metadata        JSONB       NOT NULL DEFAULT '{}'::jsonb,

    CONSTRAINT aid_format CHECK (aid ~ '^archon-[a-z0-9-]{1,62}$'),
    CONSTRAINT auth_method_valid CHECK (auth_method IN ('jwt', 'mtls', 'combined')),
    CONSTRAINT self_reference_ok CHECK (created_by_aid IS NULL OR created_by_aid <> aid),
    CONSTRAINT created_by_aid_fk FOREIGN KEY (created_by_aid) REFERENCES operators (aid)
);

-- Partial unique index: only ONE Archon can have
-- `created_by_aid IS NULL` (the first bootstrap Archon). Guarantee from
-- ADR-014: a repeat bootstrap is impossible once the table is non-empty.
CREATE UNIQUE INDEX operators_first_archon_idx
    ON operators ((1))
    WHERE created_by_aid IS NULL;

-- Partial index for fast lookup of active (non-revoked) operators:
-- 99% of RBAC-check queries go against the "active set".
CREATE INDEX operators_revoked_at_idx
    ON operators (revoked_at)
    WHERE revoked_at IS NULL;

COMMENT ON TABLE operators IS
    'Archon registry (Soul Stack operators) -- ADR-014. PK = aid (kebab-case).';
