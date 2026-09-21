-- 069_create_synods.up.sql
--
-- Synod is a group of archons (ADR-049, docs/architecture.md -> ADR-049).
-- An intermediate level between "operator" and "role": the model is Archon -> Synod -> Roles.
-- Three tables follow the same rbac_* pattern (ADR-028, migration 026):
--   - synods           - catalog of groups (symmetric to rbac_roles: catalog + builtin);
--   - synod_operators  - "Synod <-> archon" membership (symmetric to rbac_role_operators);
--   - synod_roles      - "Synod <-> role" bundle (a new level - the group's set of roles).
--
-- An archon's effective roles = direct (rbac_role_operators) ∪ roles via all of their
-- Synods - the union is assembled in the enforcer's snapshot build (ADR-049(e)).
--
-- IMPORTANT (ADR-049(f)): the least-privilege subset and self-lockout checks MUST account
-- for roles via Synod. This is slice S2 (security-SQL) - at the time of this migration
-- subset/self-lockout still only count direct roles (a known gap).

CREATE TABLE synods (
    name           TEXT        PRIMARY KEY,
    description    TEXT        NOT NULL DEFAULT '',
    builtin        BOOLEAN     NOT NULL DEFAULT false,                -- builtin=true forbids synod.delete (symmetric to rbac_roles.builtin)
    created_at     TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    created_by_aid TEXT,                                             -- FK to operators(aid); NULL for seed groups with no initiating Archon

    CONSTRAINT synods_name_format CHECK (name ~ '^[a-z][a-z0-9-]*$'),
    CONSTRAINT synods_created_by_aid_fk
        FOREIGN KEY (created_by_aid) REFERENCES operators (aid)
);

COMMENT ON TABLE synods IS
    'Catalog of Synod groups -- ADR-049. PK = name (kebab-case). builtin=true protects against synod.delete.';

CREATE TABLE synod_operators (
    synod_name   TEXT        NOT NULL,
    aid          TEXT        NOT NULL,
    added_at     TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    added_by_aid TEXT,                                               -- FK to operators(aid); NULL for seed/bootstrap membership

    PRIMARY KEY (synod_name, aid),
    CONSTRAINT synod_operators_synod_fk
        FOREIGN KEY (synod_name) REFERENCES synods (name) ON DELETE CASCADE,
    -- CASCADE is deliberate (unlike rbac_role_operators_aid_fk, which has NO CASCADE):
    -- deleting an operator auto-cleans up their Synod membership. Operators aren't actually
    -- deleted (revoke = revoked_at, ADR-014), but on a hard delete (tests/cleanup),
    -- orphaned synod_operators rows are not allowed - an FK without CASCADE would have
    -- blocked DELETE on the operator. rbac_role_operators has no such case, so the
    -- difference is deliberate, not accidental.
    CONSTRAINT synod_operators_aid_fk
        FOREIGN KEY (aid) REFERENCES operators (aid) ON DELETE CASCADE,
    CONSTRAINT synod_operators_added_by_aid_fk
        FOREIGN KEY (added_by_aid) REFERENCES operators (aid)
);

COMMENT ON TABLE synod_operators IS
    'Membership "Synod <-> archon" -- ADR-049. ON DELETE CASCADE with synods and operators.';

-- The "AID -> Synods" index is for building the enforcer's snapshot: expanding an
-- archon's membership into their groups is done by aid, not by PK order (synod_name, aid).
CREATE INDEX synod_operators_aid_idx
    ON synod_operators (aid);

CREATE TABLE synod_roles (
    synod_name     TEXT        NOT NULL,
    role_name      TEXT        NOT NULL,
    granted_at     TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    granted_by_aid TEXT,                                             -- FK to operators(aid); NULL for seed/bootstrap bundles

    PRIMARY KEY (synod_name, role_name),
    CONSTRAINT synod_roles_synod_fk
        FOREIGN KEY (synod_name) REFERENCES synods (name) ON DELETE CASCADE,
    CONSTRAINT synod_roles_role_fk
        FOREIGN KEY (role_name) REFERENCES rbac_roles (name) ON DELETE CASCADE,
    CONSTRAINT synod_roles_granted_by_aid_fk
        FOREIGN KEY (granted_by_aid) REFERENCES operators (aid)
);

COMMENT ON TABLE synod_roles IS
    'Bundle "Synod <-> role" -- ADR-049. CASCADE on both sides: DELETE synod cleans up the bundle, DELETE role removes it from all Synods.';
