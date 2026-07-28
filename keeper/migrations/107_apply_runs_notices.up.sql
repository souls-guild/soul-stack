-- 107_apply_runs_notices.up.sql
--
-- Advisory findings of a run, kept with the run (NIM-237, ADR-0076 deprecation
-- policy). A Soul checks a task's params against the manifest compiled into
-- ITSELF, so it is the only side that knows a param is deprecated; until now
-- that knowledge went to the agent's log and no further, which made the
-- deprecation window unusable for exactly the operator it protects.
--
-- Why a jsonb column on the run row rather than a table of its own: this is the
-- RECORD OF ONE RUN, read back with the run row that already carries
-- error_summary, and never queried across runs. A separate table would buy
-- indexing for a fleet-wide "who still passes this param" rollup — and that
-- rollup should not be built on run history anyway: it would answer "who passed
-- it in the runs we happened to observe", missing an incarnation nobody has run
-- this month and still accusing one fixed yesterday. The honest source for that
-- question is the definitions, not the log of applies.
--
-- Appended, never rewritten: notices arrive on separate TaskEvents, possibly at
-- DIFFERENT keeper instances (ADR-002), so accumulation is a single atomic
-- `notices || $1` UPDATE rather than read-modify-write. Duplicates across tasks
-- are expected (one deprecated param used by twenty tasks) and are collapsed on
-- READ by (code, module, param) — the write path stays a blind append, which is
-- what makes it safe to run concurrently.
--
-- DEFAULT '[]' rather than NULL so the append needs no COALESCE and a reader
-- never distinguishes "old row" from "nothing to say" — for this column they
-- mean the same thing.

ALTER TABLE apply_runs
    ADD COLUMN IF NOT EXISTS notices JSONB NOT NULL DEFAULT '[]'::jsonb;
