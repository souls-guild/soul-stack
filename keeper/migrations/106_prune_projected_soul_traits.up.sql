-- 106_prune_projected_soul_traits.up.sql
--
-- The materialized projection of `incarnation.traits` onto member hosts'
-- `souls.traits` is removed. A label lives only where the operator attached it:
-- `souls.traits` is the host's own set and nothing writes to it but an explicit
-- per-host assign (NIM-281 — ADR-080's read-time union was tried and reverted).
--
-- Whatever the projection wrote last is still sitting in `souls.traits` on the
-- day it stops running. Left alone, those pairs would silently change meaning:
-- yesterday they were a copy that tracked the incarnation, today they would be
-- labels attached to the host — surviving the removal of the key from the
-- incarnation, and permanently duplicating a label the operator only ever set in
-- one place. In an RBAC scope dimension (`trait.<key>=v`, NIM-128) that is a
-- grant nobody can find the source of.
--
-- So: delete from each host's `souls.traits` every key whose value EQUALS what
-- one of its incarnations carries for that key. Equality, not key presence, is
-- the test — a host key that merely shares a NAME with an incarnation key
-- (`owner=bobik` on the host, `owner=dba` on the incarnation) is genuinely
-- host-local and must stay — the two axes are independent, and only an exact
-- value match identifies a pair as a copy the projection wrote.
--
-- What the prune deletes is deleted for real: a rule matching `trait.<key>=v` on
-- the HOST stops matching these hosts. That is the intended outcome, not a side
-- effect — the pair was never something an operator attached to the host, and
-- after NIM-281 there is no read-time union to bring it back. The pair is still
-- on the incarnation that carried it, reachable by a rule aimed at the
-- incarnation. Deliberately not reversible in data (see the .down.sql).
--
-- Hosts with no membership and hosts whose incarnations carry no traits are
-- untouched (the NOT EXISTS below matches nothing for them).

UPDATE souls s
SET traits = (
    SELECT COALESCE(jsonb_object_agg(kv.key, kv.value), '{}'::jsonb)
    FROM jsonb_each(s.traits) AS kv
    WHERE NOT EXISTS (
        SELECT 1
        FROM incarnation_membership m
        JOIN incarnation i ON i.name = m.incarnation_name
        WHERE m.sid = s.sid
          AND i.traits -> kv.key = kv.value
    )
)
WHERE s.traits <> '{}'::jsonb
  AND EXISTS (
      SELECT 1
      FROM jsonb_each(s.traits) AS kv
      JOIN incarnation_membership m ON m.sid = s.sid
      JOIN incarnation i ON i.name = m.incarnation_name
      WHERE i.traits -> kv.key = kv.value
  );
