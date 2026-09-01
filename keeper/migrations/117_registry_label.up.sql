-- 117_registry_label.up.sql
--
-- [ADR-0085] / NIM-728: every registry entity gains a second field beside its
-- identifier -- `label`, a free-text display caption.
--
-- Ten tables, the entities of the eight registries that share the
-- `ValidName` / `NamePattern` pair: incarnation, service_registry, providers,
-- profiles, push_providers, omens, heralds, tidings, vigils, decrees. The two
-- remaining `name TEXT PRIMARY KEY` tables (`rbac_roles`, `synods`) are
-- NIM-732, and `operators.display_name` -> `label` is NIM-733.
--
-- Why the column carries no constraint
-- ------------------------------------
-- The identifier column keeps every constraint it has: PRIMARY KEY, and a
-- CHECK on its kebab form. `label` deliberately takes none of them.
--
--   * NULLABLE -- the field is optional. A row written before this migration,
--     or by a caller that does not send one, reads NULL, and the consumer
--     falls back to showing the identifier. An empty-string default would make
--     "absent" and "deliberately blank" the same value.
--
--   * NOT UNIQUE -- two incarnations may both be captioned `Redis (prod)`. A
--     caption is not an address, and uniqueness is the property of the thing
--     that IS one.
--
--   * NO CHECK on form -- capitals, spaces and punctuation are the whole point
--     ([ADR-0085] "the capital letter lives in `label` and never in `id`"). The
--     narrow grammar belongs to the identifier, which becomes a Vault path
--     segment, a URL segment and a CEL selector.
--
-- THE INVARIANT, restated where the column is created
-- ---------------------------------------------------
-- `label` participates in NOTHING derived. Not a Vault path, not an RBAC
-- scope, not a snapshot directory, not `incarnation.<...>` in CEL, not a
-- selector, not a resolver, not an FK, not a URL. That is the entire reason
-- the field exists: only under it is "I changed the label and nothing moved" a
-- guarantee rather than a hope. A derived address is built from the identifier
-- alone. The guards live next to each derivation and are listed in the package
-- doc of `keeper/internal/registrylabel`.
--
-- Consequently NO INDEX is created on `label`. An index would be evidence that
-- something looks a row up by it, and nothing may.
--
-- Idempotent: IF NOT EXISTS on every column, so a re-run matches nothing.

ALTER TABLE incarnation      ADD COLUMN IF NOT EXISTS label TEXT;
ALTER TABLE service_registry ADD COLUMN IF NOT EXISTS label TEXT;
ALTER TABLE providers        ADD COLUMN IF NOT EXISTS label TEXT;
ALTER TABLE profiles         ADD COLUMN IF NOT EXISTS label TEXT;
ALTER TABLE push_providers   ADD COLUMN IF NOT EXISTS label TEXT;
ALTER TABLE omens            ADD COLUMN IF NOT EXISTS label TEXT;
ALTER TABLE heralds          ADD COLUMN IF NOT EXISTS label TEXT;
ALTER TABLE tidings          ADD COLUMN IF NOT EXISTS label TEXT;
ALTER TABLE vigils           ADD COLUMN IF NOT EXISTS label TEXT;
ALTER TABLE decrees          ADD COLUMN IF NOT EXISTS label TEXT;

COMMENT ON COLUMN incarnation.label IS
    'Display caption of the incarnation (ADR-0085): free text, mutable, not unique, optional. NULL means the consumer shows `name`. Participates in nothing derived - no Vault path, no RBAC scope, no snapshot directory, no CEL root.';
COMMENT ON COLUMN service_registry.label IS
    'Display caption of the service (ADR-0085): free text, mutable, not unique, optional. NULL means the consumer shows `name`. Participates in nothing derived - notably not segment 2 of a derived secret path.';
COMMENT ON COLUMN providers.label IS
    'Display caption of the Cloud-Provider (ADR-0085): free text, mutable, not unique, optional. NULL means the consumer shows `name`. Participates in nothing derived - notably not the `<entity>` segment of secret/<domain>/<entity>/<field>.';
COMMENT ON COLUMN profiles.label IS
    'Display caption of the Cloud-Profile (ADR-0085): free text, mutable, not unique, optional. NULL means the consumer shows `name`. Participates in nothing derived.';
COMMENT ON COLUMN push_providers.label IS
    'Display caption of the Push-Provider (ADR-0085): free text, mutable, not unique, optional. NULL means the consumer shows `name`. Participates in nothing derived - notably not the SOUL_SSH_<UPPER_SNAKE(name)>_PARAMS env-var name.';
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
