-- 096_create_apply_run_plan.up.sql
--
-- Persistence of the host-invariant "run task plan" (NIM-37): one row per
-- (apply_id, plan_index) with metadata of the rendered task - name/module/no_log/
-- passage. Read by the GET /v1/incarnations/{name}/runs/{apply_id}/
-- tasks read endpoint, which joins the plan with per-host results from audit_log (task.executed)
-- by plan_index.
--
-- Why a separate table instead of re-rendering the scenario YAML: the only reliable
-- source of name/module/no_log is the rendered plan at dispatch time. Re-render
-- was rejected (loop/CEL expansion breaks plan_index correspondence; a snapshot ref exists
-- for not all runs). The metadata is host-invariant (the same on all hosts),
-- so we store it ONCE per run by plan_index, and NOT per-host - per-host status/
-- output is taken from audit_log (ADR-012: per-task is not in apply_runs).
--
-- name/module/no_log - NOT a secret (this is the task's address and type, not param values),
-- so masking is not needed. Task params (which may carry secrets) are NOT stored in S1a -
-- deferred to S1b with secret masking.
--
-- PK (apply_id, plan_index): plan_index is a GLOBAL cross-cutting task index across
-- the entire plan (all Passages) = RenderedTask.Index (ADR-056 §S1 Variant B), the same
-- key by which task.executed correlates the task in audit_log.
--
-- NO FK: apply_id by itself is not a PK in any table (apply_runs is keyed by
-- (apply_id, sid) - many rows per apply_id). Retention is handled by a separate Reaper rule
-- purge_apply_run_plan (migration 097), since there is no cascading FK for cleanup.

CREATE TABLE apply_run_plan (
    apply_id   TEXT        NOT NULL,
    plan_index INT         NOT NULL,
    name       TEXT        NOT NULL,
    module     TEXT        NOT NULL,
    no_log     BOOLEAN     NOT NULL DEFAULT FALSE,
    passage    INT         NOT NULL DEFAULT 0,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),

    PRIMARY KEY (apply_id, plan_index)
);

-- Loading the entire run plan by the /tasks endpoint (per apply_id → all plan_index).
CREATE INDEX apply_run_plan_apply_idx
    ON apply_run_plan (apply_id);

COMMENT ON TABLE apply_run_plan IS
    'Host-invariant run task plan (name/module/no_log/passage per plan_index) for the /tasks read endpoint (NIM-37). PK (apply_id, plan_index); no FK - retention via purge_apply_run_plan.';
