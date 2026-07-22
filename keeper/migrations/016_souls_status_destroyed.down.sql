-- 016_souls_status_destroyed.down.sql
--
-- Rollback of the `souls.status` enum extension. Before DROPping the old CHECK,
-- make sure there are no rows with status `destroyed` in the table -
-- otherwise ADD CONSTRAINT will fail. This is not fail-safe in the down-migration
-- (forward-only per ADR-019), so there is no cast to `revoked`: down
-- is only expected on a fresh DB, where there are no `destroyed` rows yet.

ALTER TABLE souls
    DROP CONSTRAINT souls_status_valid;

ALTER TABLE souls
    ADD CONSTRAINT souls_status_valid
        CHECK (status IN ('pending', 'connected', 'disconnected', 'revoked', 'expired'));
