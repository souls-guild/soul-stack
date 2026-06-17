-- 011_create_purge_used_tokens.up.sql
--
-- Reaper-правило `purge_used_tokens` (docs/keeper/reaper.md):
-- удаляет batch использованных bootstrap-токенов старше max_age от `used_at`.
-- Использованный токен (`used_at IS NOT NULL`) уже не несёт защитной
-- функции; долговременный аудит создания/использования хранится в
-- `audit_log` под своим retention-ом (ADR-022).
--
-- Default `max_age` в конфиге — 90d (docs/keeper/reaper.md).

CREATE OR REPLACE FUNCTION purge_used_tokens(max_age interval, batch_size integer DEFAULT 1000)
RETURNS BIGINT AS $$
DECLARE
    deleted_count BIGINT;
BEGIN
    WITH expired AS (
        SELECT token_id
        FROM bootstrap_tokens
        WHERE used_at IS NOT NULL
          AND used_at < NOW() - max_age
        ORDER BY used_at
        LIMIT batch_size
    )
    DELETE FROM bootstrap_tokens
    WHERE token_id IN (SELECT token_id FROM expired);

    GET DIAGNOSTICS deleted_count = ROW_COUNT;
    RETURN deleted_count;
END;
$$ LANGUAGE plpgsql;

COMMENT ON FUNCTION purge_used_tokens(interval, integer) IS
    'Удаляет batch использованных bootstrap_tokens с used_at старше max_age. Возвращает количество удалённых строк. Reaper-loop вызывает в цикле до возврата 0 (drain-pattern).';
