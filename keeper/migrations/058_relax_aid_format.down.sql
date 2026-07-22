-- 058_relax_aid_format.down.sql
--
-- Reverts to the strict AID format from migration 003 (`^archon-[a-z0-9-]{1,62}$`).
-- WARNING: the rollback will reject any AID created in the relaxed format
-- (without the `archon-` prefix or with `. _ @` characters). The down path applies
-- only if no such AIDs exist in the registry.

ALTER TABLE operators DROP CONSTRAINT aid_format;
ALTER TABLE operators ADD CONSTRAINT aid_format
    CHECK (aid ~ '^archon-[a-z0-9-]{1,62}$');
