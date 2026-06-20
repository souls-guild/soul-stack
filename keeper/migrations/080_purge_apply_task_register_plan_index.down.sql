-- 080_purge_apply_task_register_plan_index.down.sql
--
-- Откат forward-фикса 080: возвращаем тело purge_apply_task_register к форме 023
-- (DELETE-join по task_idx). Down 080 выполняется ПЕРЕД down 079 (порядок отката
-- обратный), а 079.down снимает колонку plan_index — поэтому функция обязана
-- перестать ссылаться на plan_index, иначе следующий purge упал бы на
-- несуществующей колонке. Восстановленная форма идентична 023 (N=1: task_idx
-- уникален, поведение корректно).

CREATE OR REPLACE FUNCTION purge_apply_task_register(grace_period interval, batch_size integer DEFAULT 1000)
RETURNS BIGINT AS $$
DECLARE
    deleted_count BIGINT;
BEGIN
    WITH expired AS (
        SELECT atr.apply_id, atr.sid, atr.task_idx
        FROM apply_task_register atr
        JOIN apply_runs ar
          ON ar.apply_id = atr.apply_id AND ar.sid = atr.sid
        WHERE ar.status IN ('success', 'failed', 'cancelled')
          AND ar.finished_at IS NOT NULL
          AND ar.finished_at < NOW() - grace_period
        ORDER BY ar.finished_at
        LIMIT batch_size
    )
    DELETE FROM apply_task_register t
    USING expired e
    WHERE t.apply_id = e.apply_id AND t.sid = e.sid AND t.task_idx = e.task_idx;

    GET DIAGNOSTICS deleted_count = ROW_COUNT;
    RETURN deleted_count;
END;
$$ LANGUAGE plpgsql;

COMMENT ON FUNCTION purge_apply_task_register(interval, integer) IS
    'Удаляет batch apply_task_register-строк прогонов в терминальном статусе (success/failed/cancelled) с finished_at старше grace_period. register активного (running) прогона не трогает. Возвращает количество удалённых строк.';
