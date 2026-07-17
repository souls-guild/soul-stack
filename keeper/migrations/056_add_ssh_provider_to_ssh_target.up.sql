-- 056_add_ssh_provider_to_ssh_target.up.sql
--
-- ADR-032 amendment 2026-05-27 -> P2 Multi-provider routing (W-1 schema).
--
-- Extends the shape CHECK `souls_ssh_target_shape` (migration 053): alongside
-- the three required fields `{ssh_port, ssh_user, soul_path}`, an optional
-- `ssh_provider` is added - a per-SID explicit choice of the SshProvider
-- plugin. Same regex format as `push_providers.name` (migration 054):
-- kebab-case, env-var-name-safe.
--
-- Selector 3-tier R1 (architect-decisions 2026-05-27):
--   1. souls.ssh_target.ssh_provider  (per-SID explicit; here);
--   2. push.coven_default_providers   (per-coven; keeper.yml);
--   3. push.cluster_default_provider  (cluster fallback; keeper.yml).
--
-- If all three are empty -> ErrProviderNotRouted -> fail per-host
-- (audit-summary, WITHOUT a provider chain - a security invariant, since the
-- auth perimeter differs between providers).
--
-- Compatibility: old ssh_target rows (without `ssh_provider`) pass the
-- extended CHECK unchanged - the optional field is validated only when
-- present. NULL/missing -> routing falls through to Level 2/3.

ALTER TABLE souls DROP CONSTRAINT IF EXISTS souls_ssh_target_shape;

ALTER TABLE souls ADD CONSTRAINT souls_ssh_target_shape CHECK (
    ssh_target IS NULL OR (
        jsonb_typeof(ssh_target->'ssh_port') = 'number' AND
        jsonb_typeof(ssh_target->'ssh_user') = 'string' AND
        jsonb_typeof(ssh_target->'soul_path') = 'string' AND
        (
            NOT (ssh_target ? 'ssh_provider') OR
            (
                jsonb_typeof(ssh_target->'ssh_provider') = 'string' AND
                (ssh_target->>'ssh_provider') ~ '^[a-z][a-z0-9-]{0,62}$'
            )
        )
    )
);

COMMENT ON COLUMN souls.ssh_target IS
    'Per-host SSH credentials for the push flow (ADR-032 amendment 2026-05-26, S7-1 + 2026-05-27 P2): {ssh_port, ssh_user, soul_path, ssh_provider?}. Optional `ssh_provider` - Level 1 multi-provider routing (3-tier R1); NULL -> fallback to coven_default -> cluster_default -> fail per-host.';
