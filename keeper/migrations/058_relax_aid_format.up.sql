-- 058_relax_aid_format.up.sql
--
-- ADR-014 amendment (2026-05-29): relaxes the AID format. The mandatory
-- `archon-` prefix is dropped; the charset is widened to `[a-z0-9._@-]` for
-- email-like external names (LDAP/Keycloak auto-provision). The first
-- character must be a lowercase ASCII letter or digit, total length 2..128.
--
-- The charset is deliberately narrow and safe: no `/`/`\` (path traversal),
-- ASCII-lowercase only (no unicode look-alikes or case folding), no
-- control characters/quotes (no injection).
--
-- Forward-only: migration 003 (CONSTRAINT aid_format `^archon-[a-z0-9-]{1,62}$`)
-- is already applied in production and is not being edited. Old AIDs of the form
-- `archon-<...>` remain valid under the new pattern (they start with a letter, and the
-- charset is a superset of the previous one).
--
-- PG `~` is POSIX ERE. In the class `[a-z0-9._@-]` the trailing dash is literal,
-- and the dot inside the class is literal - no escaping is required.

ALTER TABLE operators DROP CONSTRAINT aid_format;
ALTER TABLE operators ADD CONSTRAINT aid_format
    CHECK (aid ~ '^[a-z0-9][a-z0-9._@-]{1,127}$');
