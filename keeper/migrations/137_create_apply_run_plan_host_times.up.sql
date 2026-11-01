-- 137_create_apply_run_plan_host_times.up.sql
--
-- When each host started and finished each task of a run (NIM-1170). Until now
-- the run view knew a task's outcome and nothing of its time: the only instant
-- kept was the moment the outcome reached the audit log, so a step that hung for
-- fifteen minutes read exactly like one that took a second.
--
-- The Keeper stamps these rows itself, from its own clock (the database's), at
-- the moments it knows of:
--   * a Keeper task (`on: keeper`): started before the module runs, finished
--     after it returns;
--   * a Soul task: a row per task is written when the host is sent its tasks,
--     the first one started; the TaskEvent of a task finishes its row and starts
--     the host's next unstarted one. A Soul task's start is therefore the
--     Keeper's estimate - it includes the network. An `async:` task runs beside
--     the tasks after it, so the task right after it has no start or a late one.
--
-- A NULL started_at is a task sent and not started yet; a NULL finished_at one
-- that has not finished. Rows cascade from apply_run_plan, so the plan's
-- retention (purge_apply_run_plan) covers them.

CREATE TABLE apply_run_plan_host_times (
    apply_id    TEXT        NOT NULL,
    plan_index  INT         NOT NULL,
    sid         TEXT        NOT NULL,
    started_at  TIMESTAMPTZ,
    finished_at TIMESTAMPTZ,

    PRIMARY KEY (apply_id, plan_index, sid),
    FOREIGN KEY (apply_id, plan_index)
        REFERENCES apply_run_plan (apply_id, plan_index) ON DELETE CASCADE
);

-- A TaskEvent looks up the host's next unstarted task: by host, then plan order.
CREATE INDEX apply_run_plan_host_times_host_idx
    ON apply_run_plan_host_times (apply_id, sid, plan_index);

COMMENT ON TABLE apply_run_plan_host_times IS
    'When each host started and finished each run task, as the Keeper saw it (NIM-1170): a Keeper task is stamped around its own execution; a Soul task starts when the host is sent its tasks (the first) or when the TaskEvent of the task before it arrives, and finishes when its own TaskEvent arrives. NULL started_at = sent, not started; NULL finished_at = not finished. Cascades from apply_run_plan.';
