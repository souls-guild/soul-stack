-- 035_create_keeper_settings.up.sql
--
-- Cluster-wide key-value скаляры Keeper-а — managed-через-API настройки,
-- которые раньше жили top-level скалярами в keeper.yml. Один источник правды на
-- весь кластер (вместо per-node конфига), видимый всем нодам и переживающий
-- рестарт.
--
-- Хранилище плоское: key (PK, snake_case) → value (TEXT). Семантика value и
-- набор well-known ключей живут в Go-слое (serviceregistry), не в схеме —
-- таблица намеренно untyped, чтобы добавление новой настройки не требовало
-- миграции.
--
-- Well-known ключи MVP:
--   - `default_destiny_source` — дефолтный git-источник Destiny.
-- `default_module_source` НЕ заводится: у него нет потребителя в keeper-коде
-- (мёртвое поле прежнего конфига).
--
-- Сами строки настроек — runtime-данные (пишутся через API), поэтому миграция
-- НЕ вставляет ни одного well-known ключа: только создаёт таблицу.
--
-- FK updated_by_aid → operators(aid) ON DELETE SET NULL: запись настройки
-- переживает удаление оператора, последний её менявший — обнуляется
-- (симметрично omens/providers; здесь SET NULL уместен, т.к. колонка NULL-able
-- и значение настройки важнее автора).

CREATE TABLE keeper_settings (
    key            TEXT        PRIMARY KEY,
    value          TEXT        NOT NULL,
    updated_at     TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_by_aid TEXT        REFERENCES operators (aid) ON DELETE SET NULL,

    CONSTRAINT keeper_settings_key_format
        CHECK (key ~ '^[a-z][a-z0-9_]*$')
);

COMMENT ON TABLE keeper_settings IS
    'Cluster-wide key-value скаляры Keeper-а (managed-через-API). key = snake_case, value = TEXT; well-known ключи живут в Go-слое.';
