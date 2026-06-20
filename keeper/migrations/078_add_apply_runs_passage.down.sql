-- 078_add_apply_runs_passage.down.sql
--
-- Откат staged-render Passage-схемы (ADR-056, S1): возврат к composite PK
-- `apply_runs (apply_id, sid)` и FK apply_task_register на пару.
--
-- ПРЕДУСЛОВИЕ корректного отката: passage везде 0 (N=1, ни одного multi-passage
-- прогона). Если в таблице есть строки с passage>0 (стратификация S2/S3 уже
-- писала их), DROP старого/восстановление PK по (apply_id, sid) упрётся в
-- дубликаты — это корректно: откат после раскатанного staged-render невозможен
-- без потери данных, миграция forward-only по сути (как ADR-019). На S1 (passage
-- везде 0) откат чист.

-- 1. Снять тройную FK перед восстановлением парной PK.
ALTER TABLE apply_task_register
    DROP CONSTRAINT apply_task_register_apply_run_fk;

-- 2. Вернуть PK apply_runs на (apply_id, sid).
ALTER TABLE apply_runs
    DROP CONSTRAINT apply_runs_pkey;

ALTER TABLE apply_runs
    ADD CONSTRAINT apply_runs_pkey PRIMARY KEY (apply_id, sid);

-- 3. Восстановить FK apply_task_register на пару (apply_id, sid).
ALTER TABLE apply_task_register
    ADD CONSTRAINT apply_task_register_apply_run_fk
        FOREIGN KEY (apply_id, sid) REFERENCES apply_runs (apply_id, sid) ON DELETE CASCADE;

-- 4. Снять колонки passage.
ALTER TABLE apply_task_register
    DROP COLUMN passage;

ALTER TABLE apply_runs
    DROP COLUMN passage;
