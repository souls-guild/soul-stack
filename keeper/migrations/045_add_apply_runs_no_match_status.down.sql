-- 045_add_apply_runs_no_match_status.down.sql
--
-- Откат терминала `no_match` из enum `apply_runs.status` (FINDING-01 вариант (б)).
-- Перед сужением CHECK-а переводим существующие no_match-строки в `success` — это
-- ровно дореформенное поведение нецелевого хоста (до FINDING-01 Acolyte закрывал
-- такой no-op-хост как success), иначе ADD CONSTRAINT провалился бы на них. Так
-- down не теряет строки и не падает (паттерн 040/044: предварительный UPDATE
-- перед drop+recreate).
--
-- Возвращает CHECK к форме 044 (planned/claimed/running/dispatched/success/failed/
-- cancelled/orphaned) и сужает purge_apply_runs обратно (без no_match).

UPDATE apply_runs SET status = 'success' WHERE status = 'no_match';

ALTER TABLE apply_runs
    DROP CONSTRAINT apply_runs_status_valid;

ALTER TABLE apply_runs
    ADD CONSTRAINT apply_runs_status_valid
        CHECK (status IN ('planned', 'claimed', 'running', 'dispatched', 'success', 'failed', 'cancelled', 'orphaned'));

CREATE OR REPLACE FUNCTION purge_apply_runs(max_age interval, batch_size integer DEFAULT 1000)
RETURNS BIGINT AS $$
DECLARE
    deleted_count BIGINT;
BEGIN
    WITH expired AS (
        SELECT apply_id, sid FROM apply_runs
        WHERE status IN ('success', 'failed', 'cancelled', 'orphaned')
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
    'Удаляет batch finished apply_runs (success/failed/cancelled/orphaned) с finished_at старше max_age. running/dispatched не трогает. Возвращает количество удалённых строк.';
