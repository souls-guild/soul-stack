-- 008_create_bootstrap_tokens.up.sql
--
-- Реестр одноразовых SoulSeed-токенов под docs/soul/onboarding.md.
-- На один SID — один активный (`used_at IS NULL`) токен одновременно
-- (partial unique index). После использования запись остаётся в таблице
-- для аудита; Жнец позже подбирает по правилу `purge_used_tokens` с
-- `max_age: 90d` от `used_at`.
--
-- Сам plain-токен в БД не хранится — только `token_hash` (SHA-256, hex,
-- без соли — токен сам high-entropy). Plain живёт только в выводе
-- bootstrap-API оператору и в файле на хосте Soul-а.
--
-- Push-хостам (`transport: ssh`) записи в bootstrap_tokens не создаются.
--
-- FK на souls(sid) — ON DELETE CASCADE: удаление Soul-а удаляет связанные
-- токены (история токенов умирает с Soul-ом, как и в state_history vs
-- incarnation).

CREATE TABLE bootstrap_tokens (
    token_id        UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    sid             TEXT        NOT NULL,
    token_hash      TEXT        NOT NULL,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    expires_at      TIMESTAMPTZ NOT NULL,
    used_at         TIMESTAMPTZ,
    used_by_kid     TEXT,
    created_by_aid  TEXT,

    CONSTRAINT bootstrap_tokens_sid_fk
        FOREIGN KEY (sid) REFERENCES souls (sid) ON DELETE CASCADE,
    CONSTRAINT bootstrap_tokens_created_by_aid_fk
        FOREIGN KEY (created_by_aid) REFERENCES operators (aid) ON DELETE SET NULL,
    CONSTRAINT bootstrap_tokens_expires_after_created
        CHECK (expires_at > created_at),
    CONSTRAINT bootstrap_tokens_token_hash_format
        -- SHA-256 hex = 64 lower-hex chars.
        CHECK (token_hash ~ '^[0-9a-f]{64}$')
);

-- Инвариант: на один sid — один активный (неиспользованный) токен.
CREATE UNIQUE INDEX bootstrap_tokens_active_by_sid_idx
    ON bootstrap_tokens (sid)
    WHERE used_at IS NULL;

-- Lookup по token_hash при предъявлении (UNIQUE — гарантия, что один
-- хеш не висит на разных SID; high-entropy исключает коллизию де-факто,
-- но constraint держит инвариант явно).
CREATE UNIQUE INDEX bootstrap_tokens_token_hash_idx
    ON bootstrap_tokens (token_hash);

-- Жнец: использованные токены старше max_age → DELETE.
CREATE INDEX bootstrap_tokens_used_at_idx
    ON bootstrap_tokens (used_at)
    WHERE used_at IS NOT NULL;

-- Жнец: pending токены старше expires_at → DELETE + souls.status = expired.
CREATE INDEX bootstrap_tokens_expires_at_idx
    ON bootstrap_tokens (expires_at)
    WHERE used_at IS NULL;

COMMENT ON TABLE bootstrap_tokens IS
    'Одноразовые SoulSeed-токены (docs/soul/onboarding.md). Активный = used_at IS NULL.';
