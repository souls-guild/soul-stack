-- 033_create_rites.up.sql
--
-- Реестр Rite-ов — grant-ов Augur-а (ADR-025, docs/keeper/augur.md → §4.2).
-- Rite связывает субъект (Coven XOR конкретный SID) с Omen-ом, allow-list-ом
-- и режимом доставки (`delegate`): «такой-то субъект может через Augur получить
-- из такого-то Omen-а такие-то значения, в таком-то режиме».
--
-- Суррогатный PK (`id`, GENERATED ALWAYS AS IDENTITY) — у одного субъекта на
-- один Omen может быть несколько Rite-ов (разные allow-list-ы / режимы), пары
-- (omen, subject) не уникальны. Прецедент IDENTITY-PK — plugin_sigils (028).
--
-- Субъект — строго XOR: ровно одно из coven / sid непусто (CHECK
-- rites_subject_xor). coven-Rite применяется ко всем Soul-ам с этой меткой;
-- sid-Rite — к одному хосту. coven-формат проверяется только когда coven
-- задан (NULL-tolerant CHECK).
--
-- allow (JSONB) — allow-list разрешённых значений; форма зависит от
-- source_type Omen-а (vault: paths/policies, prometheus: queries, elk:
-- indices, augur.md §4.2). Shape валидируется на service-слое (JSONB нельзя
-- сопоставить с source_type другого Omen-а декларативным CHECK-ом без триггера).
--
-- delegate — граница MVP-фаз: false (брокер, MVP-1, данные через Keeper) /
-- true (делегация, MVP-2, Soul ходит сам с эфемерным scoped-credential).
-- Дефолт false — «безопасность на первом месте», делегация — явный opt-in.
--
-- token_ttl / token_num_uses — параметры минтуемого scoped Vault-токена,
-- осмысленны ТОЛЬКО для vault-Omen с delegate=true. CHECK
-- rites_token_fields_vault_only ловит только импликацию «token-поля заданы ⇒
-- delegate=true» (доступно в строке rites). Вторая половина инварианта —
-- «⇒ source_type=vault» — требует join к omens и проверяется на service-слое
-- (augur.md §4.2). token_ttl — duration-строка (config.ParseDuration),
-- валидируется на service-слое, не CHECK-ом.
--
-- FK:
--   - omen → omens(name) ON DELETE CASCADE: Rite без Omen-а бессмыслен,
--     удаление Omen-а атомарно убирает все его grant-ы (augur.md §9 форк).
--   - created_by_aid → operators(aid) ON DELETE SET NULL (запись Rite-а
--     переживает удаление оператора; симметрично omens/providers).

CREATE TABLE rites (
    id             BIGINT      GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    omen           TEXT        NOT NULL,
    coven          TEXT,
    sid            TEXT,
    allow          JSONB       NOT NULL,
    delegate       BOOLEAN     NOT NULL DEFAULT false,
    token_ttl      TEXT,
    token_num_uses INT,
    created_at     TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    created_by_aid TEXT,

    CONSTRAINT rites_omen_fk
        FOREIGN KEY (omen) REFERENCES omens (name) ON DELETE CASCADE,
    -- Субъект — строго XOR: ровно одно из coven / sid непусто.
    CONSTRAINT rites_subject_xor
        CHECK ((coven IS NOT NULL) <> (sid IS NOT NULL)),
    CONSTRAINT rites_coven_format
        CHECK (coven IS NULL OR coven ~ '^[a-z0-9][a-z0-9-]*$'),
    -- token-поля разрешены только при delegate=true (⇒vault — service-слой).
    CONSTRAINT rites_token_fields_vault_only
        CHECK ((token_ttl IS NULL AND token_num_uses IS NULL) OR delegate = true),
    CONSTRAINT rites_created_by_aid_fk
        FOREIGN KEY (created_by_aid) REFERENCES operators (aid) ON DELETE SET NULL
);

-- Lookup всех Rite-ов одного Omen-а (авторизация §6, list-by-omen).
CREATE INDEX rites_omen_idx
    ON rites (omen);

-- Lookup sid-Rite-ов по конкретному SID (авторизация §6.3). Partial:
-- индексируем только заполненные sid (XOR ⇒ половина строк с sid=NULL).
CREATE INDEX rites_sid_idx
    ON rites (sid) WHERE sid IS NOT NULL;

-- Lookup coven-Rite-ов по Coven-метке (авторизация §6.3). Partial по той же
-- причине, что rites_sid_idx.
CREATE INDEX rites_coven_idx
    ON rites (coven) WHERE coven IS NOT NULL;

COMMENT ON TABLE rites IS
    'Grant-ы Augur (ADR-025): субъект (coven XOR sid) × omen → allow + delegate + token-параметры. omen ON DELETE CASCADE.';
