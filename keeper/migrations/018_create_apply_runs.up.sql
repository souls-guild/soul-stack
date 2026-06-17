-- 018_create_apply_runs.up.sql
--
-- Реестр apply-прогонов (correlation `apply_id` ↔ incarnation/scenario) под
-- M2.x scenario-runner. Каждая строка — один Soul-хост в рамках одного
-- прогона; composite PK `(apply_id, sid)` (apply_id-model A: один apply_id
-- на scenario, разный sid на каждый хост fan-out-а).
--
-- Назначение: при получении `RunResult` от Soul-а Keeper не знает из proto,
-- к какой incarnation относится прогон (RunResult несёт только
-- apply_id/status/state_changes). Эта таблица закрывает correlation:
-- scenario-runner пишет строку при dispatch-е `ApplyRequest`, RunResult-
-- handler читает её по `(apply_id, sid)` и коммитит state в нужную
-- incarnation.
--
-- task_idx — nullable (PM-decision 2): on dispatch неизвестен; заполняется
-- при per-task прогрессе (пост-MVP) либо остаётся NULL для агрегированного
-- RunResult.
--
-- FK:
--   - incarnation_name → incarnation(name) ON DELETE CASCADE (прогоны
--     умирают вместе с incarnation, симметрично state_history).
--   - started_by_aid   → operators(aid)   ON DELETE SET NULL (история
--     прогона переживает удаление оператора; PM-decision 3).
--
-- status — closed CHECK (PM-decision 1): running/success/failed/cancelled.
-- Retention (Reaper-правило `purge_apply_runs`) — backlog (PM-decision 4).

CREATE TABLE apply_runs (
    apply_id          TEXT        NOT NULL,
    sid               TEXT        NOT NULL,
    incarnation_name  TEXT        NOT NULL,
    scenario          TEXT        NOT NULL,
    task_idx          INT,
    status            TEXT        NOT NULL,
    error_summary     TEXT,
    started_at        TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    finished_at       TIMESTAMPTZ,
    started_by_aid    TEXT,

    PRIMARY KEY (apply_id, sid),

    CONSTRAINT apply_runs_status_valid
        CHECK (status IN ('running', 'success', 'failed', 'cancelled')),
    CONSTRAINT apply_runs_incarnation_fk
        FOREIGN KEY (incarnation_name) REFERENCES incarnation (name) ON DELETE CASCADE,
    CONSTRAINT apply_runs_started_by_aid_fk
        FOREIGN KEY (started_by_aid) REFERENCES operators (aid) ON DELETE SET NULL
);

-- Лента прогонов конкретной incarnation (триаж, history-эндпоинт).
CREATE INDEX apply_runs_incarnation_idx
    ON apply_runs (incarnation_name);

-- Резолв по apply_id всех хостов прогона (scenario-runner fan-in,
-- RunResult-correlation).
CREATE INDEX apply_runs_apply_idx
    ON apply_runs (apply_id);

-- Partial-индекс для «висящих» прогонов: запрос Reaper-а / триажа «всё, что
-- ещё running» (терминальные статусы из индекса исключены).
CREATE INDEX apply_runs_status_idx
    ON apply_runs (status) WHERE status = 'running';

COMMENT ON TABLE apply_runs IS
    'Correlation apply_id ↔ incarnation/scenario для scenario-runner (M2.x). PK (apply_id, sid).';
