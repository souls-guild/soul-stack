-- 012_create_purge_souls.up.sql
--
-- Reaper-правило `purge_souls` (docs/keeper/reaper.md): удаляет записи
-- `souls` в указанных `statuses[]` (default `[disconnected, expired]`)
-- с возрастом старше `max_age`. Возраст считается от `last_seen_at` для
-- ранее живых Soul-ов; для никогда не подключавшихся (last_seen_at IS NULL)
-- — от `registered_at`.
--
-- Параметризация `statuses[]` нужна, потому что без фильтра по статусу
-- `delete` снёс бы живые `connected`-записи (см. reaper.md → колонка
-- «Обязательные поля»).
--
-- ON DELETE CASCADE на FK `bootstrap_tokens.sid` и `soul_seeds.sid` (см.
-- 008/009) — удаление Soul-а автоматически чистит связанные токены и
-- сертификатные записи. Это сознательное решение: история токенов/seed-ов
-- умирает с Soul-ом, как state_history vs incarnation.

CREATE OR REPLACE FUNCTION purge_souls(
    target_statuses text[],
    max_age         interval,
    batch_size      integer DEFAULT 1000
)
RETURNS BIGINT AS $$
DECLARE
    deleted_count BIGINT;
BEGIN
    WITH expired AS (
        SELECT sid
        FROM souls
        WHERE status = ANY(target_statuses)
          AND COALESCE(last_seen_at, registered_at) < NOW() - max_age
        ORDER BY COALESCE(last_seen_at, registered_at)
        LIMIT batch_size
    )
    DELETE FROM souls
    WHERE sid IN (SELECT sid FROM expired);

    GET DIAGNOSTICS deleted_count = ROW_COUNT;
    RETURN deleted_count;
END;
$$ LANGUAGE plpgsql;

COMMENT ON FUNCTION purge_souls(text[], interval, integer) IS
    'Удаляет batch souls в указанных статусах с COALESCE(last_seen_at, registered_at) старше max_age. Возвращает количество удалённых строк. CASCADE на bootstrap_tokens/soul_seeds.';
