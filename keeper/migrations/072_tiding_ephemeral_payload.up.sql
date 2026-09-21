-- 072_tiding_ephemeral_payload.up.sql
--
-- ADR-052 Amendment (2026-06-11, "one-off notifications + flexible body"), slice N1.
--
-- Extends the `tidings` registry with four additive columns (no existing
-- field/contract changes semantics):
--   - ephemeral   - marks a ONE-OFF rule tied to a single run
--     (ADR-052(g)). A persistent rule (as in S1) - ephemeral=false, voyage_id NULL.
--   - voyage_id   - selector binding to a specific Voyage (ADR-052(g)). Required
--     for ephemeral rules (the rule matches ONLY events of its own run);
--     NULL for persistent rules.
--   - annotations - static operator-supplied fields, merged into the webhook
--     delivery body under a new top-level `annotations` key (ADR-052(h)/(i)). JSONB object.
--   - projection  - allow-list of paths from the event payload (ADR-052(h)). Non-empty ->
--     the body is narrowed to a subset; empty (DEFAULT) = the current full form.
--
-- Merging annotations / projection happens in the delivery worker (off-path, N3);
-- the migration and domain layer (N1) only STORE the fields. voyage_id is TEXT
-- (Voyage.voyage_id), with no FK to voyages: an ephemeral Tiding is created
-- atomically by keeper in the same tx as the Voyage (ADR-052(g) N2), and cleanup
-- of orphans happens via Voyage terminal state + Reaper TTL, not cascade.

ALTER TABLE tidings
    ADD COLUMN ephemeral   BOOLEAN     NOT NULL DEFAULT false,
    ADD COLUMN voyage_id   TEXT,
    ADD COLUMN annotations JSONB       NOT NULL DEFAULT '{}'::jsonb,
    ADD COLUMN projection   TEXT[]     NOT NULL DEFAULT '{}';

-- Invariant ephemeral<=>voyage_id (ADR-052(g)): a one-off rule is tied to a run
-- (voyage_id IS NOT NULL), a persistent one has no binding (voyage_id IS NULL). Belt
-- and suspenders: the domain layer checks the same invariant (ErrEphemeralRequiresVoyage).
ALTER TABLE tidings
    ADD CONSTRAINT tidings_ephemeral_voyage_consistent
        CHECK (ephemeral = (voyage_id IS NOT NULL));

-- The ephemeral-match dispatcher (N1) and cleanup of orphaned ephemeral Tidings
-- (Reaper TTL, N2) look up rules by voyage_id. Partial index ONLY on ephemeral
-- rows: persistent rules (the majority) don't bloat the index.
CREATE INDEX tidings_ephemeral_voyage_idx
    ON tidings (voyage_id) WHERE ephemeral;

COMMENT ON COLUMN tidings.ephemeral IS
    'One-off rule tied to a single run (ADR-052(g)). false = persistent (voyage_id NULL).';
COMMENT ON COLUMN tidings.voyage_id IS
    'Selector binding to a specific Voyage (ADR-052(g)). NOT NULL <=> ephemeral.';
COMMENT ON COLUMN tidings.annotations IS
    'Static operator-supplied fields, merged under the annotations key into the webhook body (ADR-052(h)/(i)).';
COMMENT ON COLUMN tidings.projection IS
    'Allow-list of payload paths for narrowing the body (ADR-052(h)). Empty = full form.';
