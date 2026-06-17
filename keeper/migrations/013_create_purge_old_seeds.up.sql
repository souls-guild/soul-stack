-- 013_create_purge_old_seeds.up.sql
--
-- Reaper-правило `purge_old_seeds` (docs/keeper/reaper.md): удаляет
-- batch записей `soul_seeds` в указанных `statuses[]` (default
-- `[superseded, expired, revoked]`) с `issued_at` старше `max_age`.
--
-- Active-seed-ы НЕ удаляются под этим правилом (statuses-фильтр их
-- исключает, см. reaper.md). Active-seed может «умереть» только через
-- ротацию (status → superseded) или revoke (status → revoked) — оба
-- сценария регулируются soul-side / Operator API, не Жнецом.
--
-- Возраст считается от `issued_at`, а не от перехода в текущий статус.
-- Это сознательное решение: история сертификата измеряется временем
-- существования, а не остаточным сроком после ротации/отзыва.

CREATE OR REPLACE FUNCTION purge_old_seeds(
    target_statuses text[],
    max_age         interval,
    batch_size      integer DEFAULT 1000
)
RETURNS BIGINT AS $$
DECLARE
    deleted_count BIGINT;
BEGIN
    WITH expired AS (
        SELECT seed_id
        FROM soul_seeds
        WHERE status = ANY(target_statuses)
          AND issued_at < NOW() - max_age
        ORDER BY issued_at
        LIMIT batch_size
    )
    DELETE FROM soul_seeds
    WHERE seed_id IN (SELECT seed_id FROM expired);

    GET DIAGNOSTICS deleted_count = ROW_COUNT;
    RETURN deleted_count;
END;
$$ LANGUAGE plpgsql;

COMMENT ON FUNCTION purge_old_seeds(text[], interval, integer) IS
    'Удаляет batch soul_seeds в указанных статусах с issued_at старше max_age. Возвращает количество удалённых строк.';
