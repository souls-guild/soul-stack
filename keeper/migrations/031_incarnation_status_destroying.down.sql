-- 031_incarnation_status_destroying.down.sql
--
-- Rollback of the `incarnation.status` enum extension. Before dropping the old CHECK
-- we need to make sure there are no rows with status `destroying` in the table -
-- otherwise ADD CONSTRAINT would fail. This down migration is not fail-safe for that
-- (forward-only per ADR-019): down is only expected to run on a fresh DB where there
-- are no `destroying` rows yet.

ALTER TABLE incarnation
    DROP CONSTRAINT incarnation_status_valid;

ALTER TABLE incarnation
    ADD CONSTRAINT incarnation_status_valid
        CHECK (status IN ('ready', 'applying', 'error_locked', 'migration_failed'));
