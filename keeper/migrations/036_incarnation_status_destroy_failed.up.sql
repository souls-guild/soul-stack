-- 036_incarnation_status_destroy_failed.up.sql
--
-- S-D2a (incarnation.destroy): adds the terminal status `destroy_failed`
-- to the enum `incarnation.status`. Semantics: teardown (scenario `destroy`, S-D2b)
-- failed on the hosts: the instance is NOT deleted, state remains last known-good,
-- operator intervention is required (retry destroy / force-teardown / unlock to ready).
--
-- The transition to `destroy_failed` itself is set in S-D2b/S-D3 (teardown outcome);
-- this migration only introduces the allowed enum value. Extending the CHECK via
-- drop+recreate is the pattern from 016/017/031 (status is a CHECK constraint, not a
-- PG enum, so it is reversible).
--
-- Status semantics after this migration:
--   * `ready`            - incarnation is in a working state, runs are allowed.
--   * `applying`         - a scenario run is in progress (locks further operations).
--   * `error_locked`     - the scenario failed partway, needs unlock.
--   * `migration_failed` - a state_schema migration failed, needs unlock (ADR-019).
--   * `destroying`       - destroy was initiated: teardown is in progress, followed by
--     DELETE of the row (S-D1/S-D2b/S-D3).
--   * `destroy_failed`   - teardown failed: the instance is NOT deleted, operator
--     intervention is required. Terminal for the row until an explicit operator action.

ALTER TABLE incarnation
    DROP CONSTRAINT incarnation_status_valid;

ALTER TABLE incarnation
    ADD CONSTRAINT incarnation_status_valid
        CHECK (status IN ('ready', 'applying', 'error_locked', 'migration_failed', 'destroying', 'destroy_failed'));
