-- 047_incarnation_status_drift.up.sql
--
-- ADR-031 (Scry): adds the informational status `drift` to the enum
-- `incarnation.status`. Semantics - a Scry check found a discrepancy between
-- the declared state and the actual state on at least one host; the status does NOT
-- block remediation (unlike `error_locked`/`destroy_failed`),
-- remediating drift = a normal apply from `drift` -> `ready`.
--
-- The transition into `drift` itself is set by the check-drift-handler after successfully
-- building a `DriftReport` (if there are discrepancies); this migration only introduces
-- the allowed enum value. Extending the CHECK via drop+recreate is the pattern from
-- 016/017/031/036 (status is a CHECK constraint, not a PG enum, so it's reversible).
--
-- Status semantics after the migration:
--   * `ready`            - incarnation is in a working state, runs are allowed.
--   * `applying`         - a scenario run is in progress (locks further operations).
--   * `error_locked`     - the scenario failed partway through, needs unlock.
--   * `migration_failed` - the state_schema migration failed, needs unlock (ADR-019).
--   * `destroying`       - destroy has been initiated: teardown is in progress, followed by
--     row DELETE (S-D1/S-D2b/S-D3).
--   * `destroy_failed`   - teardown failed: the instance was NOT deleted, needs operator
--     intervention. Terminal for the row until an explicit operator action.
--   * `drift`            - a Scry check found a discrepancy between the actual state
--     and the declaration (ADR-031, informational, NOT blocking).

ALTER TABLE incarnation
    DROP CONSTRAINT incarnation_status_valid;

ALTER TABLE incarnation
    ADD CONSTRAINT incarnation_status_valid
        CHECK (status IN ('ready', 'applying', 'error_locked', 'migration_failed', 'destroying', 'destroy_failed', 'drift'));
