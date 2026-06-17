-- 064_voyages_batch_mode.up.sql
--
-- ADR-043 amendment (2026-06-01) → S-W1: дискриминатор batch_mode для Voyage.
--
-- Новая nullable-колонка `voyages.batch_mode` (barrier | window). Forward-compat:
-- NULL трактуется как 'barrier' (текущее реализованное поведение) — существующие
-- прогоны без поля работают как раньше (ADR-012 forward-compat only-add). Резолв
-- NULL→barrier — на стороне оркестратора (voyage.ResolveBatchMode), не дефолт
-- колонки, чтобы «не задано» оставалось отличимым от явного 'barrier' в audit/UI.
--
--   * barrier — последовательные Leg-и (пачка batch_size), барьер между пачками.
--   * window  — скользящее окно (пул concurrency воркеров из общей очереди, без
--     барьеров между пачками; batch_size не используется, batch_index=0).
--
-- CHECK-инвариант: batch_mode IS NULL OR batch_mode IN ('barrier', 'window').

ALTER TABLE voyages
    ADD COLUMN batch_mode TEXT;

ALTER TABLE voyages
    ADD CONSTRAINT voyages_batch_mode_valid
        CHECK (batch_mode IS NULL OR batch_mode IN ('barrier', 'window'));

COMMENT ON COLUMN voyages.batch_mode IS
    'Режим батчинга (ADR-043 amendment 2026-06-01): barrier (Leg-и + барьер) | window (скользящее окно). NULL ⇒ barrier (forward-compat).';
