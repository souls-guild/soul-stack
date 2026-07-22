-- 073_tiding_task_selector.down.sql
--
-- Revert of T4-match: drop the additive task column. Returns `tidings` to the 072 form.

ALTER TABLE tidings
    DROP COLUMN IF EXISTS task;
