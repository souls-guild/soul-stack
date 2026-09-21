-- 087_add_souls_traits.up.sql
--
-- Trait - operator-set key-value labels on a Soul (ADR-060). A separate
-- axis alongside the flat `souls.coven TEXT[]` (ADR-008): coven remains a
-- set of logical membership/targeting/RBAC labels, traits carry attributes
-- (owner/product/namespace) in the form key -> (scalar | list).
--
--   * `traits` is jsonb because the value is polymorphic (scalar OR list) -
--     `TEXT[]` cannot express that. NOT NULL DEFAULT '{}' - absence of
--     traits means an empty object, not NULL (symmetry with `coven`
--     DEFAULT '{}'::text[]): the read path and the registry projection
--     `soulprint.self.traits` do not distinguish "no column" from "no
--     labels".
--   * Source is the operator (the write path is the next slice, ADR-060
--     item 5); this is a read/target-only pilot.
--
-- `souls.coven TEXT[]` is left UNTOUCHED (Variant B of ADR-060: extending
-- coven to key-value was rejected - it breaks scope-pushdown
-- `$1 = ANY(coven)` and predicates like `'x' in soulprint.self.covens`).

ALTER TABLE souls
    ADD COLUMN traits JSONB NOT NULL DEFAULT '{}'::jsonb;

-- GIN index for targeting by traits: `traits @> '{"namespace":"dba-ns"}'`
-- (containment) - the standard path for jsonb predicates (parallel to
-- `souls_coven_idx` GIN over text[]). Supports the future write-/scope
-- layer, does not block the read/target pilot.
CREATE INDEX souls_traits_idx
    ON souls USING GIN (traits);

COMMENT ON COLUMN souls.traits IS
    'Trait - operator-set key-value labels (ADR-060); value is scalar|list, a separate axis alongside coven.';
