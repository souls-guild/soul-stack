-- 086_seed_archon_system.up.sql
--
-- ADR-058(d): seeds the system operator `archon-system` (created_via='system',
-- created_by_aid=NULL). Now legal - the bootstrap index (migration 085) enforces
-- uniqueness only for `created_via='bootstrap'`; a NULL created_by_aid outside
-- bootstrap does not violate the invariant.
--
-- `archon-system` is the FK anchor for system-initiated inserts:
--   - push auto-import writes push_providers.created_by_aid = 'archon-system';
--   - before ADR-058(d), federated provisioning wrote
--     operators.created_by_aid = 'archon-system' (after ADR-058(d), federated
--     writes NULL + created_via='ldap', but the system row remains the anchor
--     for auto-import).
--
-- AID `archon-system` matches the CHECK aid_format (migration 058:
-- `^[a-z0-9][a-z0-9._@-]{1,127}$`) and the earlier pattern from migration 003
-- (`^archon-[a-z0-9-]{1,62}$`).
--
-- ON CONFLICT DO NOTHING - idempotency: the row could have been seeded by the
-- earlier path (a manual operator in pilot S7-4).

INSERT INTO operators (aid, display_name, auth_method, created_by_aid, created_via, metadata)
VALUES ('archon-system', 'System (Soul Stack)', 'jwt', NULL, 'system', '{}'::jsonb)
ON CONFLICT (aid) DO NOTHING;
