-- 119_drop_cloud_registries.up.sql
--
-- NIM-761 / ADR-017 amendment: the CloudDriver contract is removed, so the two
-- registries that only ever fed it go with it. A VM is now created by an
-- ordinary `side: keeper` SoulModule plugin, which carries its own credentials
-- and its own parameters in the step -- neither reads `providers` or
-- `profiles`, and nothing else in the schema ever did.
--
-- Order matters: `profiles.provider` is a FOREIGN KEY onto `providers`
-- (migration 020) with ON DELETE RESTRICT, so the child goes first.
--
-- Forward-only, and the data is NOT recoverable from here (ADR-019): a
-- Provider's `credentials_ref` is a Vault path, so dropping the row orphans the
-- secret rather than destroying it -- the Vault entry stays and an operator can
-- still read or delete it by path. That is deliberate: this migration must not
-- reach into Vault, because a rollback of the code without a rollback of the
-- secret store is the recoverable direction and the reverse is not.

DROP TABLE IF EXISTS profiles;
DROP TABLE IF EXISTS providers;
