-- 079_apply_task_register_plan_index.up.sql
--
-- Фикс латентного бага staged-register (ADR-056 §S1 fix, Вариант B): ключевание
-- apply_task_register по ГЛОБАЛЬНОМУ сквозному plan_index вместо ЛОКАЛЬНОГО
-- task_idx.
--
-- КОРЕНЬ БАГА (два дефекта). Soul эмитит TaskEvent.task_idx = ЛОКАЛЬНУЮ позицию
-- задачи в ApplyRequest.tasks[] своего Passage (proto RenderedTask глобального
-- индекса раньше не нёс). На staged-render (N>1 Passage) ApplyRequest одного
-- Passage несёт только подмножество задач этого Passage, отфильтрованное ещё и
-- per-host через where: — поэтому task_idx:
--   (1) НЕ уникален между Passage: probe-задача (passage 0, локальная позиция 0)
--       и действие (passage 1, локальная позиция 0) делят task_idx=0. Прежний PK
--       (apply_id, sid, task_idx) → ON CONFLICT затирал probe-register действием.
--   (2) НЕ уникален между хостами одного Passage: разный where: даёт разные
--       подмножества → у одной register-задачи разный локальный idx на host-A и
--       host-B.
-- Дополнительно buildRegisterByHost (scenario/state.go) мапил nameByIdx[t.Index]
-- (ГЛОБАЛЬНЫЙ) против rows.task_idx (ЛОКАЛЬНЫЙ) — рассинхрон имён на passage>0.
--
-- ВАРИАНТ B (architect-дизайн): proto only-add RenderedTask.plan_index +
-- TaskEvent.plan_index — глобальный сквозной индекс задачи по всему плану (по
-- ВСЕМ Passage, = RenderedTask.Index на Keeper-е). Soul эхает полученный
-- plan_index. Register ключуется по нему: глобальный индекс уникален и по всем
-- Passage, и по всем хостам → оба дефекта уходят. Вариант A (passage в PK) был
-- отвергнут: локальный task_idx неоднозначен и ВНУТРИ одного Passage между
-- хостами (разный where:), поэтому (apply_id, sid, passage, task_idx) хрупок.
--
-- АДДИТИВНОСТЬ / backward-compat. plan_index NOT NULL DEFAULT 0:
--   - N=1-прогон (один Passage без register-зависимостей): локальный idx ==
--     глобальный idx, plan_index == task_idx — новый ключ совпадает по
--     селективности со старым на N=1, поведение БИТ-В-БИТ.
--   - Старый Soul без plan_index шлёт 0; но staged (N>1) gated на capability
--     "passage" (S5, Hello.capabilities) → такой Soul под staged-сценарий не
--     допускается ДО dispatch. Не-staged passage=0-прогон коллизий не порождает
--     и сейчас (один Passage, локальный==глобальный).

-- 1. Колонка plan_index: глобальный сквозной индекс задачи по всему плану. NOT
--    NULL DEFAULT 0 — backfill существующих строк нулём (на текущих данных
--    passage везде 0, plan_index==task_idx, PK-селективность не меняется).
ALTER TABLE apply_task_register
    ADD COLUMN plan_index INT NOT NULL DEFAULT 0;

-- 2. Backfill: на существующих (N=1) строках глобальный индекс совпадает с
--    локальным task_idx. Делает старые строки консистентными с новым PK ДО его
--    смены (иначе все легли бы в plan_index=0 → дубликат PK).
UPDATE apply_task_register
    SET plan_index = task_idx;

-- 3. Смена PK: (apply_id, sid, task_idx) → (apply_id, sid, plan_index). Имя
--    PK-constraint в PG по умолчанию apply_task_register_pkey. task_idx
--    остаётся колонкой-данными (локальная позиция в ApplyRequest, информационно
--    для триажа); register-корреляция и upsert-ключ — теперь plan_index.
ALTER TABLE apply_task_register
    DROP CONSTRAINT apply_task_register_pkey;

ALTER TABLE apply_task_register
    ADD CONSTRAINT apply_task_register_pkey PRIMARY KEY (apply_id, sid, plan_index);

COMMENT ON COLUMN apply_task_register.plan_index IS
    'Глобальный сквозной индекс задачи по всему плану прогона (все Passage, ADR-056 §S1 fix Variant B). Ключ register-корреляции (PK-компонент). task_idx остаётся локальной позицией в ApplyRequest своего Passage — на нём корреляция была неуникальна (latent-баг).';

COMMENT ON COLUMN apply_task_register.task_idx IS
    'Локальная позиция задачи в ApplyRequest.tasks[] её Passage (информационно). НЕ ключ корреляции — см. plan_index.';
