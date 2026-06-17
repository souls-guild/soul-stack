-- 054_create_push_providers.up.sql
--
-- ADR-032 amendment 2026-05-26 → S7-2: push_providers PG-table.
--
-- Long-term canon вместо keeper.yml::push.providers[] inline (pilot S6 / S7-1).
-- Хранит per-provider params SSH-плагинов push-flow: keeper при spawn-е плагина
-- сериализует `params` в JSON и инжектит в env-переменную `SOUL_SSH_<UPPER_SNAKE
-- (name)>_PARAMS` (ADR-020 amendment l, env-convention).
--
-- Hot-reload: изменения через CRUD-эндпоинты POST/PUT/DELETE /v1/push-providers
-- + Redis pub/sub `push-providers:changed` → SshDispatcher re-spawn плагина на
-- ближайшем RPC (spawn-on-change).
--
-- Priority: PG > keeper.yml (DB — source of truth, yml — fallback под флагом
-- push.allow_legacy_push_providers=true с 1-release WARN-deprecation; S7-2 PM-
-- decision, симметрично push.allow_legacy_push_targets из S7-1).
--
-- Sensitive params (PM-decision S7-2 #5): `vault_addr`/`role` могут идти plain
-- в jsonb; реальные секреты (`secret_id`/`token`/`password`/`private_key`)
-- ОБЯЗАНЫ быть vault-refs (`vault:<path>`) — валидация на service-слое
-- (pushprovider.Service.validateSensitive), не CHECK здесь (allow-list ключей
-- эволюционирует, БД-инвариант излишне жёсткий).
--
-- FK:
--   - created_by_aid → operators(aid) ON DELETE NO ACTION (NOT NULL): запись
--     PushProvider-а несёт инициатора всегда (управляется только Архонтом).
--   - updated_by_aid → operators(aid) ON DELETE SET NULL (nullable: пустой при
--     первой вставке).

CREATE TABLE push_providers (
    name            TEXT        PRIMARY KEY,
    params          JSONB       NOT NULL DEFAULT '{}'::jsonb,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    created_by_aid  TEXT        NOT NULL,
    updated_by_aid  TEXT,

    CONSTRAINT push_providers_name_format
        CHECK (name ~ '^[a-z][a-z0-9-]{0,62}$'),
    CONSTRAINT push_providers_created_by_aid_fk
        FOREIGN KEY (created_by_aid) REFERENCES operators (aid),
    CONSTRAINT push_providers_updated_by_aid_fk
        FOREIGN KEY (updated_by_aid) REFERENCES operators (aid) ON DELETE SET NULL
);

-- Свежие изменения сверху (триаж): UI «recently changed providers».
CREATE INDEX push_providers_updated_idx ON push_providers (updated_at DESC);

COMMENT ON TABLE push_providers IS
    'Per-provider params SSH-плагинов push-flow (ADR-032 amendment 2026-05-26, S7-2). Long-term canon вместо keeper.yml::push.providers[]. Hot-reload через Redis pub/sub push-providers:changed.';
