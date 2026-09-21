-- 108_drop_incarnation_spec_hosts.up.sql
--
-- ADR-044 amendment 2026-07-30 (NIM-330): `incarnation.spec.hosts[]` is removed.
-- The declared role is a Voice in a Choir (`incarnation_choir_voices.role`) and
-- nothing else; the roster of a run has been `incarnation_membership` since
-- NIM-124. The field decided nothing on either axis.
--
-- `spec` is a freeform jsonb, so there is no column to drop - only the key. The
-- alternative was to leave the key sitting there, and it was rejected: `spec` is
-- returned verbatim by `GET /v1/incarnations/{name}` and rendered in the UI, so a
-- leftover `hosts[]` would keep advertising a topology no resolver reads. That is
-- the exact confusion NIM-330 exists to remove, and it would age badly - a year
-- from now the key would look like data rather than debris.
--
-- Only the `hosts` key is removed; every other key of `spec` (including `traits`,
-- ADR-060) is preserved by the `-` operator. Rows whose spec has no `hosts` key
-- are not touched at all (the WHERE clause), so the migration does not rewrite
-- the whole table to no effect.
--
-- Idempotent: a re-run matches nothing.
--
-- Not reversible in data (see the .down.sql): the only writer of this key,
-- `PATCH /v1/incarnations/{name}/hosts`, is unmounted in the same change.

UPDATE incarnation
SET spec = spec - 'hosts'
WHERE spec ? 'hosts';
