-- 009_create_soul_seeds.up.sql
--
-- Реестр выпущенных SoulSeed-сертификатов под docs/soul/identity.md.
-- На один SID — много seed-ов (история ротаций); один активный
-- (`status='active'`) одновременно — гарантировано partial unique index.
--
-- Enum `status` расширяется значением `orphaned` в миграции 017
-- (ADR-017 cascade от `core.cloud.provisioned destroyed`: хост удалён,
-- но revoked-семантику перетирать нельзя).
--
-- В БД не хранятся PEM, приватный ключ, отдельный публичный ключ — только
-- fingerprint (SHA-256 публичного ключа сертификата, hex). Главная защита —
-- приватный ключ CA в Vault PKI.
--
-- Push-хостам (`transport: ssh`) soul_seeds не используется (нет mTLS).
--
-- FK на souls(sid) — ON DELETE CASCADE (история seed-ов умирает с Soul-ом).

CREATE TABLE soul_seeds (
    seed_id            UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    sid                TEXT        NOT NULL,
    fingerprint        TEXT        NOT NULL,
    serial_number      TEXT        NOT NULL,
    issued_at          TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    expires_at         TIMESTAMPTZ NOT NULL,
    issued_by_kid      TEXT,
    status             TEXT        NOT NULL DEFAULT 'active',
    revocation_reason  TEXT,

    CONSTRAINT soul_seeds_sid_fk
        FOREIGN KEY (sid) REFERENCES souls (sid) ON DELETE CASCADE,
    CONSTRAINT soul_seeds_status_valid
        CHECK (status IN ('active', 'superseded', 'expired', 'revoked')),
    CONSTRAINT soul_seeds_fingerprint_format
        -- SHA-256 hex = 64 lower-hex chars.
        CHECK (fingerprint ~ '^[0-9a-f]{64}$'),
    CONSTRAINT soul_seeds_expires_after_issued
        CHECK (expires_at > issued_at)
);

-- Инвариант: на один sid — ровно один active-seed одновременно.
CREATE UNIQUE INDEX soul_seeds_active_by_sid_idx
    ON soul_seeds (sid)
    WHERE status = 'active';

-- mTLS handshake: lookup сертификата по fingerprint (для CRL-проверки
-- статуса). UNIQUE — fingerprint должен быть глобально уникален (collision
-- = криптокатастрофа; constraint держит инвариант явно).
CREATE UNIQUE INDEX soul_seeds_fingerprint_idx
    ON soul_seeds (fingerprint);

-- Serial-number unique (CA не должен выпускать два сертификата с одинаковым
-- серийником — invariant Vault PKI; держим constraint явно для defense-in-depth).
CREATE UNIQUE INDEX soul_seeds_serial_number_idx
    ON soul_seeds (serial_number);

-- Жнец: superseded/expired seed-ы старше max_age → DELETE.
CREATE INDEX soul_seeds_status_idx
    ON soul_seeds (status);

-- Soul-side ротация просит новый seed за `expires_at - 24h`; Жнец
-- двигает active → expired при достижении expires_at.
CREATE INDEX soul_seeds_expires_at_idx
    ON soul_seeds (expires_at);

COMMENT ON TABLE soul_seeds IS
    'История выпущенных SoulSeed-сертификатов (docs/soul/identity.md). Один active per sid.';
