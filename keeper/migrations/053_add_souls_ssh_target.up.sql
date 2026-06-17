-- 053_add_souls_ssh_target.up.sql
--
-- ADR-032 amendment 2026-05-26 → S7-1: souls.ssh_target jsonb.
--
-- Long-term canon вместо keeper.yml::push.targets[] inline (pilot S6).
-- Хранит per-host SSH-реквизиты push-flow прямо в реестре souls — Keeper
-- получает их по primary-key lookup (без вторичного индекса: основной путь
-- SshDispatcher.SendApply резолвит target по SID, который уже PK таблицы).
--
-- Hot-reload: изменения через PUT /v1/souls/{sid}/ssh-target — UPDATE по PK,
-- без рестарта Keeper-а; PGFallbackTargetResolver видит свежую запись на
-- ближайшем resolve.
--
-- Priority: PG > keeper.yml (DB — source of truth, yml — fallback под флагом
-- push.allow_legacy_push_targets=true с 1-release WARN-deprecation; S7-1 PM-
-- decision).
--
-- Shape (валидируется CHECK ниже): { ssh_port: integer, ssh_user: text,
-- soul_path: text }. NULL-семантика поля целиком: запись Soul-а не имеет
-- настроенного target-а (fallback на keeper.yml либо ErrTargetNotConfigured).
-- Дефолты опущенных полей (port 22 / user root / soul-path /usr/local/bin/soul)
-- резолвятся Go-стороной в PGFallbackTargetResolver: schema хранит ТОЛЬКО
-- то, что задал оператор.

ALTER TABLE souls ADD COLUMN IF NOT EXISTS ssh_target jsonb;

-- Type-guard на shape: если ssh_target не NULL, все три поля обязаны быть
-- проставлены и иметь типы integer/text/text. Это defense-in-depth: handler
-- уже валидирует request-body, но constraint защищает от прямой записи в БД
-- (миграции, MCP-tool, отладка).
ALTER TABLE souls ADD CONSTRAINT souls_ssh_target_shape CHECK (
    ssh_target IS NULL OR (
        jsonb_typeof(ssh_target->'ssh_port') = 'number' AND
        jsonb_typeof(ssh_target->'ssh_user') = 'string' AND
        jsonb_typeof(ssh_target->'soul_path') = 'string'
    )
);

COMMENT ON COLUMN souls.ssh_target IS
    'Per-host SSH-реквизиты push-flow (ADR-032 amendment 2026-05-26, S7-1): {ssh_port, ssh_user, soul_path}. NULL → fallback на keeper.yml::push.targets[] под флагом push.allow_legacy_push_targets.';
