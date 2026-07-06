-- 097_create_purge_apply_run_plan.up.sql
--
-- Reaper-правило purge_apply_run_plan (docs/keeper/reaper.md): удаляет batch
-- строк плана задач прогона (apply_run_plan, миграция 096), чей прогон завершён и
-- достаточно стар. Зеркало purge_apply_task_register (023), но по apply_id, а не
-- (apply_id, sid): план host-инвариантен, sid у него нет.
--
-- Зачем отдельное правило (в отличие от apply_task_register, где FK ON DELETE
-- CASCADE чистит каскадом): у apply_run_plan FK НЕТ (apply_id не PK ни в одной
-- таблице), поэтому строки плана НЕ уносятся автоматически при purge_apply_runs —
-- без этого правила они росли бы неограниченно (orphan после сноса apply_runs).
--
-- Критерий удаления:
--   - created_at < NOW() - grace_period — план старше окна ретеншна. Возраст от
--     created_at (пишется РАЗ при dispatch, не сдвигается — retry-upsert per-host
--     нет, в отличие от register). Grace выровнен на ретеншн apply-истории (30d):
--     план нужен эндпоинту /tasks столько же, сколько живут apply_runs прогона.
--     Floor по created_at также закрывает гонку «план записан на render, а
--     apply_runs строки этого прогона ещё не вставлены dispatch-ем» — свежий план
--     под grace не трогается.
--   - NOT EXISTS нетерминального apply_run прогона — план АКТИВНОГО (running/
--     planned/…) прогона не удаляется НИКОГДА, независимо от возраста. Orphan
--     (apply_runs уже снесены purge_apply_runs) нетерминальных строк не имеет →
--     под floor по created_at удаляется.
--
-- CTE с LIMIT batch_size ограничивает размер транзакции, как в остальных
-- Reaper-правилах.

CREATE OR REPLACE FUNCTION purge_apply_run_plan(grace_period interval, batch_size integer DEFAULT 1000)
RETURNS BIGINT AS $$
DECLARE
    deleted_count BIGINT;
BEGIN
    WITH expired AS (
        SELECT p.apply_id, p.plan_index
        FROM apply_run_plan p
        WHERE p.created_at < NOW() - grace_period
          AND NOT EXISTS (
              SELECT 1
              FROM apply_runs ar
              WHERE ar.apply_id = p.apply_id
                AND ar.status NOT IN ('success', 'failed', 'cancelled')
          )
        ORDER BY p.created_at
        LIMIT batch_size
    )
    DELETE FROM apply_run_plan t
    USING expired e
    WHERE t.apply_id = e.apply_id AND t.plan_index = e.plan_index;

    GET DIAGNOSTICS deleted_count = ROW_COUNT;
    RETURN deleted_count;
END;
$$ LANGUAGE plpgsql;

COMMENT ON FUNCTION purge_apply_run_plan(interval, integer) IS
    'Удаляет batch apply_run_plan-строк прогонов старше grace_period (по created_at) БЕЗ нетерминальных apply_runs. План активного прогона не трогает. Возвращает количество удалённых строк.';
