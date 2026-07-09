-- 092_create_warrant.up.sql
--
-- Реестр Warrant — выпущенные СЕРВИСНЫЕ TLS-серты инкарнаций (не identity!)
-- под cert-rotation Вар1 (Keeper-центр). Ось скана Reaper-правила
-- `rotate_due_certs` — `not_after`: серты, чей срок истекает в пределах
-- порога, ротируются централизованно (Keeper генерит новый keypair+CSR,
-- SignCSR через Vault PKI, WriteKV в Vault, спавн Voyage операционного сценария
-- rotate_tls).
--
-- ★ ОТЛИЧИЕ ОТ soul_seeds (009): soul_seeds — IDENTITY-серты Soul-агентов
-- (mTLS-пара, приватник НИКОГДА не покидает хост, в БД только fingerprint).
-- Warrant — СЕРВИСНЫЕ серты (напр. серверный TLS Redis), где приватник
-- ГЕНЕРИТСЯ Keeper-ом централизованно и проходит через Keeper → Vault (R2,
-- осознанное исключение из инварианта identity-модели: сервисный серт ≠
-- identity-серт, и он уже лежит в Vault для ручного rotate_tls). Приватник в
-- БД НЕ хранится — только vault_ref + fingerprint + serial. Решение
-- зафиксировано амендментом ADR-017 (cert-rotation Warrant).
--
-- На одну (incarnation, kind) — много warrant-строк (история ротаций); один
-- active-серт каждого kind одновременно — гарантирует partial unique index
-- (симметрия soul_seeds_active_by_sid_idx).
--
-- FK на incarnation(name) — ON DELETE CASCADE (история сертов умирает с
-- инкарнацией; incarnation.PK = name, TEXT, ADR-009).

CREATE TABLE warrant (
    cert_id                   UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    incarnation_id            TEXT        NOT NULL,
    kind                      TEXT        NOT NULL,
    vault_ref                 TEXT        NOT NULL,
    serial_number             TEXT        NOT NULL,
    fingerprint               TEXT        NOT NULL,
    not_after                 TIMESTAMPTZ NOT NULL,
    issued_at                 TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    pki_mount                 TEXT,
    pki_role                  TEXT,
    status                    TEXT        NOT NULL DEFAULT 'active',
    issued_by_kid             TEXT,
    last_rotation_voyage_id   TEXT,
    auto_rotate               BOOLEAN     NOT NULL DEFAULT true,
    rotate_threshold_override INTERVAL,

    CONSTRAINT warrant_incarnation_fk
        FOREIGN KEY (incarnation_id) REFERENCES incarnation (name) ON DELETE CASCADE,
    CONSTRAINT warrant_kind_valid
        CHECK (kind IN ('cert', 'key', 'ca')),
    CONSTRAINT warrant_status_valid
        CHECK (status IN ('active', 'superseded', 'expired', 'rotating', 'failed')),
    CONSTRAINT warrant_fingerprint_format
        -- SHA-256 hex = 64 lower-hex chars (симметрия soul_seeds_fingerprint_format).
        CHECK (fingerprint ~ '^[0-9a-f]{64}$')
);

-- Инвариант: на одну (incarnation, kind) — ровно один active-серт одновременно
-- (симметрия soul_seeds_active_by_sid_idx). Ротация: старую строку в
-- superseded, новую в active — оба в одной tx, не нарушая partial-unique.
CREATE UNIQUE INDEX warrant_active_by_incarnation_kind_idx
    ON warrant (incarnation_id, kind)
    WHERE status = 'active';

-- Reaper `rotate_due_certs`: скан истекающих active-сертов по not_after
-- (`not_after < NOW() + threshold`, эффективный порог с jitter в предикате).
CREATE INDEX warrant_not_after_idx
    ON warrant (not_after);

-- `purge_old_certs` (R4): retention superseded/expired-строк по возрасту.
CREATE INDEX warrant_status_idx
    ON warrant (status);

COMMENT ON TABLE warrant IS
    'Реестр Warrant — сервисные TLS-серты инкарнаций (cert-rotation Вар1, Keeper-центр). Один active per (incarnation, kind). Приватник генерится Keeper-ом (R2), в БД только vault_ref+fingerprint+serial. Не путать с soul_seeds (identity-серты Soul, приватник не покидает хост).';
