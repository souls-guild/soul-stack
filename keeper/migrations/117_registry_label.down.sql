-- 117_registry_label.down.sql
--
-- Reverse of 117: the display caption is dropped from all ten registries.
--
-- The captions themselves are NOT recoverable after this runs, and nothing
-- else in the tree holds a copy -- `label` is written only here, precisely
-- because it participates in nothing derived ([ADR-0085]). That loss is
-- cosmetic by construction: no address, no scope and no path was ever built
-- from the column, so a rolled-back keeper resolves every secret, every RBAC
-- decision and every CEL expression exactly as it did before. What an operator
-- loses is the caption on a screen, and the fallback -- show the identifier --
-- is the same behaviour a NULL label already produced.
--
-- Idempotent: IF EXISTS on every column.

ALTER TABLE incarnation      DROP COLUMN IF EXISTS label;
ALTER TABLE service_registry DROP COLUMN IF EXISTS label;
ALTER TABLE providers        DROP COLUMN IF EXISTS label;
ALTER TABLE profiles         DROP COLUMN IF EXISTS label;
ALTER TABLE push_providers   DROP COLUMN IF EXISTS label;
ALTER TABLE omens            DROP COLUMN IF EXISTS label;
ALTER TABLE heralds          DROP COLUMN IF EXISTS label;
ALTER TABLE tidings          DROP COLUMN IF EXISTS label;
ALTER TABLE vigils           DROP COLUMN IF EXISTS label;
ALTER TABLE decrees          DROP COLUMN IF EXISTS label;
