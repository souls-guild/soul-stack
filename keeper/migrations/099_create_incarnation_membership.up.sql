-- 099_create_incarnation_membership.up.sql
--
-- ADR-008 amendment (2026-07-17, NIM-124): incarnation.name is NOT a Coven —
-- membership becomes a first-class M:N relation. Previously membership was
-- derived from the fact `incarnation.name ∈ souls.coven[]` (the "root Coven
-- label"); this conflated two axes on one column — (1) membership (which
-- incarnation a host belongs to) and (2) stable logical tags (cluster / project /
-- environment / datacenter). This migration splits them: membership moves into
-- its own table, and the synthetic incarnation-name value is stripped out of
-- souls.coven[] so the coven axis holds only real stable tags.
--
-- M:N is deliberate (a host MAY be a member of several incarnations) — the
-- "one scenario run = one incarnation" invariant is unchanged (it constrains a
-- run, not a host). Modeled on incarnation_choir_voices (060_create_choirs).
--
-- FK:
--   * incarnation_name → incarnation(name) ON DELETE CASCADE
--     (removing an incarnation removes its memberships).
--   * sid → souls(sid) ON DELETE CASCADE
--     (removing a Soul from the registry removes its memberships).
--   * bound_by_aid → operators(aid) ON DELETE SET NULL
--     (the operator that bound the host; deleting the operator doesn't lose the
--     membership).

CREATE TABLE incarnation_membership (
    incarnation_name TEXT        NOT NULL,
    sid              TEXT        NOT NULL,
    bound_at         TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    bound_by_aid     TEXT,

    CONSTRAINT incarnation_membership_pkey
        PRIMARY KEY (incarnation_name, sid),
    CONSTRAINT incarnation_membership_incarnation_fk
        FOREIGN KEY (incarnation_name) REFERENCES incarnation (name) ON DELETE CASCADE,
    CONSTRAINT incarnation_membership_sid_fk
        FOREIGN KEY (sid) REFERENCES souls (sid) ON DELETE CASCADE,
    CONSTRAINT incarnation_membership_bound_by_aid_fk
        FOREIGN KEY (bound_by_aid) REFERENCES operators (aid) ON DELETE SET NULL
);

-- Roster read (topology.LoadIncarnationHosts): all members of one incarnation.
CREATE INDEX incarnation_membership_incarnation_idx
    ON incarnation_membership (incarnation_name);

-- Reverse lookup (a host's incarnations, cascade-on-soul-delete, form-prep).
CREATE INDEX incarnation_membership_sid_idx
    ON incarnation_membership (sid);

COMMENT ON TABLE incarnation_membership IS
    'Incarnation membership — first-class M:N relation host↔incarnation (ADR-008 amendment 2026-07-17, NIM-124). Source of truth for "which incarnation a host belongs to"; replaces the derived fact incarnation.name ∈ souls.coven[]. bound_at/bound_by_aid for audit. A single SID may be a member of several incarnations (PK includes incarnation_name; deliberately no global sid uniqueness).';

-- (b) Backfill membership from the pre-existing incarnation.name ∈ souls.coven[]
-- fact, so no host loses its incarnation on the cutover.
INSERT INTO incarnation_membership (incarnation_name, sid)
SELECT i.name, s.sid
FROM incarnation i
JOIN souls s ON i.name = ANY(s.coven)
ON CONFLICT DO NOTHING;

-- (c) Strip every incarnation-name value out of souls.coven[] so the coven axis
-- holds only real stable tags. A host may carry several incarnation names —
-- remove every coven element that equals SOME incarnation name (set-based, not a
-- single array_remove). unnest WITH ORDINALITY preserves tag order; COALESCE
-- yields an empty array (column is NOT NULL) when all elements were names.
UPDATE souls s
SET coven = (
    SELECT COALESCE(array_agg(u.c ORDER BY u.ord), ARRAY[]::text[])
    FROM unnest(s.coven) WITH ORDINALITY AS u(c, ord)
    WHERE NOT EXISTS (SELECT 1 FROM incarnation i WHERE i.name = u.c)
)
WHERE EXISTS (
    SELECT 1 FROM unnest(s.coven) AS v(c)
    JOIN incarnation i ON i.name = v.c
);
