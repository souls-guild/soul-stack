-- 028_create_plugin_sigils.up.sql
--
-- Реестр Sigil-ов — Keeper-signed allow-list допущенных бинарей плагинов
-- (ADR-026, docs/keeper/plugins.md → Integrity-model). Заменяет TOFU-семантику
-- «host сам решает доверять» на «authoritative-список ведёт Keeper»: запись
-- появляется, только когда Архонт ЯВНО допустил плагин через OpenAPI/MCP.
--
-- Идентификация плагина — тройка (namespace, name, ref):
--   - namespace — тип плагина: cloud / ssh / mod;
--   - name      — имя бинаря (soul-cloud-hetzner и т.п.);
--   - ref       — версия. В MVP (ADR-026(g), Вариант C) это operator-asserted
--                 МЕТКА (как правило git-tag по ADR-007), а НЕ git-verified
--                 ref: при allow Keeper читает текущий бинарь из single-slot
--                 кеша по (namespace, name) — ref в чтении не участвует — и
--                 НЕ проверяет, что бинарь собран из этого ref-а. Authority
--                 целостности — sha256 + signature ниже, не ref. git-verified
--                 ref — post-MVP (требует ref-aware раскладки кеша).
-- Уникальность — partial unique по активным записям: не более одной АКТИВНОЙ
-- (revoked_at IS NULL) записи на (namespace, name, ref). История отозванных
-- допусков сохраняется, поэтому re-allow после revoke — чистый INSERT новой
-- записи (audit-история sha256/signature/allowed_by не затирается UPDATE-ом).
-- Прецедент стиля — bootstrap_tokens_active_by_sid_idx (миграция 008). Lookup
-- при verify идёт по этой тройке и покрыт active-индексом.
--
-- sha256 — отпечаток одобренного бинаря (hex, lowercase, 64 символа). Вместе
-- с manifest составляет подписываемый блок Sigil-а (ADR-026(b)/(c)): подпись
-- покрывает manifest с пришитым digest-ом, поэтому задекларированные
-- side_effects / capabilities не подделываемы.
--
-- signature — подпись Keeper-а над подписываемым блоком. Тип BYTEA: подпись
-- ed25519/ECDSA — это сырые бинарные байты фиксированной (ed25519, 64 байта)
-- или близкой длины; BYTEA хранит их без накладного base64-кодирования и без
-- риска рассинхрона кодировки на verify-пути. Детальный формат подписываемого
-- блока — слайс S3 (здесь только колонка).
--
-- Lifecycle допуска: allowed (allowed_by_aid / allowed_at) → опционально
-- revoked (revoked_at / revoked_by_aid). Ревокация — мягкая (запись остаётся
-- для аудита, NOT NULL revoked_at = отозван).
--
-- FK на operators(aid):
--   - allowed_by_aid (NOT NULL) — без ON DELETE: default NO ACTION (эффективно
--     RESTRICT для этого случая). Удалить оператора, держащего активный допуск,
--     нельзя — иначе теряется автор записи о доверии (security-инвариант;
--     SET NULL невозможен из-за NOT NULL).
--   - revoked_by_aid (NULL)     — ON DELETE SET NULL: история ревокации
--     переживает удаление оператора (симметрично audit_log / bootstrap_tokens).

CREATE TABLE plugin_sigils (
    id              BIGINT      GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    namespace       TEXT        NOT NULL,
    name            TEXT        NOT NULL,
    ref             TEXT        NOT NULL,
    sha256          TEXT        NOT NULL,
    signature       BYTEA       NOT NULL,
    manifest        JSONB       NOT NULL,
    allowed_by_aid  TEXT        NOT NULL,
    allowed_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    revoked_at      TIMESTAMPTZ,
    revoked_by_aid  TEXT,

    CONSTRAINT plugin_sigils_sha256_format
        -- SHA-256 hex = 64 lower-hex chars.
        CHECK (sha256 ~ '^[0-9a-f]{64}$'),
    CONSTRAINT plugin_sigils_allowed_by_aid_fk
        FOREIGN KEY (allowed_by_aid) REFERENCES operators (aid),
    CONSTRAINT plugin_sigils_revoked_by_aid_fk
        FOREIGN KEY (revoked_by_aid) REFERENCES operators (aid) ON DELETE SET NULL
);

-- Лента Sigil-ов конкретного оператора (audit / триаж «что допускал Архонт»).
CREATE INDEX plugin_sigils_allowed_by_aid_idx
    ON plugin_sigils (allowed_by_aid);

-- Инвариант: не более одной активной (не отозванной) записи на
-- (namespace, name, ref). Этот же индекс покрывает скан активных допусков —
-- типовой запрос verify-/list-пути. Прецедент — bootstrap_tokens (миграция 008).
CREATE UNIQUE INDEX plugin_sigils_active_idx
    ON plugin_sigils (namespace, name, ref)
    WHERE revoked_at IS NULL;

COMMENT ON TABLE plugin_sigils IS
    'Keeper-signed allow-list допущенных плагинов (ADR-026). Ключ (namespace, name, ref) → sha256 + подпись Keeper-а. Заменяет TOFU.';
