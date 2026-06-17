-- 060_create_choirs.up.sql
--
-- ADR-044 → S-T2 schema: Choir (именованная топология хостов внутри
-- инкарнации) + Voice (членство SID в Choir-е). Источник правды declared-
-- топологии — отдельные PG-таблицы + CRUD (НЕ `incarnation.state`, который
-- коммитится только под cross-host barrier — ADR-044 пункт 4).
--
-- Три РАЗНЫХ слоя (ADR-044 пункт 1, не дублировать):
--   * membership = `incarnation.name` в `souls.coven[]` (как было; не трогаем);
--   * coven      = стабильные логические теги (ADR-008);
--   * Choir      = именованная позиция хоста ВНУТРИ инкарнации.
--
-- Choir поглощает declared-роль (`incarnation.spec.hosts[].role` → `voice.role`,
-- ADR-044 пункт 2): `voice.role` — единственный источник declared-топологии
-- (питает `soulprint.hosts[].role` на S-T4).
--
-- Мультиинкарнационность (ADR-044 пункт 3): один SID легально является Voice в
-- Choir-ах РАЗНЫХ инкарнаций — PK включает incarnation_name, поэтому никакой
-- глобальной sid-уникальности НЕТ намеренно.
--
-- FK:
--   * incarnation_choirs.incarnation_name → incarnation(name) ON DELETE CASCADE
--     (снос инкарнации сносит её Choir-ы и каскадом их Voice-ы).
--   * incarnation_choirs.created_by_aid   → operators(aid) ON DELETE SET NULL
--     (Архонт-создатель; удаление оператора не теряет Choir).
--   * incarnation_choir_voices (incarnation_name, choir_name)
--       → incarnation_choirs (incarnation_name, choir_name) ON DELETE CASCADE.
--   * incarnation_choir_voices.sid          → souls(sid) ON DELETE CASCADE
--     (снос Soul-а из реестра убирает его Voice-ы; членство = souls.coven —
--     инвариант «Voice только для члена инкарнации» проверяется в CRUD-слое,
--     не FK-ом, т.к. членство — это значение элемента массива coven, не FK).
--   * incarnation_choir_voices.added_by_aid → operators(aid) ON DELETE SET NULL.

CREATE TABLE incarnation_choirs (
    incarnation_name TEXT        NOT NULL,
    choir_name       TEXT        NOT NULL,
    description      TEXT,
    min_size         INT,
    max_size         INT,
    created_at       TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    created_by_aid   TEXT,

    CONSTRAINT incarnation_choirs_pkey
        PRIMARY KEY (incarnation_name, choir_name),
    CONSTRAINT incarnation_choirs_name_format
        CHECK (choir_name ~ '^[a-z][a-z0-9_-]*$'),
    CONSTRAINT incarnation_choirs_min_size_positive
        CHECK (min_size IS NULL OR min_size > 0),
    CONSTRAINT incarnation_choirs_max_size_positive
        CHECK (max_size IS NULL OR max_size > 0),
    CONSTRAINT incarnation_choirs_min_le_max
        CHECK (min_size IS NULL OR max_size IS NULL OR min_size <= max_size),
    CONSTRAINT incarnation_choirs_incarnation_fk
        FOREIGN KEY (incarnation_name) REFERENCES incarnation (name) ON DELETE CASCADE,
    CONSTRAINT incarnation_choirs_created_by_aid_fk
        FOREIGN KEY (created_by_aid) REFERENCES operators (aid) ON DELETE SET NULL
);

CREATE TABLE incarnation_choir_voices (
    incarnation_name TEXT        NOT NULL,
    choir_name       TEXT        NOT NULL,
    sid              TEXT        NOT NULL,
    role             TEXT,
    position         INT,
    added_at         TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    added_by_aid     TEXT,

    CONSTRAINT incarnation_choir_voices_pkey
        PRIMARY KEY (incarnation_name, choir_name, sid),
    CONSTRAINT incarnation_choir_voices_position_non_negative
        CHECK (position IS NULL OR position >= 0),
    CONSTRAINT incarnation_choir_voices_choir_fk
        FOREIGN KEY (incarnation_name, choir_name)
        REFERENCES incarnation_choirs (incarnation_name, choir_name) ON DELETE CASCADE,
    CONSTRAINT incarnation_choir_voices_sid_fk
        FOREIGN KEY (sid) REFERENCES souls (sid) ON DELETE CASCADE,
    CONSTRAINT incarnation_choir_voices_added_by_aid_fk
        FOREIGN KEY (added_by_aid) REFERENCES operators (aid) ON DELETE SET NULL
);

-- Lookup всех Voice-ов одного хоста (для виртуальной проекции
-- `soulprint.self.choirs` — S-T4 Keeper-side join per-SID).
CREATE INDEX incarnation_choir_voices_sid_idx
    ON incarnation_choir_voices (sid);

COMMENT ON TABLE incarnation_choirs IS
    'Choir — именованная топология хостов внутри инкарнации (ADR-044, S-T2). Declared-группа («партия хора»); источник правды declared-топологии, НЕ incarnation.state. Choir != coven (ADR-008) и != membership (souls.coven).';

COMMENT ON TABLE incarnation_choir_voices IS
    'Voice — членство SID в Choir-е (ADR-044, S-T2). role — поглощённая declared-роль (spec.hosts[].role, ADR-044 пункт 2); position — порядковый индекс внутри партии. Инвариант: SID уже член инкарнации (souls.coven содержит incarnation.name) — проверяется в CRUD-слое. Один SID легально является Voice в разных инкарнациях (PK включает incarnation_name; глобальной sid-уникальности нет намеренно).';
