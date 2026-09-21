-- 068_cadences_interval_floor.up.sql
--
-- ADR-046 Pass B (floor limit, amendment 2026-06-07): minimum period of an
-- interval-Cadence is 30s. For reactive triggering faster than 30s, use Beacons
-- (Vigil/Oracle, ADR-030), not Cadence.
--
-- Three parts:
--   1. Partial index cadences_enabled_interval_idx on (interval_seconds)
--      WHERE enabled - for the adaptive MIN query of the Conductor (ADR-048 "Adaptive
--      interval"): `SELECT MIN(interval_seconds), bool_or(...) FROM cadences
--      WHERE enabled`. Before this, the aggregate went via a seq-scan over the whole
--      table; the partial index over enabled rows returns MIN without a full scan.
--      Differs from cadences_due_scan_idx (066) - that one is on (next_run_at) for the
--      due selection, this one is on (interval_seconds) for the MIN aggregate.
--   2. CHECK cadences_interval_seconds_floor: interval_seconds >= 30 (a separate
--      name, does NOT override cadences_interval_seconds_positive from 066 - positive
--      remains the base sane-bound, floor tightens the lower boundary).
--      cron-Cadence (interval_seconds NULL) is not caught by the CHECK (NULL OR ...).
--   3. Pre-flight data-guard BEFORE ADD CONSTRAINT: if the table already has rows
--      with interval_seconds < 30 (e.g. a dev stand with a 10s cadence) - RAISE
--      EXCEPTION with a clear message. NOT a silent UPDATE: silently changing the
--      period of someone else's schedule is not acceptable (the operator decides
--      themselves - raise it to 30s, switch to cron, or delete). fail-fast: the
--      migration stops, no state changes (the entire up migration runs in one
--      migrator transaction).

DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM cadences WHERE interval_seconds < 30) THEN
        RAISE EXCEPTION 'found cadences with interval_seconds < 30 -- fix/delete them before migrating (minimum 30s; for sub-30s use Beacons)';
    END IF;
END $$;

ALTER TABLE cadences
    ADD CONSTRAINT cadences_interval_seconds_floor
        CHECK (interval_seconds IS NULL OR interval_seconds >= 30);

CREATE INDEX cadences_enabled_interval_idx
    ON cadences (interval_seconds)
    WHERE enabled;

COMMENT ON CONSTRAINT cadences_interval_seconds_floor ON cadences IS
    'Floor limit on the interval-Cadence period (ADR-046 Pass B): interval_seconds >= 30s. For faster-than-30s reactions use Beacons (ADR-030). cron-Cadence (NULL) is not affected.';
