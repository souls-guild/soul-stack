-- 048_state_history_archived_at.up.sql
--
-- ADR-Q19 retention (PM-решение, 2026-05): записи `state_history` НЕ удаляются
-- физически — старые снимки помечаются soft-delete-флагом `archived_at` и
-- хранятся дальше (опц. внешнего bulk-выгрузчика). Default-политика —
-- последние N=50 на incarnation + всегда snapshot шагов state_schema
-- миграции (scenario='migration'); см. docs/keeper/reaper.md, правило
-- `archive_state_history`.
--
-- Поведение колонки:
--   * archived_at IS NULL      — активный снимок: видим в Operator API /
--     MCP / Soul-resolver; учитывается фильтром «active» при пагинации.
--   * archived_at IS NOT NULL  — soft-deleted: момент пометки правилом
--     Reaper-а. По дефолту скрыт от чтения; читается отдельным флагом
--     `include_archived=true` (Operator API) при необходимости разбора.
--
-- Запись новых снимков (INSERT в state_history) поведение не меняет:
-- свежие записи попадают с archived_at = NULL по DEFAULT, никаких правок
-- INSERT-ов не требуется.
--
-- Index `state_history_active_idx` — partial по WHERE archived_at IS NULL,
-- покрывает типовой запрос ленты истории (HistorySelectByName + Operator
-- API GET /v1/incarnations/{name}/history). Без него фильтр active
-- упирался бы в существующий `state_history_incarnation_at_idx` с лишним
-- проходом по soft-deleted-строкам — при retention 50 живых / 1000+ soft-
-- deleted это растёт линейно.

ALTER TABLE state_history
    ADD COLUMN archived_at TIMESTAMPTZ;

CREATE INDEX state_history_active_idx
    ON state_history (incarnation_name, at DESC)
    WHERE archived_at IS NULL;

COMMENT ON COLUMN state_history.archived_at IS
    'Soft-delete-флаг (ADR-Q19 retention). NULL = активный снимок; NOT NULL = soft-deleted-time правилом Reaper archive_state_history.';
