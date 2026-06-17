-- 045_add_apply_runs_no_match_status.up.sql
--
-- FINDING-01 вариант (б): вводим терминальный статус `no_match` в enum
-- `apply_runs.status`. Семантика — хост нецелевой для прогона: Acolyte-путь
-- (acolytes>0) пишет planned-задание на КАЖДЫЙ roster-хост incarnation ДО
-- per-host резолва `on:`/`where:` (резолв позже, при claim). Хост, у которого
-- после `on:`/`where:` осталось 0 задач, Acolyte закрывает в `no_match` (НЕ
-- `success`) — apply_runs больше не over-reports «успех там, где ничего не
-- применялось». Барьер классифицирует `no_match` как ТЕРМИНАЛ и НЕ-провал
-- (benign, как success): прогон, где целевые success + не-целевые no_match,
-- ведёт incarnation в `ready` (НЕ error_locked).
--
-- Расширение CHECK через drop+recreate — паттерн 025/036/040/044 (status —
-- CHECK-constraint, не PG-enum, значит обратим). Аддитивно: прежние значения
-- сохранены, идёт ПОВЕРХ 044 (orphaned), не ломая его.
--
-- Множество статусов после миграции:
--   planned / claimed / running / dispatched / success / failed / cancelled /
--   orphaned / no_match.
--
-- partial-индекс `apply_runs_claim_scan_idx` (025, WHERE status IN
-- ('planned','claimed','running')) НЕ трогаем: no_match-строки терминальны, им
-- этот индекс не нужен. Reaper-правило `purge_apply_runs` расширяем ниже —
-- no_match тоже finished-терминал (несёт finished_at), подлежит retention-purge,
-- иначе строки нецелевых хостов копились бы вечно.

ALTER TABLE apply_runs
    DROP CONSTRAINT apply_runs_status_valid;

ALTER TABLE apply_runs
    ADD CONSTRAINT apply_runs_status_valid
        CHECK (status IN ('planned', 'claimed', 'running', 'dispatched', 'success', 'failed', 'cancelled', 'orphaned', 'no_match'));

-- purge_apply_runs (021/044): добавляем `no_match` в множество finished-терминалов,
-- подлежащих retention-purge (no_match несёт finished_at, как success/failed/
-- cancelled/orphaned). Без этого нецелевые-хост-строки копились бы вечно.
CREATE OR REPLACE FUNCTION purge_apply_runs(max_age interval, batch_size integer DEFAULT 1000)
RETURNS BIGINT AS $$
DECLARE
    deleted_count BIGINT;
BEGIN
    WITH expired AS (
        SELECT apply_id, sid FROM apply_runs
        WHERE status IN ('success', 'failed', 'cancelled', 'orphaned', 'no_match')
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
    'Удаляет batch finished apply_runs (success/failed/cancelled/orphaned/no_match) с finished_at старше max_age. running/dispatched не трогает. Возвращает количество удалённых строк.';
