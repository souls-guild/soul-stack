-- 042_create_oracle_circuit.up.sql
--
-- Oracle circuit-breaker (ADR-030(a), beacons S4 Part 1): auto-disable of a Decree
-- that has fallen into a loop (N triggers within a window). Per-decree fixed-window
-- counter - the second loop-prevention barrier after cooldown (that one dampens per-(decree,
-- subject), circuit-breaker - summed by the rule).
--
-- Storage - a separate `oracle_circuit` table (per-decree, fixed-window):
-- one row per Decree, a trigger counter for the current window. A column on
-- decrees doesn't work - the read-modify-write of the window must be atomic under
-- a row-lock on a single row (UPSERT ON CONFLICT DO UPDATE ... RETURNING),
-- cluster-safe without advisory locks (several Keeper instances increment
-- concurrently, the increment is not lost).
--
-- decree -> decrees(name) ON DELETE CASCADE: a counter without a Decree is meaningless;
-- re-enabling a "failed" Decree in the MVP = delete+recreate, and the cascade
-- atomically clears its window (a new Decree starts with a clean counter).
-- Self-cleans via cascade, WITHOUT a separate Reaper rule.
CREATE TABLE oracle_circuit (
    decree        TEXT        PRIMARY KEY,
    window_start  TIMESTAMPTZ NOT NULL,
    fire_count    INT         NOT NULL DEFAULT 0,

    CONSTRAINT oracle_circuit_decree_fk
        FOREIGN KEY (decree) REFERENCES decrees (name) ON DELETE CASCADE
);

COMMENT ON TABLE oracle_circuit IS
    'circuit-breaker-state Oracle per-decree (ADR-030(a), fixed-window). Atomic UPSERT increment, cluster-safe; decree ON DELETE CASCADE (re-enable = delete+recreate clears the window).';
