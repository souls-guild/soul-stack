-- 083_operators_auth_method_ldap_oidc.down.sql
--
-- Reverts to the `auth_method` set from migration 003 (`jwt`/`mtls`/`combined`).
-- WARNING: the rollback will reject any rows with `auth_method` = `ldap`/`oidc`
-- (federated operators). The down path is only applicable when there are none in the registry.

ALTER TABLE operators DROP CONSTRAINT auth_method_valid;
ALTER TABLE operators ADD CONSTRAINT auth_method_valid
    CHECK (auth_method IN ('jwt', 'mtls', 'combined'));
