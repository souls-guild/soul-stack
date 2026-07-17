-- 047_incarnation_status_drift.down.sql
--
-- Revert of ADR-031 (Scry): removes `drift` from the `incarnation.status` enum. The down
-- migration assumes there are no rows with status='drift' (a down migration is only
-- invoked after manual verification/recovery).

ALTER TABLE incarnation
    DROP CONSTRAINT incarnation_status_valid;

ALTER TABLE incarnation
    ADD CONSTRAINT incarnation_status_valid
        CHECK (status IN ('ready', 'applying', 'error_locked', 'migration_failed', 'destroying', 'destroy_failed'));
