-- 015_add_souls_soulprint.up.sql
--
-- Storage of typed soulprint in the `souls` registry for M2.4 (event handler
-- `SoulprintReport`). Corresponds to ADR-018 (`SoulprintFacts` proto) +
-- ADR-012 (Soul -> Keeper EventStream, `SoulprintReport` payload).
--
--   * `soulprint_facts` - JSON serialization of the proto `SoulprintFacts`
--     (corresponding to `typed_facts` field). Sparse proto fields
--     are serialized jsonb-omit-empty style. Column nullable: until the first
--     `SoulprintReport` from a freshly connected Soul, `typed_facts` is empty.
--   * `soulprint_collected_at` - the moment facts were collected by the Soul agent
--     (proto `collected_at`). Stored separately from received_at - a discrepancy
--     received_at - collected_at > 10 min is logged as a warn per ADR-018.
--   * `soulprint_received_at` - the moment the `SoulprintReport` arrived at the
--     Keeper. Narrows the Reaper's "stale soulprint" detector (a separate
--     slice, post-MVP).
--
-- Schema version from proto (`SoulprintFacts` has no explicit version field in
-- ADR-018) is not materialized here - it will get its own column via a
-- forward-compat migration if the ADR settles on an explicit `schema_version`.

ALTER TABLE souls
    ADD COLUMN soulprint_facts        JSONB,
    ADD COLUMN soulprint_collected_at TIMESTAMPTZ,
    ADD COLUMN soulprint_received_at  TIMESTAMPTZ;

COMMENT ON COLUMN souls.soulprint_facts IS
    'Typed Soulprint (ADR-018, SoulprintFacts proto) - JSONB serialization of the last SoulprintReport.';
COMMENT ON COLUMN souls.soulprint_collected_at IS
    'Soul-side timestamp of the last SoulprintReport (proto collected_at, ADR-018).';
COMMENT ON COLUMN souls.soulprint_received_at IS
    'Keeper-side timestamp of receiving the last SoulprintReport (ADR-018).';
