-- 124_add_apply_run_plan_waves.up.sql
--
-- The waves a `serial:` step rolled its hosts in (NIM-996). The dispatcher splits
-- a step's hosts, sorted by SID, into waves of the `serial:` width and sends them
-- one wave at a time; until now the split was recomputed and thrown away, so the
-- run view could not tell a host sent in the second wave from one sent in the
-- first, nor list a host whose wave never started.
--
-- A JSON array of waves in dispatch order, each an array of SIDs:
-- [["a.example.com"], ["b.example.com"], ["c.example.com"]]. Written once per step,
-- before its first wave, on the plan row of every host task of that step; a keeper
-- task sharing a staged Passage rides no wave and keeps NULL. It lists every host
-- the step was to be sent to, so a wave the run ended before keeps its hosts. A host
-- the Keeper closed before the first wave is in no wave: it was sent nothing and
-- took no wave's place.
--
-- NULL for a step sent in one wave - no `serial:`, or a width that covers every
-- host - and for every run before this migration. jsonb rather than text[][]:
-- the last wave is usually shorter, and a Postgres array must be rectangular.
--
-- Retention is the plan row's: purge_apply_run_plan (migration 097) removes it.

ALTER TABLE apply_run_plan ADD COLUMN waves jsonb;

COMMENT ON COLUMN apply_run_plan.waves IS
    'Waves of a serial: step in dispatch order, each a JSON array of SIDs (NIM-996); written before the first wave on every host task row of the step; NULL for a step sent in one wave and for a keeper task.';
