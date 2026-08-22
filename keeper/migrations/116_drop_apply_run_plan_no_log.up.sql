-- 116_drop_apply_run_plan_no_log.up.sql
--
-- NIM-698 / [ADR-0083] §8: the per-task `no_log:` key is removed from the
-- scenario DSL, so the column that persisted it has nothing left to hold.
--
-- The column was never read for its own sake. Its two consumers were
-- suppression switches: `params` was stored NULL for a no_log task, and the
-- barrier's failure summary was replaced by "(no_log task failed)". Both are
-- replaced by narrower machinery that does not need a per-task flag:
--
--   * params — masked per CELL by the seal ([ADR-010] §7.4), which knows which
--     values came from a secret source instead of trusting an author to mark
--     the whole task. A task the author forgot to flag was previously stored in
--     full; it is now masked on exactly the cells that carry a secret.
--
--   * output — masked per FIELD from the module manifest ([ADR-0083] §8),
--     declared by the module that knows its own output shape rather than by the
--     scenario author.
--
-- Dropping the column is therefore not a loss of protection: nothing that was
-- suppressed through it is now written in the clear. What IS dropped is the
-- historical record of which past tasks carried the flag — an author's
-- intention, not a value, and not recoverable from anywhere else.
--
-- `params` rows written while the flag was set stay NULL. That is correct: they
-- were never captured, and inventing them retroactively is impossible.
--
-- Idempotent: IF EXISTS, and a re-run matches nothing.

ALTER TABLE apply_run_plan
    DROP COLUMN IF EXISTS no_log;

COMMENT ON TABLE apply_run_plan IS
    'Host-invariant run task plan (name/module/passage per plan_index) for the /tasks read endpoint (NIM-37). PK (apply_id, plan_index); no FK - retention via purge_apply_run_plan.';

COMMENT ON COLUMN apply_run_plan.params IS
    'Masked operator input parameters of the task (NIM-37 S1b): seal-aware masking on the write path (audit.MaskSecretsSealed); NULL for tasks without params; template_content/render_context filtered out. jsonb object.';
