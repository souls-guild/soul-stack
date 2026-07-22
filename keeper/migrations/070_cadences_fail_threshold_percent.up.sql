-- 070_cadences_fail_threshold_percent.up.sql
--
-- ADR-043 amendment 2026-06-09 (string batch fields), Cadence recipe S3: the Cadence
-- recipe gets a string field `max_failures` ("N" absolute / "N%" percent), just like
-- Voyage. The absolute form lands in the existing fail_threshold column (066). The
-- percent form gets a NEW fail_threshold_percent column.
--
-- Why a separate column (asymmetry with Voyage). For Voyage, max_failures="N%"
-- is resolved to an ABSOLUTE fail_threshold already at create-time, because the
-- run's scope is already resolved (resolveMaxFailuresPercent after the target is
-- resolved). For Cadence the scope is UNKNOWN at creation - the target is resolved
-- on every spawn (the Reaper rule spawn_due_cadence), so the percent has to be
-- STORED and resolved to an absolute value against the spawn-scope (len(resolved))
-- inside cadence.BuildVoyage. This exactly mirrors batch_percent (066): batch is
-- also stored as a percent column and resolved to effectiveBatchSize at spawn-scope.
--
-- CHECK cadences_fail_threshold_percent_range - a sane bound [1, 100] (parity with
-- cadences_batch_percent_range from 066). XOR fail_threshold ⇔ fail_threshold_percent
-- is enforced on the handler-validation side (parity with the batch_size/batch_percent
-- XOR: "both NULL" is a valid "no threshold", the CHECK doesn't falsely reject it).
--
-- Additive nullable (forward-compat only-add, ADR-012): existing rows
-- get NULL, the old fail_threshold INT path works unchanged.

ALTER TABLE cadences
    ADD COLUMN fail_threshold_percent INT;

ALTER TABLE cadences
    ADD CONSTRAINT cadences_fail_threshold_percent_range
        CHECK (fail_threshold_percent IS NULL OR (fail_threshold_percent >= 1 AND fail_threshold_percent <= 100));

COMMENT ON COLUMN cadences.fail_threshold_percent IS
    'Failure threshold as a percent of spawn-scope (ADR-043 amendment 2026-06-09). XOR with fail_threshold. Stored as a column (Cadence scope is unknown at creation) and resolved to an absolute value against len(resolved) in cadence.BuildVoyage - mirrors batch_percent.';
