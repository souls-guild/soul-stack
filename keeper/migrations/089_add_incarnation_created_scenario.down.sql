-- 089_add_incarnation_created_scenario.down.sql
--
-- Reversible откат механизма нескольких create-сценариев (Вариант A): снимаем
-- колонку incarnation.created_scenario. rerun-create-логика возвращается к
-- прежнему хардкоду `create` (state_history-based), что корректно для рантайма
-- без нескольких create-сценариев.

ALTER TABLE incarnation
    DROP COLUMN IF EXISTS created_scenario;
