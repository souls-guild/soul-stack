-- 015_add_souls_soulprint.up.sql
--
-- Хранение typed-soulprint в реестре `souls` для M2.4 (event handler
-- `SoulprintReport`). Соответствует ADR-018 (`SoulprintFacts` proto) +
-- ADR-012 (Soul → Keeper EventStream, `SoulprintReport` payload).
--
--   * `soulprint_facts` — JSON-сериализация proto `SoulprintFacts`
--     (corresponding to `typed_facts` field). Sparse-поля proto
--     сериализуются jsonb-omit-empty стилем. Колонка nullable: до первого
--     `SoulprintReport` от свежеподключённого Soul-а `typed_facts` пуст.
--   * `soulprint_collected_at` — момент сбора фактов Soul-агентом
--     (proto `collected_at`). Хранится отдельно от received_at — расхождение
--     received_at - collected_at > 10 min логируется warn-ом по ADR-018.
--   * `soulprint_received_at` — момент прихода `SoulprintReport` на
--     Keeper-у. Сужает Reaper-овский «stale soulprint»-detector (отдельный
--     slice пост-MVP).
--
-- Schema-version из proto (`SoulprintFacts` без явного version field в
-- ADR-018) тут не материализуем — выделится отдельной колонкой через
-- forward-compat миграцию, если ADR закрепит явный `schema_version`.

ALTER TABLE souls
    ADD COLUMN soulprint_facts        JSONB,
    ADD COLUMN soulprint_collected_at TIMESTAMPTZ,
    ADD COLUMN soulprint_received_at  TIMESTAMPTZ;

COMMENT ON COLUMN souls.soulprint_facts IS
    'Typed Soulprint (ADR-018, SoulprintFacts proto) — JSONB-сериализация последнего SoulprintReport.';
COMMENT ON COLUMN souls.soulprint_collected_at IS
    'Soul-side timestamp последнего SoulprintReport (proto collected_at, ADR-018).';
COMMENT ON COLUMN souls.soulprint_received_at IS
    'Keeper-side timestamp получения последнего SoulprintReport (ADR-018).';
