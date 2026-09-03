-- 118_registry_id.down.sql
--
-- Reverse of 118: the identifier goes back to being spelled `name`.
--
-- Nothing is lost. The column keeps every value and every constraint it held
-- -- a rename moves no data, and the CHECK follows the column back under its
-- old name with the same predicate. What a rolled-back keeper reads is exactly
-- what it wrote: the identifier is the same string, so every derived secret
-- path, every RBAC scope and every artifact cache directory resolves as before.
--
-- Column comments are restored to their pre-118 text, which is what makes this
-- a true reverse rather than a rename with a stale annotation left behind.

ALTER TABLE service_registry RENAME CONSTRAINT service_registry_id_format TO service_registry_name_format;
ALTER TABLE service_registry RENAME COLUMN id TO name;

ALTER TABLE providers RENAME CONSTRAINT providers_id_format TO providers_name_format;
ALTER TABLE providers RENAME COLUMN id TO name;

ALTER TABLE profiles RENAME CONSTRAINT profiles_id_format TO profiles_name_format;
ALTER TABLE profiles RENAME COLUMN id TO name;

ALTER TABLE push_providers RENAME CONSTRAINT push_providers_id_format TO push_providers_name_format;
ALTER TABLE push_providers RENAME COLUMN id TO name;

ALTER TABLE omens RENAME CONSTRAINT omens_id_format TO omens_name_format;
ALTER TABLE omens RENAME COLUMN id TO name;

ALTER TABLE heralds RENAME CONSTRAINT heralds_id_format TO heralds_name_format;
ALTER TABLE heralds RENAME COLUMN id TO name;

ALTER TABLE tidings RENAME CONSTRAINT tidings_id_format TO tidings_name_format;
ALTER TABLE tidings RENAME COLUMN id TO name;

ALTER TABLE vigils RENAME CONSTRAINT vigils_id_format TO vigils_name_format;
ALTER TABLE vigils RENAME COLUMN id TO name;

ALTER TABLE decrees RENAME CONSTRAINT decrees_id_format TO decrees_name_format;
ALTER TABLE decrees RENAME COLUMN id TO name;

ALTER TABLE incarnation RENAME CONSTRAINT incarnation_id_format TO incarnation_name_format;
ALTER TABLE incarnation RENAME COLUMN id TO name;

COMMENT ON TABLE service_registry IS
    'Managed-via-API registry of Services (moved from services[] in keeper.yml). PK = name (kebab-case), ref = git ref per ADR-007.';
COMMENT ON COLUMN service_registry.name IS NULL;
COMMENT ON COLUMN service_registry.label IS
    'Display caption of the service (ADR-0085): free text, mutable, not unique, optional. NULL means the consumer shows `name`. Participates in nothing derived - notably not segment 2 of a derived secret path.';

COMMENT ON COLUMN providers.name IS NULL;
COMMENT ON COLUMN providers.label IS
    'Display caption of the Cloud-Provider (ADR-0085): free text, mutable, not unique, optional. NULL means the consumer shows `name`. Participates in nothing derived - notably not the `<entity>` segment of secret/<domain>/<entity>/<field>.';
COMMENT ON COLUMN profiles.name IS NULL;
COMMENT ON COLUMN profiles.label IS
    'Display caption of the Cloud-Profile (ADR-0085): free text, mutable, not unique, optional. NULL means the consumer shows `name`. Participates in nothing derived.';
COMMENT ON COLUMN push_providers.name IS NULL;
COMMENT ON COLUMN push_providers.label IS
    'Display caption of the Push-Provider (ADR-0085): free text, mutable, not unique, optional. NULL means the consumer shows `name`. Participates in nothing derived - notably not the SOUL_SSH_<UPPER_SNAKE(name)>_PARAMS env-var name.';

COMMENT ON COLUMN omens.name IS NULL;
COMMENT ON COLUMN heralds.name IS NULL;
COMMENT ON COLUMN tidings.name IS NULL;
COMMENT ON COLUMN vigils.name IS NULL;
COMMENT ON COLUMN decrees.name IS NULL;

COMMENT ON COLUMN incarnation.name IS NULL;

-- Restored VERBATIM from 117 (label columns) and 005 (the table comment):
-- the up refreshed these to say `id`, and a reverse that paraphrases is not one.
COMMENT ON TABLE incarnation IS
    'Registry of runtime instances of Service (ADR-009). PK = name (Coven label).';
COMMENT ON COLUMN incarnation.label IS
    'Display caption of the incarnation (ADR-0085): free text, mutable, not unique, optional. NULL means the consumer shows `name`. Participates in nothing derived - no Vault path, no RBAC scope, no snapshot directory, no CEL root.';
COMMENT ON COLUMN omens.label IS
    'Display caption of the Omen (ADR-0085): free text, mutable, not unique, optional. NULL means the consumer shows `name`. Participates in nothing derived.';
COMMENT ON COLUMN heralds.label IS
    'Display caption of the Herald (ADR-0085): free text, mutable, not unique, optional. NULL means the consumer shows `name`. Participates in nothing derived - notably not the `<entity>` segment of secret/<domain>/<entity>/<field>.';
COMMENT ON COLUMN tidings.label IS
    'Display caption of the Tiding (ADR-0085): free text, mutable, not unique, optional. NULL means the consumer shows `name`. Participates in nothing derived.';
COMMENT ON COLUMN vigils.label IS
    'Display caption of the Vigil (ADR-0085): free text, mutable, not unique, optional. NULL means the consumer shows `name`. Participates in nothing derived.';
COMMENT ON COLUMN decrees.label IS
    'Display caption of the Decree (ADR-0085): free text, mutable, not unique, optional. NULL means the consumer shows `name`. Participates in nothing derived.';
