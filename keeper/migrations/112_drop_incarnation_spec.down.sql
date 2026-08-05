-- 112_drop_incarnation_spec.down.sql
--
-- Restores the COLUMN so a rolled-back keeper can start, and nothing else. The
-- data is gone and cannot be recomputed: `spec` held the create request, which
-- was never derived from anything else in the database.
--
-- What a rolled-back keeper actually loses is narrower than it looks. On the
-- create path it would read `spec.input` for rerun-last and find `{}`, so a rerun
-- of a failed bootstrap would start with no input — which is why a rollback
-- across this migration should be paired with re-running the affected creates
-- rather than with rerun-last. Day-2 reruns are unaffected: that path read
-- apply_runs.recipe, which this migration never touched.
--
-- NOT NULL DEFAULT '{}' so existing rows come back valid rather than NULL, which
-- the pre-NIM-408 scan code would have unmarshalled into a nil map anyway.

ALTER TABLE incarnation ADD COLUMN spec JSONB NOT NULL DEFAULT '{}'::jsonb;
