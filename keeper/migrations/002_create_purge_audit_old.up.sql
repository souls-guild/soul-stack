-- 002_create_purge_audit_old.up.sql
--
-- ADR-022(d): retention для audit_log через Reaper-правило
-- `purge_audit_old` (см. docs/keeper/reaper.md). PL/pgSQL-функция
-- удаляет один batch expired-записей и возвращает количество удалённых
-- строк; Reaper-loop (M0.6) вызывает её в цикле до возврата 0
-- (drain-pattern). Batch-DELETE снижает lock-contention по сравнению с
-- whole-table-DELETE.
--
-- `max_age` приходит из конфига (`keeper.yml → reaper.rules.purge_audit_old.max_age`,
-- alias на `audit.retention_days`); параметризация на стороне функции,
-- а не литерала в SQL — caller (reaper-runner) свободен ротировать
-- значение без re-create функции.

CREATE OR REPLACE FUNCTION purge_audit_old(max_age interval, batch_size integer DEFAULT 1000)
RETURNS BIGINT AS $$
DECLARE
    deleted_count BIGINT;
BEGIN
    WITH expired AS (
        SELECT audit_id
        FROM audit_log
        WHERE created_at < NOW() - max_age
        ORDER BY created_at
        LIMIT batch_size
    )
    DELETE FROM audit_log
    WHERE audit_id IN (SELECT audit_id FROM expired);

    GET DIAGNOSTICS deleted_count = ROW_COUNT;
    RETURN deleted_count;
END;
$$ LANGUAGE plpgsql;

COMMENT ON FUNCTION purge_audit_old(interval, integer) IS
    'Удаляет batch expired audit_log записей (created_at < NOW() - max_age). Возвращает количество удалённых строк. Reaper-loop вызывает в цикле до возврата 0 (drain-pattern).';
