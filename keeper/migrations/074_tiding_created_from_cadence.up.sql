-- 074_tiding_created_from_cadence.up.sql
--
-- ADR-052 §m (persistent Tiding from the Cadence form) + ADR-046 §9 (cascade
-- teardown of auto-rules when a Cadence is deleted).
--
-- Extends the `tidings` registry with one additive column (no existing field/
-- contract changes semantics):
--   - created_from_cadence_id - an ORIGIN marker: "this rule was created from the
--     notify[] block of THIS schedule's form" (POST /v1/cadences). NULL = the rule
--     was set up some other way (manual Tiding CRUD / ephemeral from a Voyage). A non-empty
--     value -> FK to cadences(id) ON DELETE CASCADE: tearing down the Cadence atomically
--     takes with it the rules it spawned.
--
-- Why a separate marker instead of reusing the `cadence` selector: the
-- `cadence` column is a subscription SELECTOR ("only send about runs of this schedule",
-- which can also be set on a manually created Tiding). Cascade delete must tear down
-- ONLY rules born from the form, and must NOT touch ones manually set up with the same
-- cadence selector. Hence origin is a separate column with FK CASCADE,
-- orthogonal to the `cadence` filter selector (ADR-046 §9, ADR-052 §m).
--
-- Bound by cadences.id (ULID-PK, rename-safe), NOT by the schedule's name: the
-- Cadence name is mutable (PATCH), while the ULID is a stable identifier.
--
-- cadences.id - TEXT PRIMARY KEY (migration 066) -> a valid FK target for a TEXT
-- column. ON DELETE CASCADE (not SET NULL, like voyages.cadence_id): an orphaned
-- auto-rule with no schedule is meaningless (it was created by the schedule's form), it
-- needs to be torn down together with the Cadence.

ALTER TABLE tidings
    ADD COLUMN created_from_cadence_id TEXT;

ALTER TABLE tidings
    ADD CONSTRAINT tidings_created_from_cadence_fk
        FOREIGN KEY (created_from_cadence_id) REFERENCES cadences (id) ON DELETE CASCADE;

-- Cascade teardown (DELETE cadence) scans rules via this back-link. A partial index
-- over non-empty values - the hot path of the FK cascade doesn't scan the bulk of rules with
-- a NULL marker.
CREATE INDEX tidings_created_from_cadence_idx
    ON tidings (created_from_cadence_id)
    WHERE created_from_cadence_id IS NOT NULL;

COMMENT ON COLUMN tidings.created_from_cadence_id IS
    'Origin marker: the rule was created from the notify[] block of a Cadence form (POST /v1/cadences), ADR-052 §m. NULL = set up otherwise. FK cadences(id) ON DELETE CASCADE - tearing down the Cadence takes with it the rules it spawned (ADR-046 §9). Orthogonal to the cadence selector (subscription filter).';
