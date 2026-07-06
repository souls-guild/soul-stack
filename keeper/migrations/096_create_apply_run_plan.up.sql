-- 096_create_apply_run_plan.up.sql
--
-- Персист host-инвариантного «плана задач прогона» (NIM-37): по одной строке на
-- (apply_id, plan_index) с метаданными отрендеренной задачи — name/module/no_log/
-- passage. Читается read-эндпоинтом GET /v1/incarnations/{name}/runs/{apply_id}/
-- tasks, который джойнит план с per-host результатами из audit_log (task.executed)
-- по plan_index.
--
-- Зачем отдельная таблица, а не re-render YAML сценария: единственный надёжный
-- источник name/module/no_log — рендеренный план на момент dispatch. Re-render
-- отвергнут (loop/CEL-экспансия ломает соответствие plan_index; ref снапшота есть
-- не у всех прогонов). Метаданные host-инвариантны (одинаковы на всех хостах),
-- поэтому храним их РАЗ на прогон по plan_index, а НЕ per-host — per-host статус/
-- output берётся из audit_log (ADR-012: per-task не в apply_runs).
--
-- name/module/no_log — НЕ секрет (это адрес и тип задачи, не значения params),
-- поэтому маскинг не нужен. params задачи (могут нести секреты) в S1a НЕ хранятся —
-- отложены в S1b с secret-маскингом.
--
-- PK (apply_id, plan_index): plan_index — ГЛОБАЛЬНЫЙ сквозной индекс задачи по
-- всему плану (все Passage) = RenderedTask.Index (ADR-056 §S1 Variant B), тот же
-- ключ, по которому task.executed коррелирует задачу в audit_log.
--
-- FK НЕТ: apply_id сам по себе не PK ни в одной таблице (apply_runs ключуется по
-- (apply_id, sid) — много строк на apply_id). Ретеншн — отдельным Reaper-правилом
-- purge_apply_run_plan (миграция 097), т.к. каскадного FK для очистки нет.

CREATE TABLE apply_run_plan (
    apply_id   TEXT        NOT NULL,
    plan_index INT         NOT NULL,
    name       TEXT        NOT NULL,
    module     TEXT        NOT NULL,
    no_log     BOOLEAN     NOT NULL DEFAULT FALSE,
    passage    INT         NOT NULL DEFAULT 0,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),

    PRIMARY KEY (apply_id, plan_index)
);

-- Загрузка всего плана прогона эндпоинтом /tasks (per apply_id → все plan_index).
CREATE INDEX apply_run_plan_apply_idx
    ON apply_run_plan (apply_id);

COMMENT ON TABLE apply_run_plan IS
    'Host-инвариантный план задач прогона (name/module/no_log/passage per plan_index) для read-эндпоинта /tasks (NIM-37). PK (apply_id, plan_index); FK нет — ретеншн через purge_apply_run_plan.';
