-- 076_create_purge_push_runs.up.sql
--
-- Reaper-правило `purge_push_runs` (docs/keeper/reaper.md): удаляет batch
-- завершённых push-прогонов из реестра `push_runs` (миграция 051) старше
-- `max_age`. Retention растущей истории push-прогонов (default 30d, считается
-- от `finished_at`). Зеркало `purge_apply_runs` (021) / `purge_voyages` (075)
-- для push-стороны.
--
-- ПОЧЕМУ правило нужно: `push_runs` — run-history таблица (одна строка =
-- один `POST /v1/push/apply`), растёт без потолка, и до сих пор НЕ имела
-- retention завершённых записей. Существующее правило `purge_orphan_push_runs`
-- (Go-side, push_orphan.go) — про ДРУГОЕ: терминализация in-flight зомби
-- (pending/running старше 1h → cancelled), оно НЕ удаляет завершённые строки.
-- Это правило закрывает оставшийся хвост — DELETE финишированных.
--
-- Удаляются ТОЛЬКО finished-записи (`status` ∈ `success` / `partial_failed` /
-- `failed` / `cancelled` и `finished_at IS NOT NULL`). Записи `pending` /
-- `running` НИКОГДА не purge — это идущие/висящие прогоны; их триаж и
-- терминализация — правило `purge_orphan_push_runs`, не это. Терминальный
-- `cancelled` ставится тем же orphan-правилом — такие записи это правило
-- подбирает по истечении своего окна (parity: orphan терминалит за 1h,
-- финальный DELETE — за 30d, как у apply_runs).
--
-- Возраст считается от `finished_at`: история измеряется временем с момента
-- завершения, а не старта (долгий прогон, завершившийся недавно, не должен
-- удаляться раньше короткого, завершившегося давно). Parity purge_apply_runs.
--
-- КАСКАДЫ / ЗАВИСИМОСТИ: дочерних таблиц с FK на `push_runs` НЕТ — per-host
-- результаты хранятся inline в `push_runs.summary` (jsonb), а НЕ в отдельной
-- таблице (push синхронный oneshot, не идёт через apply_runs-барьер; см.
-- 051). FK `push_runs.started_by_aid → operators` направлен ОТ push_run к
-- operators (ON DELETE SET NULL) — DELETE push_run его не задевает. Битых
-- ссылок DELETE не создаёт.
--
-- PK `apply_id` (051) → DELETE по single-column WHERE apply_id IN (...);
-- CTE с LIMIT batch_size ограничивает размер транзакции, как в остальных
-- Reaper-правилах. Отдельный индекс по finished_at не заводим до появления
-- реального объёма (history-purge — холодный фон, не hot-path; parity
-- purge_apply_runs / purge_voyages, где выделенного finished_at-индекса нет).

CREATE OR REPLACE FUNCTION purge_push_runs(max_age interval, batch_size integer DEFAULT 1000)
RETURNS BIGINT AS $$
DECLARE
    deleted_count BIGINT;
BEGIN
    WITH expired AS (
        SELECT apply_id FROM push_runs
        WHERE status IN ('success', 'partial_failed', 'failed', 'cancelled')
          AND finished_at IS NOT NULL
          AND finished_at < NOW() - max_age
        ORDER BY finished_at
        LIMIT batch_size
    )
    DELETE FROM push_runs pr
    USING expired e
    WHERE pr.apply_id = e.apply_id;

    GET DIAGNOSTICS deleted_count = ROW_COUNT;
    RETURN deleted_count;
END;
$$ LANGUAGE plpgsql;

COMMENT ON FUNCTION purge_push_runs(interval, integer) IS
    'Удаляет batch finished push_runs (success/partial_failed/failed/cancelled) с finished_at старше max_age. pending/running не трогает (это правило purge_orphan_push_runs). Дочерних FK-таблиц нет (per-host результаты inline в summary). Возвращает количество удалённых строк (parity purge_apply_runs).';
