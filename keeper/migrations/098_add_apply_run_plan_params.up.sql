-- 098_add_apply_run_plan_params.up.sql
--
-- Колонка params (jsonb NULL) для apply_run_plan (NIM-37 S1b): операторские
-- input-параметры отрендеренной задачи, показываются read-эндпоинтом /tasks
-- (GET /v1/incarnations/{name}/runs/{apply_id}/tasks). Params host-инвариантны
-- (одинаковы на всех хостах прогона), поэтому хранятся РАЗ на (apply_id,
-- plan_index) — как name/module/no_log/passage (миграция 096).
--
-- Значения params могут нести секреты (после vault-resolve+CEL), поэтому
-- маскируются seal-aware механизмом (audit.MaskSecretsSealed — тот же, что
-- status_details/error_summary: sealed-пути прогона + vault + regex-last-resort)
-- на write-path-е ДО записи, второй барьер поверх того, что params отрендерены.
-- no_log-задача → NULL (params не хранятся, симметрия с подавлением register_data).
-- Транспортные ключи core.file.rendered (template_content/render_context) в params
-- НЕ сохраняются — это не operator-facing «входные данные» задачи.
--
-- Ретеншн не меняется: та же строка плана, что чистит purge_apply_run_plan
-- (миграция 097) — отдельной очистки для params не нужно.

ALTER TABLE apply_run_plan
    ADD COLUMN params jsonb;

COMMENT ON COLUMN apply_run_plan.params IS
    'Masked операторские input-параметры задачи (NIM-37 S1b): seal-aware маскинг на write-path (audit.MaskSecretsSealed); NULL для no_log-задач и задач без params; template_content/render_context отфильтрованы. jsonb object.';
