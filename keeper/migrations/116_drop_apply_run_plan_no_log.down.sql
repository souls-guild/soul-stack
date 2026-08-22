-- 116_drop_apply_run_plan_no_log.down.sql
--
-- Reverse of 116: the column comes back with its original type and default, and
-- every row reads FALSE.
--
-- The per-row values are NOT restored, because they cannot be: `no_log` was a
-- projection of a scenario key that the rolled-back code will read from the
-- YAML again on the next run. Restoring the schema is enough for a pre-NIM-698
-- keeper to start and write the column on its own; the historical rows simply
-- claim no task was ever flagged, which understates suppression for runs that
-- are already finished and whose params were never captured in the first place.

ALTER TABLE apply_run_plan
    ADD COLUMN IF NOT EXISTS no_log BOOLEAN NOT NULL DEFAULT FALSE;

COMMENT ON TABLE apply_run_plan IS
    'Host-invariant run task plan (name/module/no_log/passage per plan_index) for the /tasks read endpoint (NIM-37). PK (apply_id, plan_index); no FK - retention via purge_apply_run_plan.';

COMMENT ON COLUMN apply_run_plan.params IS
    'Masked operator input parameters of the task (NIM-37 S1b): seal-aware masking on the write path (audit.MaskSecretsSealed); NULL for no_log tasks and tasks without params; template_content/render_context filtered out. jsonb object.';
