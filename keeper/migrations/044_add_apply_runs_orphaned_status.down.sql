-- 044_add_apply_runs_orphaned_status.down.sql
--
-- Откат терминала `orphaned` из enum `apply_runs.status` (Soul-reconcile,
-- ADR-027(g), S6). Перед сужением CHECK-а переводим существующие orphaned-строки
-- в `failed` — ближайший по семантике терминал-фейл дореформенной схемы (RunResult
-- не пришёл, хост не завершил прогон успешно), иначе ADD CONSTRAINT провалился бы
-- на них. Так down не теряет строки и не падает (паттерн 040: предварительный
-- UPDATE перед drop+recreate).
--
-- Возвращает CHECK к форме 040 (planned/claimed/running/dispatched/success/failed/
-- cancelled) и сужает purge_apply_runs обратно (без orphaned).

UPDATE apply_runs SET status = 'failed' WHERE status = 'orphaned';

ALTER TABLE apply_runs
    DROP CONSTRAINT apply_runs_status_valid;

ALTER TABLE apply_runs
    ADD CONSTRAINT apply_runs_status_valid
        CHECK (status IN ('planned', 'claimed', 'running', 'dispatched', 'success', 'failed', 'cancelled'));

CREATE OR REPLACE FUNCTION purge_apply_runs(max_age interval, batch_size integer DEFAULT 1000)
RETURNS BIGINT AS $$
DECLARE
    deleted_count BIGINT;
BEGIN
    WITH expired AS (
        SELECT apply_id, sid FROM apply_runs
        WHERE status IN ('success', 'failed', 'cancelled')
          AND finished_at IS NOT NULL
          AND finished_at < NOW() - max_age
        ORDER BY finished_at
        LIMIT batch_size
    )
    DELETE FROM apply_runs ar
    USING expired e
    WHERE ar.apply_id = e.apply_id AND ar.sid = e.sid;

    GET DIAGNOSTICS deleted_count = ROW_COUNT;
    RETURN deleted_count;
END;
$$ LANGUAGE plpgsql;

COMMENT ON FUNCTION purge_apply_runs(interval, integer) IS
    'Удаляет batch finished apply_runs (success/failed/cancelled) с finished_at старше max_age. running не трогает. Возвращает количество удалённых строк.';
