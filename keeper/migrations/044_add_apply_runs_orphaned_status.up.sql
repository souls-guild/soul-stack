-- 044_add_apply_runs_orphaned_status.up.sql
--
-- Soul-reconcile (ADR-027(g), S6): вводим терминальный статус `orphaned` в enum
-- `apply_runs.status`. Семантика — строка осталась в `dispatched` после «Keeper и
-- Soul оба мертвы после отдачи», а Soul на reconnect НЕ объявил этот apply_id в
-- WardRoster (in-flight физически нет — например, Soul-процесс перезапущен).
-- RunResult по такой строке не придёт никогда; Keeper терминалит её в `orphaned`
-- (applyrun.OrphanDispatched), барьер классифицирует как фейл (incarnation →
-- error_locked). Без этого статуса строка застряла бы в `dispatched` навсегда:
-- reclaim сужен до `claimed` (S4), Reaper dispatched-timeout сознательно не делаем.
--
-- Расширение CHECK через drop+recreate — паттерн 025/036/040 (status — CHECK-
-- constraint, не PG-enum, значит обратим). Аддитивно: прежние значения сохранены.
--
-- Множество статусов после миграции:
--   planned / claimed / running / dispatched / success / failed / cancelled / orphaned.
--
-- partial-индекс `apply_runs_claim_scan_idx` (025, WHERE status IN
-- ('planned','claimed','running')) НЕ трогаем: orphaned-строки терминальны, им
-- этот индекс не нужен (correlateRunResult/OrphanDispatched ищут по точным
-- ключам). Reaper-правило `purge_apply_runs` расширяем отдельно (см. ниже в этой
-- же миграции) — orphaned тоже finished-терминал, подлежит retention-purge.

ALTER TABLE apply_runs
    DROP CONSTRAINT apply_runs_status_valid;

ALTER TABLE apply_runs
    ADD CONSTRAINT apply_runs_status_valid
        CHECK (status IN ('planned', 'claimed', 'running', 'dispatched', 'success', 'failed', 'cancelled', 'orphaned'));

-- purge_apply_runs (021): добавляем `orphaned` в множество finished-терминалов,
-- подлежащих retention-purge (orphaned несёт finished_at, как success/failed/
-- cancelled). Без этого осиротевшие строки копились бы вечно.
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
