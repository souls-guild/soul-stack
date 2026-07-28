-- 106_prune_projected_soul_traits.up.sql
--
-- ADR-080 (label inheritance): the materialized projection of
-- `incarnation.traits` onto member hosts' `souls.traits` is removed. A label now
-- lives only where the operator attached it, and a host sees the union of its own
-- labels and those of the incarnations it belongs to, computed at read time.
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
-- host-local and must stay. Under ADR-080 both survive anyway: the effective
-- value becomes the union of the two.
--
-- Effective labels are UNCHANGED by this prune. Every pair it deletes comes
-- straight back through inheritance, from the incarnation that already carries
-- it — only the storage location is corrected. Deliberately not reversible in
-- data (see the .down.sql): the pruned pairs are not lost, they are one join
-- away.
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
