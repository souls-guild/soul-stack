-- 080_purge_apply_task_register_plan_index.up.sql
--
-- Forward-фикс Reaper-правила `purge_apply_task_register` (023) под staged-render
-- (ADR-056 §S1 fix Variant B, миграции 078/079): DELETE-join переключается с
-- `task_idx` на стабильно-уникальный `plan_index`.
--
-- ПРОБЛЕМА (batch-overshoot): миграция 023 удаляла register-строки по join
-- `(apply_id, sid, task_idx)`. После 079 task_idx БОЛЬШЕ НЕ уникален в
-- (apply_id, sid) под staged-render (N>1 Passage): probe passage0 и действие
-- passage1 делят локальный idx=0. CTE `expired` отбирает batch_size строк, но
-- финальный DELETE-join по неуникальному task_idx сносит ВСЕ строки, делящие
-- этот task_idx в рамках хоста — один selected row удаляет N физических строк.
-- `LIMIT batch_size` на CTE при этом перестаёт точно ограничивать размер
-- транзакции (overshoot до N×batch_size). N=1 (старые данные, passage везде 0,
-- plan_index==task_idx) баг не проявляли.
--
-- ФИКС: join по `(apply_id, sid, plan_index)` — стабильно-уникальный ключ
-- register-строки (PK apply_task_register после 079). Один selected row удаляет
-- ровно одну строку, batch_size снова точен. CTE-проекция несёт plan_index
-- вместо task_idx.
--
-- ПОЧЕМУ НОВАЯ МИГРАЦИЯ, А НЕ ПРАВКА 023 IN-PLACE: 023 уже применена на
-- существующих базах (beta.1). golang-migrate не реаплаит уже-применённые
-- миграции (хранит только version, не checksum тела) → правка тела 023 не дошла
-- бы до развёрнутых баз. `CREATE OR REPLACE FUNCTION` отдельной forward-миграцией
-- переисполняется везде и заменяет тело функции. Зависит от plan_index (079) —
-- порядок 079 < 080 корректен.

CREATE OR REPLACE FUNCTION purge_apply_task_register(grace_period interval, batch_size integer DEFAULT 1000)
RETURNS BIGINT AS $$
DECLARE
    deleted_count BIGINT;
BEGIN
    WITH expired AS (
        SELECT atr.apply_id, atr.sid, atr.plan_index
        FROM apply_task_register atr
        JOIN apply_runs ar
          ON ar.apply_id = atr.apply_id AND ar.sid = atr.sid AND ar.passage = atr.passage
        WHERE ar.status IN ('success', 'failed', 'cancelled')
          AND ar.finished_at IS NOT NULL
          AND ar.finished_at < NOW() - grace_period
        ORDER BY ar.finished_at
        LIMIT batch_size
    )
    DELETE FROM apply_task_register t
    USING expired e
    WHERE t.apply_id = e.apply_id AND t.sid = e.sid AND t.plan_index = e.plan_index;

    GET DIAGNOSTICS deleted_count = ROW_COUNT;
    RETURN deleted_count;
END;
$$ LANGUAGE plpgsql;

COMMENT ON FUNCTION purge_apply_task_register(interval, integer) IS
    'Удаляет batch apply_task_register-строк прогонов в терминальном статусе (success/failed/cancelled) с finished_at старше grace_period. Ключ удаления — (apply_id, sid, plan_index) (стабильно-уникальный после 079, ADR-056). register активного (running) прогона не трогает. Возвращает количество удалённых строк.';
