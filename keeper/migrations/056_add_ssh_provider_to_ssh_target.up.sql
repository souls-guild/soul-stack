-- 056_add_ssh_provider_to_ssh_target.up.sql
--
-- ADR-032 amendment 2026-05-27 → P2 Multi-provider routing (W-1 schema).
--
-- Расширяет shape-CHECK `souls_ssh_target_shape` (миграция 053): к трём
-- обязательным полям `{ssh_port, ssh_user, soul_path}` добавляется optional
-- `ssh_provider` — per-SID explicit-выбор SshProvider-плагина. Тот же
-- regex-формат, что у `push_providers.name` (миграция 054): kebab-case,
-- env-var-name-safe.
--
-- Selector 3-tier R1 (architect-decisions 2026-05-27):
--   1. souls.ssh_target.ssh_provider  (per-SID explicit; здесь);
--   2. push.coven_default_providers   (per-coven; keeper.yml);
--   3. push.cluster_default_provider  (cluster fallback; keeper.yml).
--
-- Все три пустые → ErrProviderNotRouted → fail per-host (audit-summary,
-- БЕЗ provider-chain — security-инвариант, auth-perimeter разных providers
-- разный).
--
-- Совместимость: старые ssh_target-записи (без `ssh_provider`) проходят
-- расширенный CHECK без изменений — optional-поле проверяется только при
-- наличии. NULL/missing → routing идёт по Level 2/3.

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
    'Per-host SSH-реквизиты push-flow (ADR-032 amendment 2026-05-26, S7-1 + 2026-05-27 P2): {ssh_port, ssh_user, soul_path, ssh_provider?}. Optional `ssh_provider` — Level 1 multi-provider routing (3-tier R1); NULL → fallback на coven_default → cluster_default → fail per-host.';
