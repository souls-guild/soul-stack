-- 093_create_purge_old_certs.up.sql
--
-- Reaper-правило `purge_old_certs` (R4, cert-rotation Вар1): удаляет batch
-- строк `warrant` в указанных `statuses[]` (default `[superseded, expired,
-- failed]`) с `issued_at` старше `max_age`. Retention растущей истории
-- ротаций сервисных сертов (реестр warrant пухнет superseded-строками).
--
-- Active/rotating НЕ удаляются (statuses-фильтр их исключает): active — живой
-- материал, rotating — серт в процессе ротации (Reaper-правило rotate_due_certs
-- захватило его CAS-ом). Возраст считается от `issued_at` (время существования
-- строки), симметрично purge_old_seeds (013).
--
-- Parity purge_old_seeds: та же сигнатура (statuses[], max_age, batch_size),
-- тот же batch-паттерн (LIMIT + DELETE ... WHERE cert_id IN).

CREATE OR REPLACE FUNCTION purge_old_certs(
    target_statuses text[],
    max_age         interval,
    batch_size      integer DEFAULT 1000
)
RETURNS BIGINT AS $$
DECLARE
    deleted_count BIGINT;
BEGIN
    WITH old AS (
        SELECT cert_id
        FROM warrant
        WHERE status = ANY(target_statuses)
          AND issued_at < NOW() - max_age
        ORDER BY issued_at
        LIMIT batch_size
    )
    DELETE FROM warrant
    WHERE cert_id IN (SELECT cert_id FROM old);

    GET DIAGNOSTICS deleted_count = ROW_COUNT;
    RETURN deleted_count;
END;
$$ LANGUAGE plpgsql;

COMMENT ON FUNCTION purge_old_certs(text[], interval, integer) IS
    'Удаляет batch warrant-строк в указанных статусах (superseded/expired/failed) с issued_at старше max_age. Возвращает количество удалённых строк.';
