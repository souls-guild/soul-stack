-- 065_voyages_batch_strategies.up.sql
--
-- ADR-043 amendment (2026-06-01) → S-W3/S-W4: батч-стратегии Salt-уровня для
-- Voyage. Четыре additive nullable-колонки в `voyages` (forward-compat: прогоны
-- без полей работают как раньше, ADR-012 forward-compat only-add). Резолв NULL→
-- дефолт — на стороне handler/оркестратора, не дефолтом колонки, чтобы «не
-- задано» оставалось отличимым от явного значения в audit/UI (parity batch_mode
-- миграции 064).
--
--   * batch_percent — размер пачки как % от резолвнутого scope (parity Salt
--     `-b 25%`), ВЗАИМОИСКЛЮЧАЮЩИЙ с batch_size. Хранится для audit/UI; handler
--     при наличии вычисляет эффективный batch_size = ceil(scope * pct/100) и
--     пишет его в batch_size (резолвер/оркестратор видят обычный batch_size).
--     Осмыслен только для batch_mode=barrier (в window не используется). 1..100.
--   * fail_threshold — порог абсолютного числа провалов: накоплено N провалов →
--     прогон останавливается (новые Leg-и / единицы окна не стартуют). on_failure
--     =abort ≡ fail_threshold:1; on_failure=continue ≡ NULL (без порога). N>1 —
--     промежуточная толерантность. Работает в обоих batch_mode. > 0.
--   * inter_unit_interval — per-unit пауза в batch_mode=window перед спавном
--     следующей единицы окна (parity inter_batch_interval между Leg-ами в
--     barrier). Тип INTERVAL (как inter_batch_interval). Применим только к window.
--   * require_alive — presence-фильтр: при true scope-резолв отсекает Soul-ы без
--     живого presence-lease (SoulLeaseChecker, ADR-006). Снимок после фильтра
--     фиксируется в target_resolved (snapshot-инвариант не ослаблен). NULL⇒false.
--
-- CHECK-инварианты:
--   * batch_percent IS NULL OR (batch_percent >= 1 AND batch_percent <= 100).
--   * fail_threshold IS NULL OR fail_threshold > 0.
--   * Взаимоисключение batch_size / batch_percent — handler-инвариант (ровно один
--     → 422), НЕ CHECK: «оба NULL» (весь прогон одним Leg) тоже валиден, что
--     XOR-CHECK выразить не может без ложных отказов.

ALTER TABLE voyages
    ADD COLUMN batch_percent       INT,
    ADD COLUMN fail_threshold      INT,
    ADD COLUMN inter_unit_interval INTERVAL,
    ADD COLUMN require_alive       BOOLEAN;

ALTER TABLE voyages
    ADD CONSTRAINT voyages_batch_percent_range
        CHECK (batch_percent IS NULL OR (batch_percent >= 1 AND batch_percent <= 100));

ALTER TABLE voyages
    ADD CONSTRAINT voyages_fail_threshold_positive
        CHECK (fail_threshold IS NULL OR fail_threshold > 0);

COMMENT ON COLUMN voyages.batch_percent IS
    'Размер пачки как % от scope (ADR-043 amendment 2026-06-01), XOR с batch_size. 1..100; осмыслен для batch_mode=barrier. NULL ⇒ задан batch_size (или весь прогон одним Leg).';
COMMENT ON COLUMN voyages.fail_threshold IS
    'Порог абсолютного числа провалов → стоп (ADR-043 amendment 2026-06-01). abort ≡ 1; continue ≡ NULL. Работает в обоих batch_mode.';
COMMENT ON COLUMN voyages.inter_unit_interval IS
    'Per-unit пауза в batch_mode=window перед спавном следующей единицы (ADR-043 amendment 2026-06-01, parity inter_batch_interval). NULL ⇒ без паузы.';
COMMENT ON COLUMN voyages.require_alive IS
    'Presence-фильтр живых Soul-ов на резолве scope (ADR-043 amendment 2026-06-01, SoulLeaseChecker). NULL ⇒ false (без фильтра).';
