-- 031_incarnation_status_destroying.up.sql
--
-- S-D1 (incarnation.destroy): adds the `destroying` status to the
-- `incarnation.status` enum. An operator initiates destroy -> the row moves to
-- `destroying` -> teardown starts (scenario `destroy`, S-D2) -> on success
-- the row is DELETEd (S-D3).
--
-- Status semantics after the migration:
--   * `ready`            -- incarnation is operational, runs are allowed.
--   * `applying`         -- a scenario run is in progress (locks further operations).
--   * `error_locked`     -- the scenario failed partway, needs unlock.
--   * `migration_failed` -- a state_schema migration failed, needs unlock (ADR-019).
--   * `destroying`       -- destroy initiated: teardown is running, followed by
--     a row DELETE. NOT terminal for the row (on success the row disappears,
--     on teardown failure -> error_locked in S-D2). From this status run /
--     upgrade / a repeat destroy are all rejected.

ALTER TABLE incarnation
    DROP CONSTRAINT incarnation_status_valid;

ALTER TABLE incarnation
    ADD CONSTRAINT incarnation_status_valid
        CHECK (status IN ('ready', 'applying', 'error_locked', 'migration_failed', 'destroying'));
