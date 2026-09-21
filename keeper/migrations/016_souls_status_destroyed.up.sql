-- 016_souls_status_destroyed.up.sql
--
-- ADR-017 cascade: adds the terminal status `destroyed` to the enum
-- `souls.status`. Used by the keeper-side core module
-- `core.cloud.provisioned destroyed` after a successful CloudDriver.Destroy
-- for a VM released by the same pairing (cloud-create -> cloud-destroy).
--
-- Status semantics after the migration:
--   * `pending` - the operator issued a bootstrap token, the Soul has not connected yet.
--   * `connected` - the stream is alive, Keeper holds the lease in Redis.
--   * `disconnected` - the stream is closed, the lease expired (the Soul may return).
--   * `revoked` - the operator revoked it, new connections are rejected.
--   * `expired` - the Reaper moved `pending` here after the bootstrap token TTL.
--   * `destroyed` - the Soul-side host was physically removed via
--     `core.cloud.provisioned destroyed`. Terminal (no transitions out).
--     Forensic state: NOT included in the default `purge_souls.statuses` set,
--     so the row survives incident review. The operator can delete it manually.

ALTER TABLE souls
    DROP CONSTRAINT souls_status_valid;

ALTER TABLE souls
    ADD CONSTRAINT souls_status_valid
        CHECK (status IN ('pending', 'connected', 'disconnected', 'revoked', 'expired', 'destroyed'));
