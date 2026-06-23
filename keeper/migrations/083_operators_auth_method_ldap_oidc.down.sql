-- 083_operators_auth_method_ldap_oidc.down.sql
--
-- Возврат к набору `auth_method` из миграции 003 (`jwt`/`mtls`/`combined`).
-- ВНИМАНИЕ: откат отвергнет любые строки с `auth_method` = `ldap`/`oidc`
-- (federated-операторы). Down-путь применим только при их отсутствии в реестре.

ALTER TABLE operators DROP CONSTRAINT auth_method_valid;
ALTER TABLE operators ADD CONSTRAINT auth_method_valid
    CHECK (auth_method IN ('jwt', 'mtls', 'combined'));
