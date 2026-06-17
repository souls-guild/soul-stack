-- 049_create_archive_state_history.up.sql
--
-- SQL-функция Reaper-правила `archive_state_history` (ADR-Q19 retention).
-- Soft-deletes (`archived_at = NOW()`) старые активные снимки `state_history`
-- сверх N последних на incarnation, ИСКЛЮЧАЯ снимки шагов state_schema
-- миграции (`scenario = 'migration'`, см. migrationScenarioLabel в
-- keeper/internal/incarnation/crud.go::writeMigrationHistory / Unlock —
-- последний пишет scenario='unlock'; миграция-снимок единственный, кто
-- идёт под scenario='migration', что делает критерий устойчивым).
--
-- Параметры:
--   * keep_last_n      — сколько новейших активных снимков оставлять на
--                        incarnation (по at DESC). default semantics
--                        задаёт runner (default 50).
--   * keep_version_bump — true: снимки шагов миграции НЕ архивируются
--                        никогда, независимо от keep_last_n. false:
--                        правило архивирует их наравне с обычными.
--   * batch             — лимит soft-deleted за один прогон (защита от
--                        длинных UPDATE при первом включении правила на
--                        накопленной истории).
--
-- Алгоритм:
--   1. Window-функция `row_number() OVER (PARTITION BY incarnation_name
--      ORDER BY at DESC, history_id ASC)` нумерует активные снимки
--      внутри каждой incarnation от 1 (новейший) и далее. ORDER BY
--      history_id ASC — стабильный tie-breaker при равных `at` (ULID
--      монотонен — старший = свежее, ASC = старший по at-tie остаётся
--      «выше», т.е. ближе к keep-окну; компромисс ради детерминизма).
--   2. Фильтр rn > keep_last_n — это снимки «сверх N», кандидаты на
--      архив.
--   3. Если keep_version_bump = true — дополнительно исключаем строки
--      со scenario='migration' (version-bump snapshots; restorable
--      anchor для миграций ADR-019).
--   4. LIMIT batch в подзапросе — батч-cap, чтобы первый прогон не
--      положил БД одним долгим UPDATE.
--   5. UPDATE по подзапросу-PK устанавливает archived_at = NOW().
--      Возвращаемое count(*) — affected rows за этот батч.

CREATE OR REPLACE FUNCTION archive_state_history(
    keep_last_n        integer,
    keep_version_bump  boolean,
    batch              integer
) RETURNS BIGINT
LANGUAGE sql AS $$
    WITH ranked AS (
        SELECT history_id,
               scenario,
               row_number() OVER (
                   PARTITION BY incarnation_name
                   ORDER BY at DESC, history_id ASC
               ) AS rn
        FROM state_history
        WHERE archived_at IS NULL
    ),
    archived AS (
        UPDATE state_history sh
        SET archived_at = NOW()
        WHERE sh.history_id IN (
            SELECT history_id
            FROM ranked
            WHERE rn > keep_last_n
              AND (NOT keep_version_bump OR scenario <> 'migration')
            ORDER BY rn DESC, history_id ASC
            LIMIT batch
        )
        RETURNING 1
    )
    SELECT count(*) FROM archived;
$$;

COMMENT ON FUNCTION archive_state_history(integer, boolean, integer) IS
    'Reaper archive_state_history (ADR-Q19): soft-delete активных снимков сверх N последних на incarnation, опц. с защитой version-bump (scenario=migration).';
