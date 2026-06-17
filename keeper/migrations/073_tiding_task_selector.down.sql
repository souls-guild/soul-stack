-- 073_tiding_task_selector.down.sql
--
-- Откат T4-match: снос additive-колонки task. Возвращает `tidings` к форме 072.

ALTER TABLE tidings
    DROP COLUMN IF EXISTS task;
