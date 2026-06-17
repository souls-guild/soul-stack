-- 037_create_sigil_signing_keys.up.sql
--
-- Реестр trust-anchor-ключей подписи Sigil (ADR-026(h), R3 multi-anchor).
-- До R3 подпись Sigil-а вёл ОДИН ed25519-ключ из Vault KV (по паттерну
-- jwt-signing-key, ADR-014). Multi-anchor вводит набор ключей: ровно один
-- `primary` (которым Keeper подписывает новые Sigil-ы) и любое число прочих
-- `active` (которыми Soul ещё валидирует ранее подписанное) — это даёт
-- безразрывную ротацию: новый ключ вводится как active, становится primary,
-- старый дослуживает active и затем retired (ADR-026(h), replace-семантика
-- SigilTrustAnchors на Soul-стороне).
--
-- Инвариант безопасности: ПРИВАТНИК НИКОГДА не в Postgres. В таблице — только
-- публичная часть (pubkey_pem, SPKI PEM) для раздачи Soul-у как trust-anchor и
-- ссылка vault_ref на приватник в Vault KV (корень доверия — Vault, ADR-026(d)).
--
-- key_id — стабильный идентификатор ключа (SHA-256 от SPKI-DER, hex). Не
-- зависит от строки PEM (whitespace/перенос строки): один и тот же ключ всегда
-- даёт один key_id. UNIQUE — повторный ввод того же ключа отбивается на INSERT.
--
-- status — lifecycle ключа: active (валиден для verify; ровно один из active —
-- primary) → retired (выведен; Soul забывает его при следующем
-- SigilTrustAnchors). Forward-only: retired обратно в active не возвращается
-- (re-introduce = новый INSERT).
--
-- FK на operators(aid) — оба ON DELETE SET NULL: история ввода/вывода ключа
-- переживает удаление оператора (симметрично audit_log / plugin_sigils.revoked).

CREATE TABLE sigil_signing_keys (
    id                BIGINT      GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    key_id            TEXT        NOT NULL UNIQUE,        -- стабильный id: SHA-256 от SPKI-DER, hex
    pubkey_pem        TEXT        NOT NULL,               -- ТОЛЬКО публичная часть (SPKI PEM); приватник НИКОГДА не в PG
    vault_ref         TEXT        NOT NULL,               -- ссылка на приватник в Vault KV
    is_primary        BOOLEAN     NOT NULL DEFAULT false,
    status            TEXT        NOT NULL DEFAULT 'active',  -- active | retired
    introduced_at     TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    introduced_by_aid TEXT,
    retired_at        TIMESTAMPTZ,
    retired_by_aid    TEXT,

    CONSTRAINT sigil_signing_keys_status_enum
        CHECK (status IN ('active', 'retired')),
    CONSTRAINT sigil_signing_keys_introduced_by_fk
        FOREIGN KEY (introduced_by_aid) REFERENCES operators (aid) ON DELETE SET NULL,
    CONSTRAINT sigil_signing_keys_retired_by_fk
        FOREIGN KEY (retired_by_aid) REFERENCES operators (aid) ON DELETE SET NULL
);

-- Инвариант: ровно один primary СРЕДИ АКТИВНЫХ. Partial unique по
-- (is_primary) при status='active' AND is_primary — допускает не более одной
-- строки, удовлетворяющей предикату (две active-primary → 23505). Прецедент
-- стиля — operators_first_archon_idx (003), bootstrap_tokens_active (008).
CREATE UNIQUE INDEX sigil_signing_keys_one_primary
    ON sigil_signing_keys (is_primary)
    WHERE status = 'active' AND is_primary;

COMMENT ON TABLE sigil_signing_keys IS
    'Trust-anchor-ключи подписи Sigil (ADR-026(h), multi-anchor). ТОЛЬКО pubkey_pem + vault_ref; приватник в Vault. Ровно один primary среди active.';
