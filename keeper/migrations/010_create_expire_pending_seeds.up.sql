-- 010_create_expire_pending_seeds.up.sql
--
-- Reaper-правило `expire_pending_seeds` (docs/keeper/reaper.md).
-- Семантика — DELETE: bootstrap-токен с `used_at IS NULL` и истёкшим
-- `expires_at` не может быть использован (проверка на Burn-стороне
-- отвергает истёкшие токены), хранить его дальше смысла нет.
--
-- ПРИМЕЧАНИЕ: на момент имплементации Reaper.b принято PM-решение
-- переинтерпретировать правило как DELETE (а не UPDATE-with-status):
-- таблица `bootstrap_tokens` не имеет колонки `status`, а отдельной
-- семантики «expired token, но ещё хранится» в MVP нет. Аудит создания
-- токена живёт в `audit_log` под своим retention-ом (ADR-022).
--
-- Параметр `max_age` сейчас не используется: критерий — `expires_at < NOW()`,
-- порог TTL зафиксирован при создании токена. Параметр оставлен в сигнатуре
-- для симметрии с остальными правилами и hot-reload-конвенцией (если в
-- будущем оператор захочет ввести grace-period «не удалять сразу истёкшие
-- N часов» — это станет `expires_at < NOW() - max_age`).

CREATE OR REPLACE FUNCTION expire_pending_seeds(max_age interval, batch_size integer DEFAULT 1000)
RETURNS BIGINT AS $$
DECLARE
    deleted_count BIGINT;
BEGIN
    WITH expired AS (
        SELECT token_id
        FROM bootstrap_tokens
        WHERE used_at IS NULL
          AND expires_at < NOW() - max_age
        ORDER BY expires_at
        LIMIT batch_size
    )
    DELETE FROM bootstrap_tokens
    WHERE token_id IN (SELECT token_id FROM expired);

    GET DIAGNOSTICS deleted_count = ROW_COUNT;
    RETURN deleted_count;
END;
$$ LANGUAGE plpgsql;

COMMENT ON FUNCTION expire_pending_seeds(interval, integer) IS
    'Удаляет batch неиспользованных bootstrap_tokens с истёкшим expires_at (older than max_age beyond expiry). Возвращает количество удалённых строк. Reaper-loop вызывает в цикле до возврата 0 (drain-pattern).';
