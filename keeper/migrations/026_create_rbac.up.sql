-- 026_create_rbac.up.sql
--
-- RBAC storage in Postgres (ADR-028, docs/keeper/rbac.md → § Storage).
-- Three tables materialize the trio "operators (Archons) ↔ roles ↔ permissions":
--   - rbac_roles            - role catalog;
--   - rbac_role_permissions - role permissions (as a RAW string, parsed by ParsePermission in Go);
--   - rbac_role_operators   - "role ↔ operator" membership (the layer whose
--                             absence was the cause of BUG-1: membership had nowhere
--                             to live persistently in a way visible to both keeper init
--                             and the enforcer on every node in the cluster).
--
-- Seeding the cluster-admin role (E1) - separate migration 027 (idempotent INSERT).

CREATE TABLE rbac_roles (
    name           TEXT        PRIMARY KEY,
    description    TEXT        NOT NULL DEFAULT '',
    builtin        BOOLEAN     NOT NULL DEFAULT false,                -- builtin=true forbids role.delete / role.update (Phase 2)
    created_at     TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    created_by_aid TEXT,                                             -- FK to operators(aid); NULL for seed roles with no initiating Archon

    CONSTRAINT rbac_roles_name_format CHECK (name ~ '^[a-z][a-z0-9-]*$'),
    CONSTRAINT rbac_roles_created_by_aid_fk
        FOREIGN KEY (created_by_aid) REFERENCES operators (aid)
);

COMMENT ON TABLE rbac_roles IS
    'RBAC role catalog - ADR-028. PK = name (kebab-case). builtin=true protects against role.delete/update.';

CREATE TABLE rbac_role_permissions (
    role_name  TEXT NOT NULL,
    permission TEXT NOT NULL,                                        -- stored as a RAW string; matching is done by ParsePermission in Go

    PRIMARY KEY (role_name, permission),
    CONSTRAINT rbac_role_permissions_role_fk
        FOREIGN KEY (role_name) REFERENCES rbac_roles (name) ON DELETE CASCADE
);

COMMENT ON TABLE rbac_role_permissions IS
    'Role permissions (as a RAW string) - ADR-028. ON DELETE CASCADE with rbac_roles.';

CREATE TABLE rbac_role_operators (
    role_name      TEXT        NOT NULL,
    aid            TEXT        NOT NULL,
    granted_at     TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    granted_by_aid TEXT,                                             -- FK to operators(aid); NULL for seed/bootstrap membership

    PRIMARY KEY (role_name, aid),
    CONSTRAINT rbac_role_operators_role_fk
        FOREIGN KEY (role_name) REFERENCES rbac_roles (name) ON DELETE CASCADE,
    CONSTRAINT rbac_role_operators_aid_fk
        FOREIGN KEY (aid) REFERENCES operators (aid),
    CONSTRAINT rbac_role_operators_granted_by_aid_fk
        FOREIGN KEY (granted_by_aid) REFERENCES operators (aid)
);

COMMENT ON TABLE rbac_role_operators IS
    'Membership "role ↔ operator" - ADR-028. The absence of this layer was the cause of BUG-1.';

-- The "AID -> roles" index for building the enforcer snapshot with three SELECTs:
-- the main membership query goes by aid, not by PK order (role_name, aid).
CREATE INDEX rbac_role_operators_aid_idx
    ON rbac_role_operators (aid);
