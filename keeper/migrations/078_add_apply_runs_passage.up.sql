-- 078_add_apply_runs_passage.up.sql
--
-- Staged-render (Passage, ADR-056, S1): прогон сценария исполняется как N≥1
-- упорядоченных Passage (render → dispatch → barrier → сбор register). На один
-- хост прогона теперь приходится N заданий — по одному на Passage. Прежний
-- composite PK `apply_runs (apply_id, sid)` (миграция 018) больше не уникален
-- per-host.
--
-- ВЫБОР ПОДХОДА (ADR-056 оставил I vs II на S1): Variant I — расширить PK до
-- `(apply_id, sid, passage)`, N строк на хост в той же таблице. Обоснование
-- «меньший регресс-риск»:
--   - Барьер (waitBarrier/classify), Ward-claim (ClaimNext/MarkDispatched),
--     recovery-reclaim (ReclaimApplyRuns), Soul-reconcile (OrphanDispatched) и
--     RunResult-correlation ВСЕ работают с per-host(-per-passage) строками
--     ИМЕННО этой таблицы. Variant II (отдельная apply_run_passages) потребовал
--     бы переуказать все эти чтения/записи на новую таблицу + изобрести
--     write-path агрегата apply_runs — шире зона касания, не уже.
--   - passage DEFAULT 0: на S1 НИКТО не пишет passage>0 (стратификация — S2/S3),
--     поэтому каждый существующий запрос `WHERE apply_id=$1 AND sid=$2` хитит
--     ровно одну строку (passage 0) — поведение БИТ-В-БИТ как сейчас.
--   - Единственная FK, ссылавшаяся на apply_runs(apply_id, sid) — это
--     apply_task_register (миграция 022). Переуказываем её на тройку
--     (apply_id, sid, passage). NB: на ЭТОЙ миграции PK apply_task_register ещё
--     по task_idx; миграция 079 (ADR-056 §S1 fix Variant B) затем перетягивает
--     register-ключ на ГЛОБАЛЬНЫЙ plan_index — task_idx ЛОКАЛЕН для Passage/host
--     (неуникален между Passage и между хостами под per-host where:), сквозным
--     по Passage он НЕ является (исходное допущение здесь оказалось неверным).
--
-- АДДИТИВНОСТЬ: passage NOT NULL DEFAULT 0 — существующие строки получают 0,
-- новый PK (apply_id, sid, 0) совпадает по селективности со старым на текущих
-- данных. Backward-compat (passage везде 0 = как до staged-render) — инвариант
-- S1.

-- 1. Колонка passage на apply_runs. NOT NULL DEFAULT 0: backfill существующих
--    строк нулём, новый PK-компонент детерминирован для текущего write-path-а
--    (Insert/InsertPlanned не пишут passage явно → 0).
ALTER TABLE apply_runs
    ADD COLUMN passage INT NOT NULL DEFAULT 0;

-- 2. apply_task_register: симметрично несёт passage (NOT NULL DEFAULT 0).
--    Нужно ДО переуказания FK — referencing-колонки должны существовать.
--    PK apply_task_register (apply_id, sid, task_idx) на этой миграции НЕ
--    меняется; passage — данные строки + компонент FK-цели. NB: task_idx
--    ЛОКАЛЕН для Passage/host (не сквозной) — миграция 079 перетянет register-PK
--    на глобальный plan_index (ADR-056 §S1 fix Variant B).
ALTER TABLE apply_task_register
    ADD COLUMN passage INT NOT NULL DEFAULT 0;

-- 3. Переуказание FK apply_task_register → apply_runs на тройку. Старая FK
--    ссылалась на (apply_id, sid); после смены PK apply_runs этот префикс уже
--    не уникален — FK обязана включить passage. ON DELETE CASCADE сохраняется
--    (register-данные умирают вместе со строкой Passage; каскад от incarnation
--    через apply_runs остаётся в силе).
ALTER TABLE apply_task_register
    DROP CONSTRAINT apply_task_register_apply_run_fk;

-- 4. Смена PK apply_runs: (apply_id, sid) → (apply_id, sid, passage). Имя
--    PK-constraint в PG по умолчанию apply_runs_pkey.
ALTER TABLE apply_runs
    DROP CONSTRAINT apply_runs_pkey;

ALTER TABLE apply_runs
    ADD CONSTRAINT apply_runs_pkey PRIMARY KEY (apply_id, sid, passage);

-- 5. Восстановление FK apply_task_register на новый тройной ключ.
ALTER TABLE apply_task_register
    ADD CONSTRAINT apply_task_register_apply_run_fk
        FOREIGN KEY (apply_id, sid, passage) REFERENCES apply_runs (apply_id, sid, passage) ON DELETE CASCADE;

COMMENT ON COLUMN apply_runs.passage IS
    'Индекс Passage (0-based) staged-render (ADR-056). 0 = единственный Passage (поведение как до staged-render). Часть PK (apply_id, sid, passage).';

COMMENT ON COLUMN apply_task_register.passage IS
    'Passage задачи (ADR-056), компонент FK на apply_runs(apply_id, sid, passage). task_idx ЛОКАЛЕН для Passage/host (не сквозной) — register-ключ перетянут на глобальный plan_index миграцией 079.';
