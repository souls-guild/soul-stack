-- 034_create_service_registry.up.sql
--
-- Registry of Services in Postgres (ADR-028-style managed-via-API registry,
-- symmetric with RBAC). Moves the `services[]` catalog out of the static keeper.yml
-- into a managed-via-OpenAPI/MCP table: a Service record appears/changes
-- only through an explicit Archon operation, is visible to every cluster node, and
-- survives a restart without editing the config.
--
-- Columns map 1:1 to the former config.ServiceRegistryEntry:
--   - name    - PK (kebab-case), unique Service name within the cluster;
--   - git     - git source of the Service repo (non-empty);
--   - ref     - git ref (tag/branch) per ADR-007 (non-empty; no semver ranges);
--   - refresh - auto-refresh duration string ("5m"); NULL = no auto-refresh.
--               The format is NOT caught by a CHECK - the service layer does it via
--               config.ParseDuration (same as augur token_ttl).
--
-- FK to operators(aid) for created_by_aid / updated_by_aid - ON DELETE SET NULL:
-- a Service record survives the authoring operator's offboarding, the audit field
-- is nulled out (symmetric with omens/providers/incarnation). RESTRICT is kept
-- only for the security-critical rbac_roles.

CREATE TABLE service_registry (
    name           TEXT        PRIMARY KEY,
    git            TEXT        NOT NULL,
    ref            TEXT        NOT NULL,
    refresh        TEXT,                                            -- auto-refresh duration string ("5m"); NULL = no auto-refresh
    created_at     TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at     TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    created_by_aid TEXT,                                            -- FK to operators(aid); NULL for seed/no Archon initiator
    updated_by_aid TEXT,                                            -- FK to operators(aid); NULL until the first update

    CONSTRAINT service_registry_name_format
        CHECK (name ~ '^[a-z][a-z0-9-]*$'),
    CONSTRAINT service_registry_git_nonempty
        CHECK (git <> ''),
    CONSTRAINT service_registry_ref_nonempty
        CHECK (ref <> ''),
    CONSTRAINT service_registry_created_by_fk
        FOREIGN KEY (created_by_aid) REFERENCES operators (aid) ON DELETE SET NULL,
    CONSTRAINT service_registry_updated_by_fk
        FOREIGN KEY (updated_by_aid) REFERENCES operators (aid) ON DELETE SET NULL
);

COMMENT ON TABLE service_registry IS
    'Managed-via-API registry of Services (moved from services[] in keeper.yml). PK = name (kebab-case), ref = git ref per ADR-007.';
