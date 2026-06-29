-- 090_incarnation_created_scenario_nullable.up.sql
--
-- Bare-инкарнация через NULL (Фаза 2 create-вариантов). Миграция 089 ввела
-- created_scenario как TEXT NOT NULL DEFAULT 'create' (back-compat union, когда
-- дефолтный `create` был привилегирован и всегда годен). Фаза 2 убрала
-- хардкод-union: набор стартовых сценариев = РОВНО {scenario с `create: true`},
-- имя `create` больше не привилегировано. Появился новый класс инкарнаций —
-- BARE: сервис без единого create-сценария создаётся StatusReady БЕЗ прогона
-- (готов к day-2). Для bare колонка должна нести NULL (нет bootstrap-сценария),
-- а не выдуманное 'create'.
--
-- DROP NOT NULL + DROP DEFAULT: NULL = bare (нет создавшего сценария), непустое =
-- имя bootstrap-сценария. rerun-create по NULL отказывает (incarnation.UnlockForRerun
-- → ErrRerunScenarioNotCreate → 409): перезапускать нечего.
--
-- Existing-инкарнации не трогаем: строки со значением 'create' (вставленные до
-- этой миграции дефолтом 089) остаются корректными — у redis-сервиса scenario/
-- create/main.yml теперь несёт `create: true` (Фаза 1), поэтому 'create' для них
-- по-прежнему валидный bootstrap, а не legacy-артефакт. Backfill не нужен.

ALTER TABLE incarnation
    ALTER COLUMN created_scenario DROP NOT NULL,
    ALTER COLUMN created_scenario DROP DEFAULT;

COMMENT ON COLUMN incarnation.created_scenario IS
    'Имя стартового сценария, которым создана инкарнация (механизм нескольких create-сценариев, Вариант A). NULL = bare-инкарнация (создана без bootstrap-сценария, StatusReady без прогона). rerun-create перезапускает именно его (по NULL — отказ 409).';
