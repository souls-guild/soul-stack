-- 123_create_apply_run_plan_host_params.up.sql
--
-- Per-host params of a run task (NIM-985). Since NIM-908 a task can render
-- different params on different hosts; apply_run_plan keeps ONE masked map per
-- task, with every top-level key whose value differs between hosts replaced by
-- `<per-host>`. This table holds what that marker hides: one row per host the
-- task was rendered for, carrying that host's value of each marked key it
-- rendered and nothing else. A host's input is the plan row's unmarked keys
-- plus its row.
--
-- Rows exist only for a task whose params differ between hosts after masking;
-- a host-invariant task writes none. Values are masked on the write path by the
-- same seal-aware masker as apply_run_plan.params, per host, before the
-- comparison - a sealed cell masks to the same value on every host that has
-- it, so it lands here only as that mask.
--
-- Retention: the FK cascades from apply_run_plan, so purge_apply_run_plan
-- (migration 097) removes these rows with their plan row and needs no rule of
-- its own.

CREATE TABLE apply_run_plan_host_params (
    apply_id   TEXT  NOT NULL,
    plan_index INT   NOT NULL,
    sid        TEXT  NOT NULL,
    params     jsonb NOT NULL,

    PRIMARY KEY (apply_id, plan_index, sid),
    FOREIGN KEY (apply_id, plan_index)
        REFERENCES apply_run_plan (apply_id, plan_index) ON DELETE CASCADE
);

COMMENT ON TABLE apply_run_plan_host_params IS
    'Per-host params of a run task (NIM-985): for a task whose masked params differ between hosts, each host''s value of the keys apply_run_plan.params marks <per-host>. Masked on the write path; cascades from apply_run_plan.';
