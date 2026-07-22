-- 032_create_omens.up.sql
--
-- Registry of Omens - external systems that Augur mediates Soul access to
-- (ADR-025, docs/keeper/augur.md -> section 4.1). An Omen is a managed-via-API record:
-- one Vault mount / one Prometheus / one ELK cluster. The cloud analog of Provider
-- (migration 019).
--
-- PK is `name` (kebab-case, unique within the cluster). `source_type` is a
-- descriptive closed enum (`vault` / `prometheus` / `elk`, augur.md section 7);
-- extending the enum is propose-and-wait + a PR to augur.md and naming-rules.md.
--
-- Invariant: `auth_ref` is ALWAYS a vault-ref (`vault:<mount>/<path>`) to
-- Keeper's master credential - the secret itself is NOT stored in the DB, only the reference
-- (symmetric to providers.credentials_ref). The vault-ref format is NOT caught here by a CHECK -
-- that's done by the service layer via vault.ParseRef (augur.md section 4.1).
--
-- FK:
--   - created_by_aid -> operators(aid) ON DELETE SET NULL (an Omen record
--     survives operator deletion; symmetric to providers/incarnation).

CREATE TABLE omens (
    name           TEXT        PRIMARY KEY,
    source_type    TEXT        NOT NULL,
    endpoint       TEXT        NOT NULL,
    auth_ref       TEXT        NOT NULL,
    created_at     TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    created_by_aid TEXT,

    CONSTRAINT omens_name_format
        CHECK (name ~ '^[a-z0-9-]{1,63}$'),
    CONSTRAINT omens_source_type_enum
        CHECK (source_type IN ('vault', 'prometheus', 'elk')),
    CONSTRAINT omens_created_by_aid_fk
        FOREIGN KEY (created_by_aid) REFERENCES operators (aid) ON DELETE SET NULL
);

-- Feed of Omens for a specific operator (audit / triage).
CREATE INDEX omens_created_by_aid_idx
    ON omens (created_by_aid);

COMMENT ON TABLE omens IS
    'Registry of external Augur systems (ADR-025). auth_ref = vault:<mount>/<path>, the master credential is not written to the DB.';
