-- 090_incarnation_created_scenario_nullable.down.sql
--
-- Reversible откат: возвращаем created_scenario к NOT NULL DEFAULT 'create'
-- (состояние миграции 089). Перед восстановлением NOT NULL обязан пройти backfill
-- NULL → 'create' — иначе ALTER ... SET NOT NULL упадёт на bare-строках (NULL).
-- Backfill корректен как откат: при возврате к union-семантике 089 дефолтный
-- `create` снова привилегирован, поэтому bare-инкарнации схлопываются в 'create'
-- (rerun-create для них опять станет возможен — это и есть прежнее поведение).

UPDATE incarnation
    SET created_scenario = 'create'
    WHERE created_scenario IS NULL;

ALTER TABLE incarnation
    ALTER COLUMN created_scenario SET DEFAULT 'create',
    ALTER COLUMN created_scenario SET NOT NULL;

COMMENT ON COLUMN incarnation.created_scenario IS
    'Имя стартового сценария, которым создана инкарнация (механизм нескольких create-сценариев, Вариант A). rerun-create перезапускает именно его. DEFAULT create — back-compat.';
