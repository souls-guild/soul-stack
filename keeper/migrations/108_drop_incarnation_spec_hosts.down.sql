-- 108_drop_incarnation_spec_hosts.down.sql
--
-- Schema-neutral by construction: the up-migration changes DATA only (it strips
-- one key out of the freeform `incarnation.spec` jsonb), so there is no structure
-- to undo.
--
-- The declared hosts are deliberately NOT restored. They cannot be: the key held
-- operator intent, not a projection of anything else in the database, so there is
-- no source to recompute it from. Nor is one needed - a keeper rolled back to
-- pre-NIM-330 code resolves every host's role from its Voice first
-- (`incarnation_choir_voices`, ADR-044 amendment 2026-05-29(a)) and only falls
-- back to the spec for hosts that have none. Such a host resolves to an empty
-- role, which is precisely what the pre-rollback keeper was already giving it.

SELECT 1;
