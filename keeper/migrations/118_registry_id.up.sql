-- 118_registry_id.up.sql
--
-- [ADR-0085] / NIM-729: the identifier of a registry entity is spelled `id`,
-- not `name`. The companion of migration 117, which added the mutable `label`
-- beside it -- together the two are the split the ADR is about: one field that
-- never moves and one that is free to.
--
-- NIM-729 lands registry by registry and appends to this one file, mirroring
-- migration 117 which added `label` to the same ten entities. READ THE
-- STATEMENTS BELOW rather than this paragraph to know what has converted --
-- they are the fact, and `TestIDGrammar_EveryRenamedTableIsGuarded` reads them
-- too, so a table renamed here without a guard row is a failing test.
--
-- The two remaining `name TEXT PRIMARY KEY` tables (`rbac_roles`, `synods`) are
-- NIM-732, and `operators.display_name` -> `label` is NIM-733. Neither is in
-- this file at all.
--
-- The grammar does not move, only the spelling
-- --------------------------------------------
-- Each table keeps the CHECK it already had, character for character, and the
-- pair of statements below is what makes that true rather than merely intended.
-- `RENAME COLUMN` is the one that carries the predicate: the CHECK expression
-- is stored as a parse tree over column attnums, not as text, so renaming the
-- column re-renders the predicate against the new name with nothing retyped.
-- `RENAME CONSTRAINT` then only changes `pg_constraint.conname`, so the
-- constraint's NAME stops advertising a column that no longer exists.
--
-- Neither is a DROP + ADD, which is the point: a DROP + ADD would restate the
-- predicate by hand (a place to mistype it), leave the table momentarily
-- unconstrained, and run a validation scan over existing rows that a row legal
-- today could fail.
--
-- [ADR-0085] does decide ONE grammar for every registry
-- (`^[a-z0-9][a-z0-9-]{0,62}$`). Adopting it is deliberately NOT part of this
-- migration: on six of these registries it is a NARROWING, which is a
-- different kind of change -- it can reject rows that exist -- and mixing it
-- into a rename would make the diff unable to prove itself. It is scheduled
-- separately.
--
-- What a REFERENCED table needs, and what `service_registry` does not show
-- ------------------------------------------------------------------------
-- `service_registry` is the one converted table with no inbound foreign key, so
-- the two statements below are the whole of its conversion. Six of the nine
-- still to come are referenced, and they need a third thing:
--
--   * The FK itself needs nothing. A foreign key is stored against column
--     identity, so `REFERENCES incarnation (name)` follows a `RENAME COLUMN` on
--     its own and re-renders as `REFERENCES incarnation (id)`.
--   * The REFERENCING COLUMN keeps its spelling HERE, and that is a decision.
--     `state_history.incarnation_name`, `apply_runs.incarnation_name`,
--     `incarnation_choirs.incarnation_name`,
--     `incarnation_membership.incarnation_name` and `decrees.incarnation_name`
--     stay `*_name` in NIM-729 and convert in **NIM-732**, alongside
--     `rbac_roles` and `synods` -- which is the paragraph of [ADR-0085] Scope
--     they are listed in, and it ends "the tail beyond the eight is ticketed
--     NIM-732 so that it is scheduled rather than assumed".
--
--     Nothing is left broken by that split: a foreign key is stored against
--     column identity, so `REFERENCES incarnation (name)` re-renders as
--     `REFERENCES incarnation (id)` on its own and every constraint keeps
--     working. What is left is a column NAMED after a concept the row no longer
--     uses, which is a readability debt with a ticket against it rather than a
--     schema that disagrees with itself.
--
--     The size is why the split is worth stating: the FK tail is ~898
--     occurrences of `incarnation_name` / `IncarnationName` and breaks nine
--     `json:"incarnation_name"` wire fields including the mandatory
--     `Decree.incarnation_name` -- on its own the larger half of this ticket.
--
-- NOT idempotent, unlike 117, and deliberately
-- -------------------------------------------
-- `ALTER TABLE ... RENAME COLUMN` has no `IF EXISTS` form. The runner is
-- golang-migrate, which records the applied version and runs each file exactly
-- once, so the guard 117 could afford to add has nothing to protect here.

ALTER TABLE service_registry RENAME COLUMN name TO id;
ALTER TABLE service_registry RENAME CONSTRAINT service_registry_name_format TO service_registry_id_format;

-- `providers.type` keeps `providers_type_format`: it shares this grammar without
-- sharing the role, and a driver-plugin name is not an entity identifier.
ALTER TABLE providers RENAME COLUMN name TO id;
ALTER TABLE providers RENAME CONSTRAINT providers_name_format TO providers_id_format;

-- `profiles.provider` is the FK VALUE, not a `*_name` column, so it needs no
-- statement; the constraint `profiles_provider_fk` follows the renamed column on
-- the referenced side by itself.
ALTER TABLE profiles RENAME COLUMN name TO id;
ALTER TABLE profiles RENAME CONSTRAINT profiles_name_format TO profiles_id_format;

ALTER TABLE push_providers RENAME COLUMN name TO id;
ALTER TABLE push_providers RENAME CONSTRAINT push_providers_name_format TO push_providers_id_format;

-- `rites.omen` is the FK VALUE, not a `*_name` column: no statement needed, and
-- `rites_omen_fk` follows the renamed column on the referenced side by itself.
ALTER TABLE omens RENAME COLUMN name TO id;
ALTER TABLE omens RENAME CONSTRAINT omens_name_format TO omens_id_format;

-- `tidings.herald` is likewise the FK VALUE and stays put.
ALTER TABLE heralds RENAME COLUMN name TO id;
ALTER TABLE heralds RENAME CONSTRAINT heralds_name_format TO heralds_id_format;

ALTER TABLE tidings RENAME COLUMN name TO id;
ALTER TABLE tidings RENAME CONSTRAINT tidings_name_format TO tidings_id_format;

-- `decrees.on_beacon` names a Vigil and is a value, not a column to rename.
-- `decrees.incarnation_name` is a `*_name` FK column and does NOT convert here
-- either: the whole `*_name` FK tail is NIM-732 (see the header).
ALTER TABLE vigils RENAME COLUMN name TO id;
ALTER TABLE vigils RENAME CONSTRAINT vigils_name_format TO vigils_id_format;

ALTER TABLE decrees RENAME COLUMN name TO id;
ALTER TABLE decrees RENAME CONSTRAINT decrees_name_format TO decrees_id_format;

-- The tenth and last. Five tables carry an `incarnation_name` FK column and NONE
-- of them is touched: the FK follows this rename by itself, and the columns'
-- own spelling is NIM-732 (see the header).
ALTER TABLE incarnation RENAME COLUMN name TO id;
ALTER TABLE incarnation RENAME CONSTRAINT incarnation_name_format TO incarnation_id_format;

COMMENT ON TABLE service_registry IS
    'Managed-via-API registry of Services (moved from services[] in keeper.yml). PK = id (kebab-case, immutable - ADR-0085), ref = git ref per ADR-007.';
COMMENT ON COLUMN service_registry.id IS
    'Immutable identifier of the service (ADR-0085): kebab-case, set once at registration, no rename operation. Segment 2 of every derived secret path <mount>/<service>/<incarnation>/<state-field>, and the artifact cache directory - which is why it never moves.';
COMMENT ON COLUMN service_registry.label IS
    'Display caption of the service (ADR-0085): free text, mutable, not unique, optional. NULL means the consumer shows `id`. Participates in nothing derived - notably not segment 2 of a derived secret path.';

COMMENT ON COLUMN providers.id IS
    'Immutable identifier of the Cloud-Provider (ADR-0085): kebab-case, set once at creation, no rename operation. The `<entity>` segment of secret/provider/<entity>/credentials - one hop from this row, with nothing in between to notice a change.';
COMMENT ON COLUMN providers.label IS
    'Display caption of the Cloud-Provider (ADR-0085): free text, mutable, not unique, optional. NULL means the consumer shows `id`. Participates in nothing derived - notably not the `<entity>` segment of secret/<domain>/<entity>/<field>.';

COMMENT ON COLUMN profiles.id IS
    'Immutable identifier of the Cloud-Profile (ADR-0085): kebab-case, set once at creation, no rename operation.';
COMMENT ON COLUMN profiles.label IS
    'Display caption of the Cloud-Profile (ADR-0085): free text, mutable, not unique, optional. NULL means the consumer shows `id`. Participates in nothing derived.';

COMMENT ON COLUMN push_providers.id IS
    'Immutable identifier of the Push-Provider (ADR-0085): kebab-case, letter-first, set once at creation, no rename operation. The source of the env-var name SOUL_SSH_<UPPER_SNAKE(id)>_PARAMS, which is why the letter-first rule is on THIS column and not on the caption.';
COMMENT ON COLUMN push_providers.label IS
    'Display caption of the Push-Provider (ADR-0085): free text, mutable, not unique, optional. NULL means the consumer shows `id`. Participates in nothing derived - notably not the SOUL_SSH_<UPPER_SNAKE(id)>_PARAMS env-var name.';

COMMENT ON COLUMN omens.id IS
    'Immutable identifier of the Omen (ADR-0085): kebab code word, set once at creation, no rename operation. The target of the rites.omen FK. NOT to be confused with rites.id, the int64 surrogate - an entity id is a code word, a surrogate is neither.';
COMMENT ON COLUMN heralds.id IS
    'Immutable identifier of the Herald channel (ADR-0085): kebab code word, set once at creation, no rename operation. The `<entity>` segment of secret/herald/<entity>/<field> - one hop from this row with nothing in between to notice - and the target of the tidings.herald FK.';
COMMENT ON COLUMN tidings.id IS
    'Immutable identifier of the Tiding rule (ADR-0085): kebab code word, set once at creation, no rename operation.';
COMMENT ON COLUMN vigils.id IS
    'Immutable identifier of the Vigil (ADR-0085): kebab code word, set once at creation, no rename operation. What a Decree names in on_beacon.';
COMMENT ON COLUMN decrees.id IS
    'Immutable identifier of the Decree (ADR-0085): kebab code word, set once at creation, no rename operation. The key oracle_fires and oracle_circuit hang cooldown state and breaker state off.';

COMMENT ON COLUMN incarnation.id IS
    'Immutable identifier of the incarnation (ADR-0085): kebab code word, set once at creation, no rename operation. Segment 3 of every derived secret path <mount>/<service>/<incarnation>/<state-field>, the value of the RBAC incarnation= scope dimension, and the CEL root - which is still SPELLED incarnation.name until NIM-730 opens its compatibility window.';

-- The remaining six `label` comments, and the one table comment that named the
-- old column. Migration 117 wrote "NULL means the consumer shows `name`" on all
-- ten; four were refreshed above beside their own `id` comment, and leaving the
-- other six would have them naming a column their table no longer has.
COMMENT ON TABLE incarnation IS
    'Registry of runtime instances of Service (ADR-009). PK = id (immutable - ADR-0085).';
COMMENT ON COLUMN incarnation.label IS
    'Display caption of the incarnation (ADR-0085): free text, mutable, not unique, optional. NULL means the consumer shows `id`. Participates in nothing derived - no Vault path, no RBAC scope, no snapshot directory, no CEL root.';
COMMENT ON COLUMN omens.label IS
    'Display caption of the Omen (ADR-0085): free text, mutable, not unique, optional. NULL means the consumer shows `id`. Participates in nothing derived.';
COMMENT ON COLUMN heralds.label IS
    'Display caption of the Herald (ADR-0085): free text, mutable, not unique, optional. NULL means the consumer shows `id`. Participates in nothing derived - notably not the `<entity>` segment of secret/<domain>/<entity>/<field>.';
COMMENT ON COLUMN tidings.label IS
    'Display caption of the Tiding (ADR-0085): free text, mutable, not unique, optional. NULL means the consumer shows `id`. Participates in nothing derived.';
COMMENT ON COLUMN vigils.label IS
    'Display caption of the Vigil (ADR-0085): free text, mutable, not unique, optional. NULL means the consumer shows `id`. Participates in nothing derived.';
COMMENT ON COLUMN decrees.label IS
    'Display caption of the Decree (ADR-0085): free text, mutable, not unique, optional. NULL means the consumer shows `id`. Participates in nothing derived.';
