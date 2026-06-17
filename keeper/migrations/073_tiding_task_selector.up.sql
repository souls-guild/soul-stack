-- 073_tiding_task_selector.up.sql
--
-- ADR-052 §l (task-селектор Tiding — эпик «таска X»), слайс T4-match.
--
-- Расширяет реестр `tidings` одной additive-колонкой (ни одно существующее поле/
-- контракт не меняет семантику):
--   - task — опц. селектор подписки на КОНКРЕТНУЮ задачу прогона по её адресу
--     (register ∪ id из changed_tasks события incarnation.run_completed, ADR-052 §j).
--     NULL = без фильтра (текущее поведение). Непустое значение → правило матчит
--     incarnation.run_completed, только если в его changed_tasks есть запись с
--     register == task ИЛИ id == task (dispatcher.matchTask). «Присутствие в
--     changed_tasks» = задача изменилась — селектор самодостаточен (см. ADR-052 §l).
--
-- Без CHECK на формат: адрес — register/id одного пространства подписки
-- (snake-case), но это значение СЕЛЕКТОРА (что матчить), не грамматики задачи;
-- сопоставление с фактическими changed_tasks делает dispatcher, БД-CHECK по форме
-- был бы хрупким (симметрично incarnation/cadence-селекторам без CHECK).

ALTER TABLE tidings
    ADD COLUMN task TEXT;

COMMENT ON COLUMN tidings.task IS
    'Опц. селектор подписки на конкретную задачу прогона по адресу register∪id из changed_tasks (ADR-052 §l). NULL = без фильтра.';
