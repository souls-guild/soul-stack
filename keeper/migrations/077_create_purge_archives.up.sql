-- 077_create_purge_archives.up.sql
--
-- Reaper-правила retention архивных данных compliance-класса
-- (docs/keeper/reaper.md): три SQL-функции, удаляющие старые архивные строки
-- по их возрастной метке. Закрывает backlog «отдельное Reaper-правило ретеншена
-- архива», помеченный в миграции 039 (incarnation_archive / state_history_archive).
--
-- ТРИ ПОВЕРХНОСТИ РОСТА (все накапливались без потолка до этой миграции):
--
--   1. incarnation_archive (миграция 039) — снимок снесённых incarnation на
--      момент destroy. Растёт на каждый destroy; никогда не чистился.
--   2. state_history_archive (миграция 039) — снимок журнала state_history
--      снесённой incarnation. Растёт вместе с (1); никогда не чистился.
--   3. state_history с archived_at IS NOT NULL (миграция 048) — soft-deleted
--      снимки в ЖИВОЙ таблице state_history. Правило archive_state_history
--      (049) ТОЛЬКО проставляет флаг (archived_at = NOW()), но физически их не
--      удаляет — soft-deleted-хвост рос без потолка, утяжеляя живую таблицу.
--
-- ВОЗРАСТНАЯ МЕТКА (по которой считается окно retention):
--   * incarnation_archive.archived_at    — момент записи в архив при destroy.
--   * state_history_archive.archived_at   — то же (записываются одной транзакцией).
--   * state_history.archived_at           — момент soft-delete правилом
--     archive_state_history. NULL = активный снимок (НИКОГДА не удаляется этим
--     правилом — он не soft-deleted); фильтр archived_at IS NOT NULL обязателен.
--
-- ОКНО: архив — историко-compliance данные, поэтому консервативный дефолт
-- задаётся runner-ом (365 дней, см. defaultPurgeArchive* в runner.go),
-- настраивается через keeper.yml → reaper.rules.<rule>.max_age. Возраст
-- считается от соответствующего archived_at: строка удаляется только если она
-- старше окна. Свежее окна НЕ трогаем (цена ошибки в DELETE архива высокая).
--
-- BATCH: CTE с LIMIT batch_size ограничивает размер транзакции, как во всех
-- остальных Reaper-правилах (защита от длинного DELETE при первом включении на
-- накопленном архиве). DELETE ... USING expired по PK архива.
--
-- КАСКАДЫ / FK: дочерних таблиц с FK на incarnation_archive /
-- state_history_archive НЕТ (миграция 039: архив намеренно без ссылочной
-- целостности к live-реестру, проверено grep'ом — нет REFERENCES *_archive).
-- created_by_aid / changed_by_aid в архиве — строки-значения, НЕ FK (039),
-- DELETE их не задевает. У state_history FK направлен ОТ неё к incarnation
-- (ON DELETE CASCADE, 006) — DELETE soft-deleted-снимка родителя не трогает.
-- Битых ссылок ни один DELETE не создаёт.

CREATE OR REPLACE FUNCTION purge_incarnation_archive(max_age interval, batch_size integer DEFAULT 1000)
RETURNS BIGINT AS $$
DECLARE
    deleted_count BIGINT;
BEGIN
    WITH expired AS (
        SELECT archive_id FROM incarnation_archive
        WHERE archived_at < NOW() - max_age
        ORDER BY archived_at
        LIMIT batch_size
    )
    DELETE FROM incarnation_archive ia
    USING expired e
    WHERE ia.archive_id = e.archive_id;

    GET DIAGNOSTICS deleted_count = ROW_COUNT;
    RETURN deleted_count;
END;
$$ LANGUAGE plpgsql;

COMMENT ON FUNCTION purge_incarnation_archive(interval, integer) IS
    'Удаляет batch строк incarnation_archive с archived_at старше max_age (compliance retention, default 365d из runner). Дочерних FK нет (039). Возвращает количество удалённых строк.';

CREATE OR REPLACE FUNCTION purge_state_history_archive(max_age interval, batch_size integer DEFAULT 1000)
RETURNS BIGINT AS $$
DECLARE
    deleted_count BIGINT;
BEGIN
    WITH expired AS (
        SELECT archive_id FROM state_history_archive
        WHERE archived_at < NOW() - max_age
        ORDER BY archived_at
        LIMIT batch_size
    )
    DELETE FROM state_history_archive sha
    USING expired e
    WHERE sha.archive_id = e.archive_id;

    GET DIAGNOSTICS deleted_count = ROW_COUNT;
    RETURN deleted_count;
END;
$$ LANGUAGE plpgsql;

COMMENT ON FUNCTION purge_state_history_archive(interval, integer) IS
    'Удаляет batch строк state_history_archive с archived_at старше max_age (compliance retention, default 365d из runner). Дочерних FK нет (039). Возвращает количество удалённых строк.';

-- purge_archived_state_history — физический DELETE soft-deleted-снимков
-- (archived_at IS NOT NULL) из ЖИВОЙ таблицы state_history старше max_age.
-- Фильтр archived_at IS NOT NULL обязателен: активные снимки (archived_at IS
-- NULL) — это рабочая лента истории, удалять их это правило НЕ должно (их
-- soft-delete — отдельное правило archive_state_history, 049). Возраст —
-- от archived_at (момент soft-delete), не от at (момент события): окно
-- считается «сколько данные лежат soft-deleted», parity с архив-таблицами.
CREATE OR REPLACE FUNCTION purge_archived_state_history(max_age interval, batch_size integer DEFAULT 1000)
RETURNS BIGINT AS $$
DECLARE
    deleted_count BIGINT;
BEGIN
    WITH expired AS (
        SELECT history_id FROM state_history
        WHERE archived_at IS NOT NULL
          AND archived_at < NOW() - max_age
        ORDER BY archived_at
        LIMIT batch_size
    )
    DELETE FROM state_history sh
    USING expired e
    WHERE sh.history_id = e.history_id;

    GET DIAGNOSTICS deleted_count = ROW_COUNT;
    RETURN deleted_count;
END;
$$ LANGUAGE plpgsql;

COMMENT ON FUNCTION purge_archived_state_history(interval, integer) IS
    'Удаляет batch soft-deleted-снимков (archived_at IS NOT NULL) из живой state_history старше max_age (compliance retention, default 365d из runner). Активные снимки (archived_at IS NULL) НЕ трогает. Возвращает количество удалённых строк.';
