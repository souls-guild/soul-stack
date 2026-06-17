-- 021_create_purge_apply_runs.up.sql
--
-- Reaper-правило `purge_apply_runs` (docs/keeper/reaper.md): удаляет batch
-- завершённых apply-прогонов из реестра `apply_runs` (миграция 018) старше
-- `max_age`. Retention apply-истории (default 30d, считается от
-- `finished_at`).
--
-- Удаляются ТОЛЬКО finished-записи (`success` / `failed` / `cancelled` с
-- `finished_at IS NOT NULL`). Записи `running` НИКОГДА не purge — это
-- висящие/идущие прогоны; их триаж и завершение — отдельный механизм
-- scenario-runner-а, не Жнеца.
--
-- Возраст считается от `finished_at`: история прогона измеряется временем,
-- прошедшим с момента завершения, а не старта (долгий прогон, завершившийся
-- недавно, не должен удаляться раньше короткого, завершившегося давно).
--
-- composite PK `(apply_id, sid)` (018) → DELETE ... USING expired по обоим
-- ключам; CTE с LIMIT batch_size ограничивает размер транзакции, как и в
-- остальных Reaper-правилах.

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
