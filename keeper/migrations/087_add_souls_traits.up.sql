-- 087_add_souls_traits.up.sql
--
-- Trait — operator-set key-value метки на Soul (ADR-060). Отдельная ось рядом с
-- плоским `souls.coven TEXT[]` (ADR-008): coven остаётся множеством логических
-- меток членства/таргетинга/RBAC, traits несут атрибуты (владелец/продукт/
-- namespace) в форме key → (scalar | list).
--
--   * `traits` — jsonb, потому что значение полиморфно (скаляр ИЛИ список) —
--     `TEXT[]` этого не выражает. NOT NULL DEFAULT '{}' — отсутствие traits = пустой
--     объект, не NULL (симметрия с `coven` DEFAULT '{}'::text[]): read-путь и
--     registry-проекция `soulprint.self.traits` не различают «нет колонки» / «нет
--     меток».
--   * Источник — оператор (write-путь — следующий слайс, ADR-060 п.5); пилот
--     read/target-only.
--
-- `souls.coven TEXT[]` НЕ трогается (Вариант B ADR-060: расширение coven до
-- key-value отвергнуто — ломает scope-pushdown `$1 = ANY(coven)` и предикаты
-- `'x' in soulprint.self.covens`).

ALTER TABLE souls
    ADD COLUMN traits JSONB NOT NULL DEFAULT '{}'::jsonb;

-- GIN-индекс под таргетинг по traits: `traits @> '{"namespace":"dba-ns"}'`
-- (containment) — стандартный путь для jsonb-предикатов (параллель
-- `souls_coven_idx` GIN по text[]). Поддерживает будущий write-/scope-слой,
-- не блокирует read/target пилот.
CREATE INDEX souls_traits_idx
    ON souls USING GIN (traits);

COMMENT ON COLUMN souls.traits IS
    'Trait — operator-set key-value метки (ADR-060); значение scalar|list, отдельная ось рядом с coven.';
