-- 083_operators_auth_method_ldap_oidc.up.sql
--
-- ADR-058 (LDAP part accepted): extends the `auth_method` enum of the
-- operators registry with the federated authentication values `ldap` and
-- `oidc`. Only-add - the previous set (`jwt`/`mtls`/`combined`) remains
-- valid, existing rows are not affected.
--
-- The CHECK `auth_method_valid` was created by migration 003 (forward-only,
-- not edited in place): DROP + ADD with the extended set. `oidc` is added
-- now (ADR-058 stage 2) so that the later OIDC implementation does not
-- require another CHECK migration.

ALTER TABLE operators DROP CONSTRAINT auth_method_valid;
ALTER TABLE operators ADD CONSTRAINT auth_method_valid
    CHECK (auth_method IN ('jwt', 'mtls', 'combined', 'ldap', 'oidc'));
