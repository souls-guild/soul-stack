-- 089_add_incarnation_created_scenario.up.sql
--
-- Механизм нескольких create-сценариев (Вариант A): сервис может объявить
-- НЕСКОЛЬКО стартовых сценариев (`scenario/<name>/main.yml` c `create: true`),
-- оператор выбирает один при POST /v1/incarnations (поле `create_scenario`,
-- default `create`). Эта колонка хранит ВЫБОР как runtime-факт инкарнации:
-- каким сценарием она была создана.
--
-- Зачем колонка, а не вывод из state_history: rerun-create (POST .../rerun-create)
-- перезапускает ИМЕННО создавший сценарий. До этой миграции rerun хардкодил
-- `create` (incarnation.UnlockForRerun читал последний state_history.scenario и
-- сверял со строкой "create"). С несколькими create-сценариями это сломалось бы:
-- инкарнация, созданная `create_cluster`, при rerun перезапускала бы `create`.
-- created_scenario — стабильный авторитетный источник «чем создавали» (last
-- state_history может нести метку rerun-create/migration, не имя bootstrap-а).
--
-- NOT NULL DEFAULT 'create' — back-compat: все существующие инкарнации были
-- созданы дефолтным `create`, поэтому backfill значением 'create' корректен и
-- DEFAULT покрывает строки, вставленные старым кодом в переходный период. Имя
-- сценария — snake_case verb (ScenarioNamePattern), TEXT достаточно; отдельного
-- CHECK на формат не вводим (валидация имени — на request-пути keeper-а, БД не
-- дублирует прикладной regex, как и для covens/traits).

ALTER TABLE incarnation
    ADD COLUMN created_scenario TEXT NOT NULL DEFAULT 'create';

COMMENT ON COLUMN incarnation.created_scenario IS
    'Имя стартового сценария, которым создана инкарнация (механизм нескольких create-сценариев, Вариант A). rerun-create перезапускает именно его. DEFAULT create — back-compat.';
