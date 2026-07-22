-- 046_add_incarnation_covens.up.sql
--
-- Declared environment tags for incarnation, for per-Coven RBAC scope
-- (ADR-008 amendment a). Before this migration the `coven=` RBAC selector on
-- incarnation endpoints never matched: the extractor put only
-- `{incarnation: name}` into the context, without coven/service (docs<->code drift,
-- rbac.md declared the source, the code didn't land it). The column carries stable
-- env labels (prod/dev/dc1/...), set by the operator at create time; the RBAC context
-- of incarnation routes = `covens ∪ {name}` (the name is the root Coven label per ADR-008).
--
-- The format of each label is CovenPattern (`^[a-z][a-z0-9]*(-[a-z0-9]+)*$`),
-- symmetric with souls.coven[]. Format checking lives in the API layer (ValidCoven),
-- not a CHECK constraint: grammar-checking TEXT[] elements in a CHECK is more expensive
-- and duplicates the API validation (the same souls.coven pattern also has no CHECK).
--
-- DEFAULT '{}' - an incarnation with no declared env tags: coven-scoped roles by
-- env won't match, but `coven=<name>` (name-as-coven) and `service=` still work.

ALTER TABLE incarnation
    ADD COLUMN covens TEXT[] NOT NULL DEFAULT '{}';

COMMENT ON COLUMN incarnation.covens IS
    'Declared environment tags for incarnation (ADR-008). RBAC scope coven= for incarnation operations = covens ∪ {name}.';
