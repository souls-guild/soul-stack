-- 042_create_oracle_circuit.up.sql
--
-- circuit-breaker Oracle (ADR-030(a), beacons S4 Part 1): авто-disable Decree-а,
-- сорвавшегося в петлю (N срабатываний за окно). Per-decree fixed-window
-- счётчик — второй барьер loop-prevention после cooldown (тот гасит per-(decree,
-- subject), circuit-breaker — суммарно по правилу).
--
-- Хранилище — отдельная таблица `oracle_circuit` (per-decree, fixed-window):
-- одна строка на Decree, счётчик срабатываний в текущем окне. Колонка на
-- decrees не годится — read-modify-write окна должен быть атомарным под
-- row-lock-ом одной строки (UPSERT ON CONFLICT DO UPDATE … RETURNING),
-- cluster-safe без advisory-lock-ов (несколько Keeper-инстансов инкрементируют
-- конкурентно, инкремент не теряется).
--
-- decree → decrees(name) ON DELETE CASCADE: счётчик без Decree-а бессмыслен;
-- re-enable «провалившегося» Decree-а в MVP = delete+recreate, и каскад
-- атомарно чистит его окно (новый Decree стартует с чистого счётчика).
-- Само-чистится каскадом, БЕЗ отдельного Reaper-правила.
CREATE TABLE oracle_circuit (
    decree        TEXT        PRIMARY KEY,
    window_start  TIMESTAMPTZ NOT NULL,
    fire_count    INT         NOT NULL DEFAULT 0,

    CONSTRAINT oracle_circuit_decree_fk
        FOREIGN KEY (decree) REFERENCES decrees (name) ON DELETE CASCADE
);

COMMENT ON TABLE oracle_circuit IS
    'circuit-breaker-state Oracle per-decree (ADR-030(a), fixed-window). Атомарный UPSERT-инкремент cluster-safe; decree ON DELETE CASCADE (re-enable = delete+recreate чистит окно).';
