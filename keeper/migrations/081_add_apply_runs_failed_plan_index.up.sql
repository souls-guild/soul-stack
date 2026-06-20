-- 081_add_apply_runs_failed_plan_index.up.sql
--
-- Фикс failure-канала под staged-render (ADR-056 §S1 fix Variant B, последняя
-- инстанция класса global-vs-local-task_idx): apply_runs несёт ГЛОБАЛЬНЫЙ
-- сквозной plan_index упавшей задачи, а не только её ЛОКАЛЬНУЮ позицию в
-- ApplyRequest.tasks[] (колонка task_idx).
--
-- КОРЕНЬ БАГА. recordTaskFailure (grpc/events_taskevent.go) писал в
-- apply_runs.task_idx ЛОКАЛЬНЫЙ TaskEvent.task_idx (позиция задачи в
-- ApplyRequest.tasks[] её Passage). buildHostReport (scenario/checkdrift.go,
-- failure-ветка) и failureReason (scenario/dispatch.go, no_log-подавление)
-- резолвили имя/мета упавшей задачи через taskMeta[task_idx] / noLogByIndex
-- [task_idx], где taskMeta/noLogByIndex построены по RenderedTask.Index
-- (ГЛОБАЛЬНЫЙ индекс по всему плану). На staged-render (N>1 Passage) или при
-- per-host where: локальный task_idx ≠ глобальный Index → module/action упавшей
-- задачи промаркированы соседней задачей (mislabel). Тот же дефект, что был
-- закрыт в register-канале миграцией 079; failure-канал — его последняя
-- инстанция.
--
-- ВАРИАНТ B (симметрия 079): Soul уже эхает TaskEvent.plan_index (глобальный
-- сквозной индекс, = RenderedTask.Index) во ВСЕХ ветках. apply_runs получает
-- отдельную nullable-колонку failed_plan_index под этот глобальный индекс;
-- task_idx остаётся информационно-локальным (триаж), как
-- apply_task_register.task_idx после 079. Корреляторы (drift-report failure +
-- barrier no_log) переключаются на failed_plan_index.
--
-- АДДИТИВНОСТЬ / backward-compat. failed_plan_index nullable (как task_idx):
--   - N=1-прогон (один Passage, без per-host фильтрации) — локальный idx ==
--     глобальный idx, failed_plan_index == task_idx упавшей задачи. Корреляция
--     по новому полю совпадает по результату со старой (task_idx). BACKFILL на
--     существующих строках: failed_plan_index := task_idx (где task_idx
--     заполнен) — для N=1 это ТОЧНОЕ значение глобального индекса.
--   - Старый Soul без plan_index в TaskEvent (plan_index=0): не-staged
--     passage=0-прогон коллизий не порождает (локальный==глобальный), staged
--     (N>1) gated на capability "passage" (S5) — такой Soul под staged-сценарий
--     не допускается до dispatch.

-- 1. Колонка failed_plan_index: глобальный сквозной индекс упавшей задачи по
--    всему плану прогона (все Passage). NULL до первой упавшей задачи (как
--    task_idx); заполняется recordTaskFailure first-failure-wins (COALESCE).
ALTER TABLE apply_runs
    ADD COLUMN failed_plan_index INT;

-- 2. Backfill: на существующих строках с уже записанной упавшей задачей
--    глобальный индекс совпадает с локальным task_idx (passage везде 0 на
--    текущих данных, локальный==глобальный). Делает старые строки
--    консистентными с новой корреляцией.
UPDATE apply_runs
    SET failed_plan_index = task_idx
    WHERE task_idx IS NOT NULL;

COMMENT ON COLUMN apply_runs.failed_plan_index IS
    'Глобальный сквозной plan_index первой упавшей задачи хоста по всему плану прогона (все Passage, ADR-056 §S1 fix Variant B). Ключ корреляции с RenderedTask.Index для module/action упавшей задачи (drift-report) и no_log-подавления (barrier). task_idx остаётся локальной позицией в ApplyRequest своего Passage (информационно) — на нём корреляция была неуникальна под staged/per-host-where (latent-баг).';

COMMENT ON COLUMN apply_runs.task_idx IS
    'Локальная позиция первой упавшей задачи в ApplyRequest.tasks[] её Passage (информационно, эхо TaskEvent.task_idx). НЕ ключ корреляции с глобальным планом — см. failed_plan_index.';
