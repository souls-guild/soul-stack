-- 039_create_incarnation_archive.up.sql
--
-- S-D3 (incarnation.destroy, каскад V3): archive-таблицы под физический снос
-- строки incarnation. Решение пользователя — НЕ хранить tombstone в live-реестре,
-- а копировать compliance-минимум в отдельные archive-таблицы ДО DELETE, после
-- чего DELETE FROM incarnation каскадом сносит live state_history / apply_runs /
-- apply_task_register. Архив переживает каскад, потому что у него НЕТ FK на live
-- incarnation.
--
-- Две таблицы:
--   * incarnation_archive    — снимок ключевых колонок incarnation на момент
--     destroy (name / service / version / spec / state / status + временные метки)
--     + archived_at.
--   * state_history_archive   — снимок журнала state_history удаляемой
--     incarnation (история переходов важна для compliance) + archived_at.
--
-- Зачем archive-таблицы, а не tombstone-флаг в incarnation: live-реестр остаётся
-- чистым (status-enum не разрастается «удалённым» значением, FK-целостность
-- apply_runs/state_history не висит на мёртвой строке), а compliance-данные
-- сохраняются неограниченно (отдельное Reaper-правило ретеншена архива — backlog).
--
-- FK-инвариант: archive-таблицы НЕ ссылаются на incarnation(name) — иначе DELETE
-- родителя либо упал бы (RESTRICT), либо снёс бы только что записанный архив
-- (CASCADE). created_by_aid / changed_by_aid тоже НЕ FK на operators: архив —
-- замороженный снимок, он не обязан переживать ссылочную целостность реестра
-- операторов (AID хранится как строка-значение для аудита, не как живая ссылка).
--
-- name НЕ PK: одно и то же имя incarnation может быть пере-создано и снова
-- снесено (повторный destroy того же имени) — архив накапливает все инкарнации
-- этого имени. PK — суррогатный IDENTITY (archive_id), уникальный на запись.

CREATE TABLE incarnation_archive (
    archive_id            BIGINT      GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    name                  TEXT        NOT NULL,
    service               TEXT        NOT NULL,
    service_version       TEXT        NOT NULL,
    state_schema_version  INTEGER     NOT NULL,
    spec                  JSONB       NOT NULL DEFAULT '{}'::jsonb,
    state                 JSONB       NOT NULL DEFAULT '{}'::jsonb,
    status                TEXT        NOT NULL,
    status_details        JSONB,
    created_by_aid        TEXT,
    created_at            TIMESTAMPTZ NOT NULL,
    updated_at            TIMESTAMPTZ NOT NULL,
    archived_at           TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

-- Поиск архива по имени снесённой incarnation (compliance-запрос «что было у
-- redis-prod до удаления»).
CREATE INDEX incarnation_archive_name_idx
    ON incarnation_archive (name);

COMMENT ON TABLE incarnation_archive IS
    'Архив снесённых incarnation (S-D3, каскад V3). БЕЗ FK на live incarnation — переживает DELETE.';

CREATE TABLE state_history_archive (
    archive_id         BIGINT      GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    history_id         TEXT        NOT NULL,
    incarnation_name   TEXT        NOT NULL,
    scenario           TEXT        NOT NULL,
    state_before       JSONB       NOT NULL DEFAULT '{}'::jsonb,
    state_after        JSONB       NOT NULL DEFAULT '{}'::jsonb,
    changed_by_aid     TEXT,
    apply_id           TEXT        NOT NULL,
    at                 TIMESTAMPTZ NOT NULL,
    archived_at        TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

-- Лента архивной истории конкретной снесённой incarnation.
CREATE INDEX state_history_archive_incarnation_idx
    ON state_history_archive (incarnation_name);

COMMENT ON TABLE state_history_archive IS
    'Архив журнала state_history снесённых incarnation (S-D3). БЕЗ FK на live incarnation.';
