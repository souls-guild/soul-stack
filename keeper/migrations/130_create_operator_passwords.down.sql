-- 130_create_operator_passwords.down.sql
--
-- Drops local passwords. The CHECK goes back to the set of migration 083, so
-- the rollback refuses while any row of `operators` has auth_method = 'local',
-- revoked ones included — revoking only sets revoked_at, and no API deletes an
-- operator. A cluster that ever created a local operator keeps this migration
-- unless those rows are removed by hand. Roll the binary back with it — a
-- keeper that knows `local` would try to read a table that is gone.

DROP TABLE IF EXISTS operator_passwords;
ALTER TABLE operators DROP CONSTRAINT IF EXISTS operators_aid_auth_method_key;
ALTER TABLE operators DROP CONSTRAINT auth_method_valid;
ALTER TABLE operators ADD CONSTRAINT auth_method_valid
    CHECK (auth_method IN ('jwt', 'mtls', 'combined', 'ldap', 'oidc'));
