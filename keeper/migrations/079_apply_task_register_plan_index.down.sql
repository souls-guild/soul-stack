-- 079_apply_task_register_plan_index.down.sql
--
-- Откат ключевания apply_task_register по plan_index (ADR-056 §S1 fix Variant B):
-- возврат к PK (apply_id, sid, task_idx).
--
-- ПРЕДУСЛОВИЕ корректного отката: ни одного staged-прогона (N>1 Passage) с
-- коллизией task_idx между Passage в живых данных. Если такие строки есть (probe
-- passage0 + действие passage1 делят task_idx), восстановление PK (apply_id, sid,
-- task_idx) упрётся в дубликат — это корректно: откат после раскатанного staged-
-- register невозможен без потери данных (forward-only по сути, как ADR-019). На
-- N=1 (plan_index==task_idx) откат чист.

-- 1. Вернуть PK на (apply_id, sid, task_idx).
ALTER TABLE apply_task_register
    DROP CONSTRAINT apply_task_register_pkey;

ALTER TABLE apply_task_register
    ADD CONSTRAINT apply_task_register_pkey PRIMARY KEY (apply_id, sid, task_idx);

-- 2. Снять колонку plan_index.
ALTER TABLE apply_task_register
    DROP COLUMN plan_index;
