-- 084_operators_created_via.up.sql
--
-- ADR-058(d): the `created_via` field of the operators registry - "where the
-- operator was created from" (bootstrap / user / ldap / oidc / system). Differs from
-- `auth_method` ("what the operator logs in with"): the bootstrap Archon is created via
-- `keeper init`, but logs in via jwt; a federated operator is created by auto-provisioning
-- (ldap/oidc), the system row (`archon-system`) is seeded for FK attribution
-- of system-initiated inserts (auto-import push, federated-provision).
--
-- This field legalizes `created_by_aid IS NULL` on NON-bootstrap rows
-- (`archon-system` + federated operators): the bootstrap invariant (exactly one
-- first Archon) moves from `created_by_aid IS NULL` to
-- `created_via = 'bootstrap'` by migration 085.
--
-- DEFAULT 'user' - a safe fallback for existing rows created
-- via the Operator API (POST /v1/operators).

ALTER TABLE operators ADD COLUMN created_via TEXT NOT NULL DEFAULT 'user';
ALTER TABLE operators ADD CONSTRAINT created_via_valid
    CHECK (created_via IN ('bootstrap','user','ldap','oidc','system'));

-- reconcile existing rows: the first bootstrap Archon had created_by_aid IS NULL.
UPDATE operators SET created_via = 'bootstrap' WHERE created_by_aid IS NULL;

-- archon-system (if already seeded via the old path) - system.
UPDATE operators SET created_via = 'system' WHERE aid = 'archon-system';

COMMENT ON COLUMN operators.created_via IS
    'Source of operator creation (ADR-058): bootstrap|user|ldap|oidc|system. Differs from auth_method (login method).';
