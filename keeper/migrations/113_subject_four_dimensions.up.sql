-- 113_subject_four_dimensions.up.sql
--
-- A rule's subject becomes EXACTLY ONE OF FOUR dimensions (NIM-280), replacing
-- the `coven XOR sid` shape that vigils (041), decrees (041) and rites (033)
-- each carried:
--
--     sid          TEXT[]            these hosts, by identity
--     service      TEXT  \           every host on that incarnation's roster
--     incarnation  TEXT  /           (the pair IS the address)
--     coven        TEXT[]            hosts carrying any of these labels
--     trait_key    TEXT  \           hosts carrying that pair
--     trait_value  TEXT  /
--
-- WHY the two new dimensions. `coven XOR sid` had no way to say "the hosts of
-- this incarnation". It used to have one by accident: incarnation.name doubled
-- as the root Coven label (ADR-008), so `coven: redis-prod` reached the
-- members. NIM-124 made membership a real relation and NIM-281 removed label
-- inheritance outright, which closed an escalation (a host merely TAGGED
-- `redis-prod` was indistinguishable from a member) and, with it, the only way
-- an operator had to aim a rule at an incarnation. `incarnation=` restores the
-- capability without the ambiguity: it reads `incarnation_membership`, the
-- relation, so a same-spelled host tag does not satisfy it.
--
-- WHY coven/trait now read two levels. A label set on an incarnation reaches
-- every host on its roster, resolved AT MATCH TIME (keeper/internal/subject).
-- Nothing is written to `souls`: the host still carries only what an operator
-- attached to it, so `soulprint.self.covens`, the push provider choice and
-- every RBAC scope see exactly what they saw yesterday. That is the difference
-- from the inheritance NIM-281 removed, which made the host CARRY the label and
-- therefore changed every consumer at once.
--
-- ★ THE BOUNDARY. Targeting expands; operator authorization never does. An RBAC
-- role scope (`soul.list on coven=prod`) still matches the row's OWN column
-- (rbac.CovenScopeSQL) and this migration does not touch it. Wiring the
-- two-level resolution into a scope would mean that tagging an incarnation
-- `prod` grants permanent visibility of every host on its roster — the exact
-- escalation NIM-281 closed.
--
-- ⚠ The label namespace is shared: tagging an incarnation `prod` widens every
-- existing `coven=prod` SUBJECT to its members without touching a single rule.
-- Declared behaviour, and the reason a subject and a scope must not share a
-- resolver.
--
-- WHY sid and rites.coven become arrays. The three registries now carry the
-- identical subject shape, which is what lets one predicate
-- (subject.MatchSQL) serve all three instead of three hand-written queries
-- drifting apart. rites.coven was the lone scalar; sid was scalar everywhere.
-- Pre-release, so no backcompat shim.
--
-- Deliberate loosening: `rites_coven_format` is dropped rather than ported. A
-- per-element CHECK over text[] is not declaratively expressible without a
-- trigger (migration 041 made the same call for vigils/decrees), so the label
-- form is enforced at the service layer (subject.Validate) for all three.
--
-- NO FK on (service, incarnation). Follows the precedent decrees.incarnation_name
-- set in 041: these registries are managed and outlive incarnation recreation;
-- a subject pointing at an incarnation that does not exist simply matches no
-- host (fail-closed). Creation-time existence is checked at the service layer,
-- where a typo can still be answered with a 422 instead of a rule that silently
-- never fires.

-- ---------------------------------------------------------------------------
-- vigils
-- ---------------------------------------------------------------------------

ALTER TABLE vigils
    ALTER COLUMN sid TYPE TEXT[] USING (CASE WHEN sid IS NULL THEN NULL ELSE ARRAY[sid] END);

ALTER TABLE vigils
    ADD COLUMN service     TEXT,
    ADD COLUMN incarnation TEXT,
    ADD COLUMN trait_key   TEXT,
    ADD COLUMN trait_value TEXT;

ALTER TABLE vigils DROP CONSTRAINT vigils_subject_xor;

ALTER TABLE vigils
    ADD CONSTRAINT vigils_subject_one_of CHECK (
        (CASE WHEN sid   IS NOT NULL AND array_length(sid, 1)   > 0 THEN 1 ELSE 0 END)
      + (CASE WHEN service IS NOT NULL OR incarnation IS NOT NULL   THEN 1 ELSE 0 END)
      + (CASE WHEN coven IS NOT NULL AND array_length(coven, 1) > 0 THEN 1 ELSE 0 END)
      + (CASE WHEN trait_key IS NOT NULL OR trait_value IS NOT NULL THEN 1 ELSE 0 END)
      = 1),
    ADD CONSTRAINT vigils_subject_incarnation_pair CHECK ((service IS NULL) = (incarnation IS NULL)),
    ADD CONSTRAINT vigils_subject_trait_pair       CHECK ((trait_key IS NULL) = (trait_value IS NULL)),
    ADD CONSTRAINT vigils_subject_service_format
        CHECK (service IS NULL OR service ~ '^[a-z0-9][a-z0-9-]{0,62}$'),
    ADD CONSTRAINT vigils_subject_incarnation_format
        CHECK (incarnation IS NULL OR incarnation ~ '^[a-z0-9][a-z0-9-]{0,62}$'),
    ADD CONSTRAINT vigils_subject_trait_key_format
        CHECK (trait_key IS NULL OR trait_key ~ '^[a-z][a-z0-9_.-]*$');

-- sid became text[]: the lookup operator is now && (overlap), so the index has
-- to be GIN, like vigils_coven_idx.
DROP INDEX vigils_sid_idx;
CREATE INDEX vigils_sid_idx
    ON vigils USING GIN (sid) WHERE sid IS NOT NULL;

CREATE INDEX vigils_incarnation_idx
    ON vigils (service, incarnation) WHERE incarnation IS NOT NULL;

CREATE INDEX vigils_trait_idx
    ON vigils (trait_key, trait_value) WHERE trait_key IS NOT NULL;

COMMENT ON TABLE vigils IS
    'Registry of Soul-side checks for the Oracle circuit (ADR-030). Subject = exactly one of sid / incarnation(service+name) / coven / trait (NIM-280); the active set travels to the host via VigilSnapshot (ReplaceAll).';

-- ---------------------------------------------------------------------------
-- decrees
-- ---------------------------------------------------------------------------

ALTER TABLE decrees
    ALTER COLUMN subject_sid TYPE TEXT[]
        USING (CASE WHEN subject_sid IS NULL THEN NULL ELSE ARRAY[subject_sid] END);

ALTER TABLE decrees
    ADD COLUMN subject_service     TEXT,
    ADD COLUMN subject_incarnation TEXT,
    ADD COLUMN subject_trait_key   TEXT,
    ADD COLUMN subject_trait_value TEXT;

ALTER TABLE decrees DROP CONSTRAINT decrees_subject_xor;

ALTER TABLE decrees
    ADD CONSTRAINT decrees_subject_one_of CHECK (
        (CASE WHEN subject_sid   IS NOT NULL AND array_length(subject_sid, 1)   > 0 THEN 1 ELSE 0 END)
      + (CASE WHEN subject_service IS NOT NULL OR subject_incarnation IS NOT NULL   THEN 1 ELSE 0 END)
      + (CASE WHEN subject_coven IS NOT NULL AND array_length(subject_coven, 1) > 0 THEN 1 ELSE 0 END)
      + (CASE WHEN subject_trait_key IS NOT NULL OR subject_trait_value IS NOT NULL THEN 1 ELSE 0 END)
      = 1),
    ADD CONSTRAINT decrees_subject_incarnation_pair
        CHECK ((subject_service IS NULL) = (subject_incarnation IS NULL)),
    ADD CONSTRAINT decrees_subject_trait_pair
        CHECK ((subject_trait_key IS NULL) = (subject_trait_value IS NULL)),
    ADD CONSTRAINT decrees_subject_service_format
        CHECK (subject_service IS NULL OR subject_service ~ '^[a-z0-9][a-z0-9-]{0,62}$'),
    ADD CONSTRAINT decrees_subject_incarnation_format
        CHECK (subject_incarnation IS NULL OR subject_incarnation ~ '^[a-z0-9][a-z0-9-]{0,62}$'),
    ADD CONSTRAINT decrees_subject_trait_key_format
        CHECK (subject_trait_key IS NULL OR subject_trait_key ~ '^[a-z][a-z0-9_.-]*$');

-- The Decree hot path selects by on_beacon (decrees_on_beacon_idx) and filters
-- the handful of survivors by subject, so the subject columns need no index of
-- their own here — unlike vigils/rites, where the subject IS the lookup key.

COMMENT ON TABLE decrees IS
    'Registry of Oracle reactor rules (ADR-030). Default-deny; subject = exactly one of sid / incarnation(service+name) / coven / trait (NIM-280); target incarnation_name (ServiceRef is resolved from it); action = named scenario (whitelist).';

-- ---------------------------------------------------------------------------
-- rites
-- ---------------------------------------------------------------------------

ALTER TABLE rites
    ALTER COLUMN sid TYPE TEXT[] USING (CASE WHEN sid IS NULL THEN NULL ELSE ARRAY[sid] END);

-- rites.coven was the lone scalar coven of the three registries.
ALTER TABLE rites DROP CONSTRAINT rites_coven_format;
ALTER TABLE rites
    ALTER COLUMN coven TYPE TEXT[] USING (CASE WHEN coven IS NULL THEN NULL ELSE ARRAY[coven] END);

ALTER TABLE rites
    ADD COLUMN service     TEXT,
    ADD COLUMN incarnation TEXT,
    ADD COLUMN trait_key   TEXT,
    ADD COLUMN trait_value TEXT;

ALTER TABLE rites DROP CONSTRAINT rites_subject_xor;

ALTER TABLE rites
    ADD CONSTRAINT rites_subject_one_of CHECK (
        (CASE WHEN sid   IS NOT NULL AND array_length(sid, 1)   > 0 THEN 1 ELSE 0 END)
      + (CASE WHEN service IS NOT NULL OR incarnation IS NOT NULL   THEN 1 ELSE 0 END)
      + (CASE WHEN coven IS NOT NULL AND array_length(coven, 1) > 0 THEN 1 ELSE 0 END)
      + (CASE WHEN trait_key IS NOT NULL OR trait_value IS NOT NULL THEN 1 ELSE 0 END)
      = 1),
    ADD CONSTRAINT rites_subject_incarnation_pair CHECK ((service IS NULL) = (incarnation IS NULL)),
    ADD CONSTRAINT rites_subject_trait_pair       CHECK ((trait_key IS NULL) = (trait_value IS NULL)),
    ADD CONSTRAINT rites_subject_service_format
        CHECK (service IS NULL OR service ~ '^[a-z0-9][a-z0-9-]{0,62}$'),
    ADD CONSTRAINT rites_subject_incarnation_format
        CHECK (incarnation IS NULL OR incarnation ~ '^[a-z0-9][a-z0-9-]{0,62}$'),
    ADD CONSTRAINT rites_subject_trait_key_format
        CHECK (trait_key IS NULL OR trait_key ~ '^[a-z][a-z0-9_.-]*$');

DROP INDEX rites_sid_idx;
CREATE INDEX rites_sid_idx
    ON rites USING GIN (sid) WHERE sid IS NOT NULL;

DROP INDEX rites_coven_idx;
CREATE INDEX rites_coven_idx
    ON rites USING GIN (coven) WHERE coven IS NOT NULL;

CREATE INDEX rites_incarnation_idx
    ON rites (service, incarnation) WHERE incarnation IS NOT NULL;

CREATE INDEX rites_trait_idx
    ON rites (trait_key, trait_value) WHERE trait_key IS NOT NULL;

COMMENT ON TABLE rites IS
    'Augur grants (ADR-025): subject x omen -> allow + delegate + token parameters. Subject = exactly one of sid / incarnation(service+name) / coven / trait (NIM-280). omen ON DELETE CASCADE.';

-- ---------------------------------------------------------------------------
-- Stale column comments left by NIM-124 / NIM-121 / NIM-281
--
-- Both describe rules that no longer exist. A COMMENT is what an operator reads
-- off a live database with \d+, so a stale one is a documented lie about the
-- authorization model — the same class of drift NIM-281 spent its diff removing
-- from the .sql headers, missed here because these are runtime object comments
-- rather than file prose.
-- ---------------------------------------------------------------------------

COMMENT ON COLUMN incarnation.covens IS
    'Declared environment tags for the incarnation (ADR-008). NOT inherited by member hosts (NIM-281). An RBAC coven= scope on incarnation operations matches these DECLARED labels only - the name is an identity, reached through the incarnation= dimension (NIM-124). A rule SUBJECT spelled coven= additionally reaches every host on this incarnation''s roster (NIM-280) - targeting, never authorization.';

COMMENT ON COLUMN incarnation.traits IS
    'Trait - operator-set key-value labels for the incarnation (ADR-060 amend, R1); value is scalar|list. NOT projected onto souls.traits: the sync-hook was removed by NIM-121 and the read-time union by NIM-281 - a host carries only the traits an operator set on it. A rule SUBJECT spelled trait.<key>= additionally reaches every host on this incarnation''s roster (NIM-280).';
