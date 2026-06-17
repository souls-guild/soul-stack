-- 075_create_purge_voyages.up.sql
--
-- Reaper-правило `purge_voyages` (docs/keeper/reaper.md): удаляет batch
-- завершённых Voyage-прогонов из реестра `voyages` (миграция 059) старше
-- `max_age`. Retention растущей истории прогонов — реализация отложенного
-- `purge_voyages` из ADR-046 §79 (там значился «если будет введён»; на пути
-- к бете рост `voyages` на бета-флоте сделал правило обязательным).
--
-- ПОЧЕМУ именно `voyages` (а не cadences/choirs/errands):
--   * `voyages` — ИСТОРИЯ прогонов, растёт без потолка (каждый ручной запуск,
--     каждый спавн Cadence = новая строка). Единственная run-history таблица
--     БЕЗ retention (apply_runs/audit/errands/seeds/souls/state_history уже
--     покрыты своими правилами; tides/errand_runs дропнуты миграциями 061/062;
--     отдельной таблицы `cadence_runs`/`surges` нет — история спавнов = сами
--     `voyages` с populated `cadence_id`, surge поглощён `voyage_targets.batch_index`).
--   * `cadences` — АКТИВНЫЕ расписания (enabled, переживают прогоны), НЕ история.
--     Их retention был бы порчей данных (см. ADR-046 §9: удаление Cadence — это
--     операторское действие, а не возрастная гигиена). НЕ purge.
--   * `incarnation_choirs`/`incarnation_choir_voices` — активная declared-топология,
--     НЕ история. НЕ purge.
--
-- Удаляются ТОЛЬКО finished-записи (`succeeded`/`failed`/`partial_failed`/
-- `cancelled` с `finished_at IS NOT NULL`). `scheduled`/`pending`/`running`
-- НИКОГДА не purge — это незавершённые/идущие прогоны (терминал им проставит
-- VoyageWorker.Finalize или reclaim_voyages, не Жнец). Зеркало `purge_apply_runs`
-- (021): только терминал, возраст от `finished_at`.
--
-- Возраст считается от `finished_at`: история измеряется временем с момента
-- завершения, а не старта (долгий прогон, завершившийся недавно, не должен
-- удаляться раньше короткого, завершившегося давно). Parity purge_apply_runs.
--
-- КАСКАДЫ / ЗАВИСИМОСТИ (инварианты «не оставить битых ссылок»):
--   * `voyage_targets.voyage_id` → voyages ON DELETE CASCADE (миграция 059):
--     Leg-строки прогона уносятся каскадом. Корректно — это части того же прогона.
--   * `voyage_targets.apply_id`/`errand_id` — soft-link БЕЗ FK на apply_runs/errands.
--     Purge voyage их НЕ трогает: apply_runs чистится `purge_apply_runs` по СВОЕМУ
--     окну, errands — `purge_old_errands`. ИНВАРИАНТ КОРРЕЛЯЦИИ: окно purge_voyages
--     по умолчанию = окно purge_apply_runs (30d), чтобы drill «voyage → его
--     apply_runs» не терял одну из сторон (voyage удалён, а apply_runs ещё нужны
--     для correlation — или наоборот). Выравнивание дефолтов — в keeper.yml/коде.
--   * `tidings.voyage_id` — ephemeral-Tiding, soft-link БЕЗ FK (миграция 072,
--     намеренно: очистка через терминал + `purge_orphan_ephemeral_tidings` grace 5m).
--     На момент purge_voyages (30d) ephemeral-Tiding-и давно снесены тем правилом;
--     если же остался осиротевший — его подберёт `purge_orphan_ephemeral_tidings`
--     по предикату `NOT EXISTS voyages`. Битых ссылок DELETE voyage НЕ создаёт.
--   * `voyages.cadence_id` → cadences ON DELETE SET NULL (миграция 066): purge
--     ДЕТЕЙ не трогает РАСПИСАНИЕ (FK направлен от voyage к cadence, не наоборот).
--
-- PK `voyage_id` (059) → DELETE по single-column WHERE voyage_id IN (...);
-- CTE с LIMIT batch_size ограничивает размер транзакции, как в остальных
-- Reaper-правилах. Используется индекс `voyages_pending_pickup_idx`? — нет, тот
-- partial по pending; для покрытия finished-скана достаточно последовательного
-- прохода с LIMIT (history-purge — холодный фон, не hot-path), отдельный индекс
-- по finished_at не заводим до появления реального объёма (parity purge_apply_runs,
-- где тоже нет выделенного finished_at-индекса).

-- ВНИМАНИЕ: параметр LIMIT назван `batch_limit`, а НЕ `batch_size` (в отличие от
-- purge_apply_runs 021): таблица `voyages` НЕСЁТ колонку `batch_size` (миграция
-- 059, размер батча прогона), и одноимённый параметр функции в `LIMIT batch_size`
-- даёт `column reference "batch_size" is ambiguous` (SQLSTATE 42702) — PG
-- предпочитает колонку из FROM. У apply_runs такой колонки нет, поэтому 021 не
-- страдает. Имя параметра — единственное отличие от шаблона 021.
CREATE OR REPLACE FUNCTION purge_voyages(max_age interval, batch_limit integer DEFAULT 1000)
RETURNS BIGINT AS $$
DECLARE
    deleted_count BIGINT;
BEGIN
    WITH expired AS (
        SELECT voyage_id FROM voyages
        WHERE status IN ('succeeded', 'failed', 'partial_failed', 'cancelled')
          AND finished_at IS NOT NULL
          AND finished_at < NOW() - max_age
        ORDER BY finished_at
        LIMIT batch_limit
    )
    DELETE FROM voyages v
    USING expired e
    WHERE v.voyage_id = e.voyage_id;

    GET DIAGNOSTICS deleted_count = ROW_COUNT;
    RETURN deleted_count;
END;
$$ LANGUAGE plpgsql;

COMMENT ON FUNCTION purge_voyages(interval, integer) IS
    'Удаляет batch finished voyages (succeeded/failed/partial_failed/cancelled) с finished_at старше max_age. scheduled/pending/running не трогает. voyage_targets уносятся ON DELETE CASCADE. Возвращает количество удалённых строк (ADR-046 §79, parity purge_apply_runs).';
