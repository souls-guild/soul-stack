-- 053_add_souls_ssh_target.up.sql
--
-- ADR-032 amendment 2026-05-26 -> S7-1: souls.ssh_target jsonb.
--
-- Long-term canon replacing keeper.yml::push.targets[] inline (pilot S6).
-- Stores per-host SSH credentials for the push-flow right in the souls
-- registry - the Keeper fetches them via a primary-key lookup (no
-- secondary index needed: the main path, SshDispatcher.SendApply, resolves
-- the target by SID, which is already the table's PK).
--
-- Hot-reload: changes via PUT /v1/souls/{sid}/ssh-target - UPDATE by PK,
-- no Keeper restart needed; PGFallbackTargetResolver sees the fresh
-- record on the next resolve.
--
-- Priority: PG > keeper.yml (DB is the source of truth, yml is a fallback
-- under the flag push.allow_legacy_push_targets=true with a 1-release
-- WARN-deprecation; S7-1 PM decision).
--
-- Shape (validated by the CHECK below): { ssh_port: integer, ssh_user: text,
-- soul_path: text }. NULL semantics for the whole field: the Soul's record
-- has no configured target (fallback to keeper.yml or ErrTargetNotConfigured).
-- Defaults for omitted fields (port 22 / user root / soul-path
-- /usr/local/bin/soul) are resolved on the Go side in
-- PGFallbackTargetResolver: the schema stores ONLY what the operator set.

ALTER TABLE souls ADD COLUMN IF NOT EXISTS ssh_target jsonb;

-- Shape type-guard: if ssh_target is not NULL, all three fields must be
-- present and typed integer/text/text. This is defense-in-depth: the
-- handler already validates the request body, but the constraint guards
-- against direct writes to the DB (migrations, MCP tool, debugging).
ALTER TABLE souls ADD CONSTRAINT souls_ssh_target_shape CHECK (
    ssh_target IS NULL OR (
        jsonb_typeof(ssh_target->'ssh_port') = 'number' AND
        jsonb_typeof(ssh_target->'ssh_user') = 'string' AND
        jsonb_typeof(ssh_target->'soul_path') = 'string'
    )
);

COMMENT ON COLUMN souls.ssh_target IS
    'Per-host SSH credentials for the push-flow (ADR-032 amendment 2026-05-26, S7-1): {ssh_port, ssh_user, soul_path}. NULL -> fallback to keeper.yml::push.targets[] under the push.allow_legacy_push_targets flag.';
