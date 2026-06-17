-- 074_tiding_created_from_cadence.up.sql
--
-- ADR-052 §m (постоянный Tiding из формы Cadence) + ADR-046 §9 (каскад-снос
-- автоправил при удалении Cadence).
--
-- Расширяет реестр `tidings` одной additive-колонкой (ни одно существующее поле/
-- контракт не меняет семантику):
--   - created_from_cadence_id — маркер ПРОИСХОЖДЕНИЯ: «правило создано из блока
--     notify[] формы ЭТОГО расписания» (POST /v1/cadences). NULL = правило
--     заведено иным путём (CRUD Tiding вручную / ephemeral от Voyage). Непустое
--     значение → FK на cadences(id) ON DELETE CASCADE: снос Cadence атомарно
--     уносит порождённые ею правила.
--
-- Зачем отдельный маркер, а НЕ переиспользование селектора `cadence`: колонка
-- `cadence` — это СЕЛЕКТОР подписки («слать только про прогоны этого расписания»,
-- может стоять и на вручную созданном Tiding-е). Каскад-удаление обязано сносить
-- ТОЛЬКО правила, рождённые формой, и НЕ трогать руками заведённые с тем же
-- cadence-селектором. Поэтому происхождение — отдельная колонка с FK CASCADE,
-- ортогональная фильтр-селектору `cadence` (ADR-046 §9, ADR-052 §m).
--
-- Привязка по cadences.id (ULID-PK, rename-safe), НЕ по имени расписания: имя
-- Cadence мутабельно (PATCH), а ULID — стабильный идентификатор.
--
-- cadences.id — TEXT PRIMARY KEY (миграция 066) → корректная цель FK для TEXT-
-- колонки. ON DELETE CASCADE (а не SET NULL, как voyages.cadence_id): осиротевшее
-- автоправило без расписания бессмысленно (его создавала форма расписания), его
-- надо снести вместе с Cadence.

ALTER TABLE tidings
    ADD COLUMN created_from_cadence_id TEXT;

ALTER TABLE tidings
    ADD CONSTRAINT tidings_created_from_cadence_fk
        FOREIGN KEY (created_from_cadence_id) REFERENCES cadences (id) ON DELETE CASCADE;

-- Каскад-снос (DELETE cadence) скан-ит правила по этому back-link. Partial-индекс
-- среди непустых — горячий путь FK-каскада не сканирует основную массу правил с
-- NULL-маркером.
CREATE INDEX tidings_created_from_cadence_idx
    ON tidings (created_from_cadence_id)
    WHERE created_from_cadence_id IS NOT NULL;

COMMENT ON COLUMN tidings.created_from_cadence_id IS
    'Маркер происхождения: правило создано из блока notify[] формы Cadence (POST /v1/cadences), ADR-052 §m. NULL = заведено иначе. FK cadences(id) ON DELETE CASCADE — снос Cadence уносит порождённые правила (ADR-046 §9). Ортогонально селектору cadence (фильтр подписки).';
