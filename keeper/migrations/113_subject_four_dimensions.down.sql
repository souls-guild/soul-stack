-- 113_subject_four_dimensions.down.sql
--
-- Restores the `coven XOR sid` shape so a rolled-back keeper can start.
--
-- ⚠ LOSSY, in two ways an operator must plan around:
--
--  1. Rules written on the two new dimensions cannot be expressed in the old
--     shape at all. They are DELETED, not silently degraded: a Vigil whose
--     subject was `incarnation=redis.redis-prod` has no `coven XOR sid` form,
--     and turning it into some approximation would hand the rule a different set
--     of hosts than the operator wrote it for. For a Rite that approximation
--     would be a widened GRANT. Deleting is the only fail-closed answer.
--
--  2. sid and rites.coven go back to scalar and keep only the FIRST element.
--     A rule listing three SIDs comes back covering one host.
--
-- So a rollback across this migration silently narrows what fires and what is
-- granted. Re-create the affected rules from the operator's source of truth
-- rather than trusting what survives here.

DELETE FROM vigils  WHERE incarnation IS NOT NULL OR trait_key IS NOT NULL;
DELETE FROM decrees WHERE subject_incarnation IS NOT NULL OR subject_trait_key IS NOT NULL;
DELETE FROM rites   WHERE incarnation IS NOT NULL OR trait_key IS NOT NULL;

-- ---------------------------------------------------------------------------
-- vigils
-- ---------------------------------------------------------------------------

DROP INDEX vigils_trait_idx;
DROP INDEX vigils_incarnation_idx;

ALTER TABLE vigils
    DROP CONSTRAINT vigils_subject_one_of,
    DROP CONSTRAINT vigils_subject_incarnation_pair,
    DROP CONSTRAINT vigils_subject_trait_pair,
    DROP CONSTRAINT vigils_subject_service_format,
    DROP CONSTRAINT vigils_subject_incarnation_format,
    DROP CONSTRAINT vigils_subject_trait_key_format;

ALTER TABLE vigils
    DROP COLUMN service,
    DROP COLUMN incarnation,
    DROP COLUMN trait_key,
    DROP COLUMN trait_value;

DROP INDEX vigils_sid_idx;
ALTER TABLE vigils ALTER COLUMN sid TYPE TEXT USING (sid[1]);
CREATE INDEX vigils_sid_idx ON vigils (sid) WHERE sid IS NOT NULL;

ALTER TABLE vigils
    ADD CONSTRAINT vigils_subject_xor
        CHECK ((coven IS NOT NULL AND array_length(coven, 1) > 0) <> (sid IS NOT NULL));

COMMENT ON TABLE vigils IS
    'Registry of Soul-side checks for the Oracle circuit (ADR-030). Subject coven XOR sid; the active set travels to the host via VigilSnapshot (ReplaceAll).';

-- ---------------------------------------------------------------------------
-- decrees
-- ---------------------------------------------------------------------------

ALTER TABLE decrees
    DROP CONSTRAINT decrees_subject_one_of,
    DROP CONSTRAINT decrees_subject_incarnation_pair,
    DROP CONSTRAINT decrees_subject_trait_pair,
    DROP CONSTRAINT decrees_subject_service_format,
    DROP CONSTRAINT decrees_subject_incarnation_format,
    DROP CONSTRAINT decrees_subject_trait_key_format;

ALTER TABLE decrees
    DROP COLUMN subject_service,
    DROP COLUMN subject_incarnation,
    DROP COLUMN subject_trait_key,
    DROP COLUMN subject_trait_value;

ALTER TABLE decrees ALTER COLUMN subject_sid TYPE TEXT USING (subject_sid[1]);

ALTER TABLE decrees
    ADD CONSTRAINT decrees_subject_xor
        CHECK ((subject_coven IS NOT NULL AND array_length(subject_coven, 1) > 0) <> (subject_sid IS NOT NULL));

COMMENT ON TABLE decrees IS
    'Registry of Oracle reactor rules (ADR-030). Default-deny; subject coven XOR sid; target incarnation_name (ServiceRef is resolved from it); action = named scenario (whitelist).';

-- ---------------------------------------------------------------------------
-- rites
-- ---------------------------------------------------------------------------

DROP INDEX rites_trait_idx;
DROP INDEX rites_incarnation_idx;

ALTER TABLE rites
    DROP CONSTRAINT rites_subject_one_of,
    DROP CONSTRAINT rites_subject_incarnation_pair,
    DROP CONSTRAINT rites_subject_trait_pair,
    DROP CONSTRAINT rites_subject_service_format,
    DROP CONSTRAINT rites_subject_incarnation_format,
    DROP CONSTRAINT rites_subject_trait_key_format;

ALTER TABLE rites
    DROP COLUMN service,
    DROP COLUMN incarnation,
    DROP COLUMN trait_key,
    DROP COLUMN trait_value;

DROP INDEX rites_sid_idx;
ALTER TABLE rites ALTER COLUMN sid TYPE TEXT USING (sid[1]);
CREATE INDEX rites_sid_idx ON rites (sid) WHERE sid IS NOT NULL;

DROP INDEX rites_coven_idx;
ALTER TABLE rites ALTER COLUMN coven TYPE TEXT USING (coven[1]);
CREATE INDEX rites_coven_idx ON rites (coven) WHERE coven IS NOT NULL;

ALTER TABLE rites
    ADD CONSTRAINT rites_subject_xor
        CHECK ((coven IS NOT NULL) <> (sid IS NOT NULL)),
    ADD CONSTRAINT rites_coven_format
        CHECK (coven IS NULL OR coven ~ '^[a-z0-9][a-z0-9-]*$');

COMMENT ON TABLE rites IS
    'Augur grants (ADR-025): subject (coven XOR sid) x omen -> allow + delegate + token parameters. omen ON DELETE CASCADE.';

-- ---------------------------------------------------------------------------
-- Column comments: back to what 046 / 088 shipped, stale claims included. A
-- down migration restores the previous state, it does not improve on it.
-- ---------------------------------------------------------------------------

COMMENT ON COLUMN incarnation.covens IS
    'Declared environment tags for incarnation (ADR-008). RBAC scope coven= for incarnation operations = covens ∪ {name}.';

COMMENT ON COLUMN incarnation.traits IS
    'Trait - operator-set key-value labels for the incarnation (ADR-060 amend, R1); value is scalar|list. Source of truth, projected into souls.traits of member hosts via a sync-hook.';
