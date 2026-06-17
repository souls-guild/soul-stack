-- 040_add_apply_runs_dispatched_status.down.sql
--
-- Откат фазы `dispatched` из enum `apply_runs.status`. Перед сужением CHECK-а
-- переводим существующие dispatched-строки обратно в `running` — иначе
-- ADD CONSTRAINT провалился бы на них. `running` — ближайший по семантике
-- статус «отдано/исполняется» в дореформенной схеме (vestigial, но валидный),
-- так down не теряет строки и не падает (симметрично 036, но с предварительным
-- UPDATE — у `incarnation.status` down-строк не ожидалось, тут страхуемся).
-- Возвращает CHECK к форме 025 (planned/claimed/running/success/failed/cancelled).

UPDATE apply_runs SET status = 'running' WHERE status = 'dispatched';

ALTER TABLE apply_runs
    DROP CONSTRAINT apply_runs_status_valid;

ALTER TABLE apply_runs
    ADD CONSTRAINT apply_runs_status_valid
        CHECK (status IN ('planned', 'claimed', 'running', 'success', 'failed', 'cancelled'));
