-- 036_incarnation_status_destroy_failed.down.sql
--
-- Rollback of the `incarnation.status` enum extension. Before DROPping the old CHECK,
-- make sure the table has no rows with status `destroy_failed` - otherwise
-- ADD CONSTRAINT will fail. This down migration is not fail-safe (forward-only per
-- ADR-019): down is only intended for a fresh DB where there are no
-- `destroy_failed` rows yet. Returns the CHECK to the form from 031 (with `destroying`, without `destroy_failed`).

ALTER TABLE incarnation
    DROP CONSTRAINT incarnation_status_valid;

ALTER TABLE incarnation
    ADD CONSTRAINT incarnation_status_valid
        CHECK (status IN ('ready', 'applying', 'error_locked', 'migration_failed', 'destroying'));
