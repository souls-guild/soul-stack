-- 098_add_apply_run_plan_params.up.sql
--
-- The params column (jsonb NULL) for apply_run_plan (NIM-37 S1b): operator
-- input parameters of the rendered task, shown by the read endpoint /tasks
-- (GET /v1/incarnations/{name}/runs/{apply_id}/tasks). Params are host-invariant
-- (the same on all hosts of the run), so they are stored ONCE per (apply_id,
-- plan_index) - like name/module/no_log/passage (migration 096).
--
-- Params values can carry secrets (after vault-resolve+CEL), so they
-- are masked by the seal-aware mechanism (audit.MaskSecretsSealed - the same one that masks
-- status_details/error_summary: sealed run paths + vault + regex-last-resort)
-- on the write path BEFORE the write, a second barrier on top of the fact that params are rendered.
-- no_log task -> NULL (params are not stored, symmetric with register_data suppression).
-- Transport keys of core.file.rendered (template_content/render_context) are
-- NOT saved in params - this is not operator-facing "input data" for the task.
--
-- Retention is unchanged: the same plan row that purge_apply_run_plan cleans up
-- (migration 097) - no separate cleanup is needed for params.

ALTER TABLE apply_run_plan
    ADD COLUMN params jsonb;

COMMENT ON COLUMN apply_run_plan.params IS
    'Masked operator input parameters of the task (NIM-37 S1b): seal-aware masking on the write path (audit.MaskSecretsSealed); NULL for no_log tasks and tasks without params; template_content/render_context filtered out. jsonb object.';
