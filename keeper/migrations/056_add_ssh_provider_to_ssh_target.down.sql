-- 056_add_ssh_provider_to_ssh_target.down.sql
--
-- Rollback of the extended CHECK to the shape of migration 053. Existing rows with
-- `ssh_provider` set will pass the downgrade-CHECK only if the field
-- is absent from the payload - otherwise CHECK violation. Therefore the operator must
-- remove `ssh_provider` from all `souls.ssh_target` before running the down-migration
-- (or accept that rows with a provider will not pass the downgrade - this is intentional
-- protection: a down-migration must not silently lose data).

ALTER TABLE souls DROP CONSTRAINT IF EXISTS souls_ssh_target_shape;

ALTER TABLE souls ADD CONSTRAINT souls_ssh_target_shape CHECK (
    ssh_target IS NULL OR (
        jsonb_typeof(ssh_target->'ssh_port') = 'number' AND
        jsonb_typeof(ssh_target->'ssh_user') = 'string' AND
        jsonb_typeof(ssh_target->'soul_path') = 'string'
    )
);

COMMENT ON COLUMN souls.ssh_target IS
    'Per-host SSH credentials for push-flow (ADR-032 amendment 2026-05-26, S7-1): {ssh_port, ssh_user, soul_path}. NULL -> fallback to keeper.yml::push.targets[] under the push.allow_legacy_push_targets flag.';
