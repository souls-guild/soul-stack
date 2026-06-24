-- 083_operators_auth_method_ldap_oidc.up.sql
--
-- ADR-058 (LDAP-часть принята): расширение enum `auth_method` реестра
-- operators значениями федеративной аутентификации `ldap` и `oidc`.
-- Only-add — прежний набор (`jwt`/`mtls`/`combined`) остаётся валидным,
-- существующие строки не затрагиваются.
--
-- CHECK `auth_method_valid` создан миграцией 003 (forward-only, не правится):
-- DROP + ADD с расширенным набором. `oidc` заводится сразу (стадия 2 ADR-058),
-- чтобы дальнейшая имплементация OIDC не требовала повторной миграции CHECK.

ALTER TABLE operators DROP CONSTRAINT auth_method_valid;
ALTER TABLE operators ADD CONSTRAINT auth_method_valid
    CHECK (auth_method IN ('jwt', 'mtls', 'combined', 'ldap', 'oidc'));
