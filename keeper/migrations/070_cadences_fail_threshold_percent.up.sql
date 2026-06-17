-- 070_cadences_fail_threshold_percent.up.sql
--
-- ADR-043 amendment 2026-06-09 (строковые batch-поля), Cadence-recipe S3: рецепт
-- Cadence получает строковое поле `max_failures` ("N" абсолют / "N%" процент), как
-- у Voyage. Абсолют ложится в существующую колонку fail_threshold (066). Процент —
-- НОВАЯ колонка fail_threshold_percent.
--
-- Почему отдельная колонка (асимметрия с Voyage). У Voyage max_failures="N%"
-- резолвится в АБСОЛЮТНЫЙ fail_threshold ещё на create-time, потому что scope
-- прогона уже резолвнут (resolveMaxFailuresPercent после резолва target-а). У
-- Cadence scope при создании НЕИЗВЕСТЕН — target резолвится при каждом спавне
-- (Reaper-правило spawn_due_cadence), поэтому процент надо ХРАНИТЬ и резолвить в
-- абсолют на spawn-scope (len(resolved)) внутри cadence.BuildVoyage. Это полное
-- зеркало batch_percent (066): batch тоже хранится процентом-колонкой и резолвится
-- effectiveBatchSize на spawn-scope.
--
-- CHECK cadences_fail_threshold_percent_range — sane-bound [1, 100] (parity
-- cadences_batch_percent_range из 066). XOR fail_threshold ⇔ fail_threshold_percent
-- — на стороне handler-валидации (parity batch_size/batch_percent XOR: «оба NULL»
-- — валидное «без порога», CHECK его ложно не отвергает).
--
-- Additive nullable (forward-compat only-add, ADR-012): существующие строки
-- получают NULL, старый путь fail_threshold INT работает без изменений.

ALTER TABLE cadences
    ADD COLUMN fail_threshold_percent INT;

ALTER TABLE cadences
    ADD CONSTRAINT cadences_fail_threshold_percent_range
        CHECK (fail_threshold_percent IS NULL OR (fail_threshold_percent >= 1 AND fail_threshold_percent <= 100));

COMMENT ON COLUMN cadences.fail_threshold_percent IS
    'Порог провалов в процентах от spawn-scope (ADR-043 amendment 2026-06-09). XOR с fail_threshold. Хранится колонкой (scope Cadence неизвестен на создании) и резолвится в абсолют на len(resolved) в cadence.BuildVoyage — зеркало batch_percent.';
