-- 017_soulseeds_status_orphaned.up.sql
--
-- ADR-017 cascade: adds the `orphaned` status to the `soul_seeds.status`
-- enum for seeds of a host that has been physically removed via
-- `core.cloud.provisioned destroyed`.
--
-- Why a separate status instead of `revoked`:
--   * `revoked` -- an operator explicitly revoked it (security incident, compromise).
--     Audit semantics: "the operator made a decision".
--   * `orphaned` -- the host stopped existing (cloud-destroy). Audit
--     semantics: "the VM's lifecycle ended".
--   Overwriting revoked with orphaned is not allowed -- revoked outranks orphaned
--   in priority (see the cascade condition WHERE status='active' in provisioned.go).
--
-- Semantics after the migration:
--   * `active`     -- the current issued seed, exactly one per SID.
--   * `superseded` -- replaced by rotation, the new seed is now active.
--   * `expired`    -- moved by the Reaper / Vault PKI after not_after.
--   * `revoked`    -- revoked by an operator (compromise).
--   * `orphaned`   -- host removed by cascade from `core.cloud.provisioned destroyed`.

ALTER TABLE soul_seeds
    DROP CONSTRAINT soul_seeds_status_valid;

ALTER TABLE soul_seeds
    ADD CONSTRAINT soul_seeds_status_valid
        CHECK (status IN ('active', 'superseded', 'expired', 'revoked', 'orphaned'));
