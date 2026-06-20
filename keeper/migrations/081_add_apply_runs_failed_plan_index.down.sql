-- 081_add_apply_runs_failed_plan_index.down.sql
--
-- Откат failure-канального фикса (ADR-056 §S1 fix Variant B): снимаем колонку
-- failed_plan_index. На N=1 (failed_plan_index == task_idx) откат чист — данные
-- упавшей задачи остаются в task_idx, корреляция возвращается к нему (как до
-- фикса; для N=1 task_idx == глобальный индекс, поведение корректно). На
-- раскатанном staged-render (N>1) после отката failure-корреляция снова стала
-- бы локальной (mislabel) — forward-only по сути, как 079/080.
ALTER TABLE apply_runs
    DROP COLUMN IF EXISTS failed_plan_index;
