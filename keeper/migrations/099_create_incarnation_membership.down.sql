-- 099_create_incarnation_membership.down.sql
--
-- Drops the membership relation. The up-migration's data steps — the backfill
-- and the coven strip (removing incarnation-name values from souls.coven[]) —
-- are NOT reversed: the stripped synthetic values are not restored into
-- souls.coven[] (this matches the repo norm for data migrations; recovery of the
-- pre-cutover coven state, if ever needed, is via state_history / a backup).

DROP TABLE IF EXISTS incarnation_membership;
