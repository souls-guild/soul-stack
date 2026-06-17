-- 014_create_mark_disconnected.up.sql
--
-- Reaper-правило `mark_disconnected` (docs/keeper/reaper.md): UPDATE-only
-- правило (action `set_status`), переводит `souls.status='connected'` →
-- `disconnected`, если `last_seen_at` старше `stale_after` (default 90s).
--
-- В отличие от остальных правил, здесь нет фильтра по `target_statuses[]`
-- — только источник `connected`. Целевой статус нормирован документом
-- (`target_status: disconnected`) и здесь зашит — расширения нет, всё что
-- не connected → disconnected не имеет смысла (revoked / expired — это
-- terminal-состояния, их не двигаем).
--
-- Соответствие docs/keeper/reaper.md: «last_seen_at старше N + нет
-- live-стрима → disconnected». Эта функция — fallback-режим (single-instance
-- dev / unit без Redis): «нет live-стрима» проверяется неявно — live-стрим
-- обновляет `last_seen_at` через flush из Redis (ADR-006), поэтому stale
-- `last_seen_at` ⇔ нет живого стрима при одном инстансе.
--
-- Lease-aware режим (миграция 043, select_disconnect_candidates +
-- mark_disconnected_sids) переработал правило в две фазы со сверкой Redis
-- SID-lease — закрыл отложенный slice ADR-006(a). Эта функция оставлена как
-- fallback и НЕ удалена.
--
-- last_seen_at IS NULL — никогда не подключавшийся (только что
-- pending → connected без heartbeat-а). Под правило не попадает, потому
-- что NULL < NOW() - stale_after = NULL (SQL three-valued logic),
-- предикат `false`.

CREATE OR REPLACE FUNCTION mark_disconnected(stale_after interval, batch_size integer DEFAULT 1000)
RETURNS BIGINT AS $$
DECLARE
    updated_count BIGINT;
BEGIN
    WITH stale AS (
        SELECT sid
        FROM souls
        WHERE status = 'connected'
          AND last_seen_at < NOW() - stale_after
        ORDER BY last_seen_at
        LIMIT batch_size
    )
    UPDATE souls
       SET status = 'disconnected'
     WHERE sid IN (SELECT sid FROM stale);

    GET DIAGNOSTICS updated_count = ROW_COUNT;
    RETURN updated_count;
END;
$$ LANGUAGE plpgsql;

COMMENT ON FUNCTION mark_disconnected(interval, integer) IS
    'Переводит batch souls connected → disconnected, если last_seen_at старше stale_after. Возвращает количество обновлённых строк.';
