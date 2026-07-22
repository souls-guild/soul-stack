-- 060_create_choirs.up.sql
--
-- ADR-044 → S-T2 schema: Choir (a named host topology within an
-- incarnation) + Voice (SID membership in a Choir). The source of truth for the
-- declared topology is separate PG tables + CRUD (NOT `incarnation.state`, which
-- is committed only under the cross-host barrier - ADR-044 item 4).
--
-- Three DIFFERENT layers (ADR-044 item 1, do not conflate):
--   * membership = `incarnation.name` in `souls.coven[]` (as before; untouched);
--   * coven      = stable logical tags (ADR-008);
--   * Choir      = a host's named position WITHIN an incarnation.
--
-- Choir absorbs the declared role (`incarnation.spec.hosts[].role` → `voice.role`,
-- ADR-044 item 2): `voice.role` is the sole source of the declared topology
-- (feeds `soulprint.hosts[].role` at S-T4).
--
-- Multi-incarnation membership (ADR-044 item 3): a single SID can legally be a Voice in
-- Choirs of DIFFERENT incarnations - the PK includes incarnation_name, so there is
-- deliberately NO global sid uniqueness.
--
-- FK:
--   * incarnation_choirs.incarnation_name → incarnation(name) ON DELETE CASCADE
--     (removing an incarnation removes its Choirs and cascades to their Voices).
--   * incarnation_choirs.created_by_aid   → operators(aid) ON DELETE SET NULL
--     (the creating Archon; deleting the operator doesn't lose the Choir).
--   * incarnation_choir_voices (incarnation_name, choir_name)
--       → incarnation_choirs (incarnation_name, choir_name) ON DELETE CASCADE.
--   * incarnation_choir_voices.sid          → souls(sid) ON DELETE CASCADE
--     (removing a Soul from the registry removes its Voices; membership = souls.coven -
--     the invariant "a Voice only for an incarnation member" is checked at the CRUD
--     layer, not via FK, since membership is the value of a coven array element, not an FK).
--   * incarnation_choir_voices.added_by_aid → operators(aid) ON DELETE SET NULL.

CREATE TABLE incarnation_choirs (
    incarnation_name TEXT        NOT NULL,
    choir_name       TEXT        NOT NULL,
    description      TEXT,
    min_size         INT,
    max_size         INT,
    created_at       TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    created_by_aid   TEXT,

    CONSTRAINT incarnation_choirs_pkey
        PRIMARY KEY (incarnation_name, choir_name),
    CONSTRAINT incarnation_choirs_name_format
        CHECK (choir_name ~ '^[a-z][a-z0-9_-]*$'),
    CONSTRAINT incarnation_choirs_min_size_positive
        CHECK (min_size IS NULL OR min_size > 0),
    CONSTRAINT incarnation_choirs_max_size_positive
        CHECK (max_size IS NULL OR max_size > 0),
    CONSTRAINT incarnation_choirs_min_le_max
        CHECK (min_size IS NULL OR max_size IS NULL OR min_size <= max_size),
    CONSTRAINT incarnation_choirs_incarnation_fk
        FOREIGN KEY (incarnation_name) REFERENCES incarnation (name) ON DELETE CASCADE,
    CONSTRAINT incarnation_choirs_created_by_aid_fk
        FOREIGN KEY (created_by_aid) REFERENCES operators (aid) ON DELETE SET NULL
);

CREATE TABLE incarnation_choir_voices (
    incarnation_name TEXT        NOT NULL,
    choir_name       TEXT        NOT NULL,
    sid              TEXT        NOT NULL,
    role             TEXT,
    position         INT,
    added_at         TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    added_by_aid     TEXT,

    CONSTRAINT incarnation_choir_voices_pkey
        PRIMARY KEY (incarnation_name, choir_name, sid),
    CONSTRAINT incarnation_choir_voices_position_non_negative
        CHECK (position IS NULL OR position >= 0),
    CONSTRAINT incarnation_choir_voices_choir_fk
        FOREIGN KEY (incarnation_name, choir_name)
        REFERENCES incarnation_choirs (incarnation_name, choir_name) ON DELETE CASCADE,
    CONSTRAINT incarnation_choir_voices_sid_fk
        FOREIGN KEY (sid) REFERENCES souls (sid) ON DELETE CASCADE,
    CONSTRAINT incarnation_choir_voices_added_by_aid_fk
        FOREIGN KEY (added_by_aid) REFERENCES operators (aid) ON DELETE SET NULL
);

-- Lookup of all Voices for a single host (for the virtual projection
-- `soulprint.self.choirs` - S-T4 Keeper-side join per-SID).
CREATE INDEX incarnation_choir_voices_sid_idx
    ON incarnation_choir_voices (sid);

COMMENT ON TABLE incarnation_choirs IS
    'Choir - a named host topology within an incarnation (ADR-044, S-T2). A declared group (a "choir part"); source of truth for the declared topology, NOT incarnation.state. Choir != coven (ADR-008) and != membership (souls.coven).';

COMMENT ON TABLE incarnation_choir_voices IS
    'Voice - SID membership in a Choir (ADR-044, S-T2). role - the absorbed declared role (spec.hosts[].role, ADR-044 item 2); position - ordinal index within the part. Invariant: the SID is already an incarnation member (souls.coven contains incarnation.name) - checked at the CRUD layer. A single SID can legally be a Voice in different incarnations (the PK includes incarnation_name; deliberately no global sid uniqueness).';
