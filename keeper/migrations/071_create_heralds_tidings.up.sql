-- 071_create_heralds_tidings.up.sql
--
-- ADR-052 (Herald + Tiding — уведомления о событиях прогонов), слайс S1.
--
-- Две managed-через-API/MCP сущности (паттерн Omen/Rite, миграции 032/033):
--   - heralds — реестр КАНАЛОВ доставки уведомлений («куда слать»).
--   - tidings — реестр ПРАВИЛ подписки («на что реагировать → каким Herald-ом»).
--
-- Доставка/tap/notification-dispatcher — следующие слайсы (S2-S4); здесь только
-- хранилище + CRUD-слой (keeper/internal/herald).

-- heralds — канал доставки. PK name (kebab-case). type — closed enum (webhook в
-- MVP; slack/email — additive post-MVP без breaking change, новые значения CHECK).
--
-- config (JSONB) — per-type конфигурация канала: для webhook — `url` (обязателен)
-- + опц. `headers` + опц. флаги безопасности `http_allowed`/`allow_private`
-- (явный opt-out SSRF-контура, паттерн core.url, ADR-052(e)). Shape валидируется
-- на service-слое (herald.validateConfig) — JSONB нельзя сопоставить с type
-- декларативным CHECK-ом без триггера, как omens.auth_ref / push_providers.params.
--
-- secret_ref (vault-ref, nullable) — секрет канала (signing-token webhook-а). НЕ
-- каждому webhook нужна подпись, поэтому NULL-able (отличие от omens.auth_ref
-- NOT NULL: там credential к внешней системе обязателен всегда). Master-cred в БД
-- НЕ хранится — только vault-ref (ADR-052(e), паттерн omens.auth_ref / core.url).
-- Формат vault-ref CHECK-ом НЕ ловится — это делает service-слой (vault.ParseRef).
--
-- FK:
--   - created_by_aid → operators(aid) ON DELETE SET NULL (запись Herald-а
--     переживает удаление оператора; симметрично omens/providers). NULL-able.

CREATE TABLE heralds (
    name           TEXT        PRIMARY KEY,
    type           TEXT        NOT NULL,
    config         JSONB       NOT NULL DEFAULT '{}'::jsonb,
    secret_ref     TEXT,
    enabled        BOOLEAN     NOT NULL DEFAULT true,
    created_at     TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at     TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    created_by_aid TEXT,

    CONSTRAINT heralds_name_format
        CHECK (name ~ '^[a-z0-9-]{1,63}$'),
    CONSTRAINT heralds_type_enum
        CHECK (type IN ('webhook')),
    CONSTRAINT heralds_created_by_aid_fk
        FOREIGN KEY (created_by_aid) REFERENCES operators (aid) ON DELETE SET NULL
);

COMMENT ON TABLE heralds IS
    'Реестр каналов доставки уведомлений Herald (ADR-052). type closed-enum (webhook MVP), config JSONB (webhook: url+headers+opt-out флаги), secret_ref = vault-ref (секрет канала в БД не хранится).';

-- tidings — правило подписки. PK name (kebab-case). event_types — непустой
-- TEXT[] audit event-types с поддержкой area-glob (`scenario_run.*`); shape
-- (известный тип ИЛИ glob области прогона) валидируется на service-слое — каталог
-- EventType-констант эволюционирует, БД-CHECK по нему был бы хрупким (как
-- omens.auth_ref / rites.allow). CHECK здесь ловит только инвариант «список
-- непустой» (cardinality, доступно декларативно).
--
-- only_failures / only_changes — фильтры события (ADR-052(c)). incarnation /
-- cadence — опц. селекторы привязки к источнику прогона (NULL = без фильтра).
--
-- FK:
--   - herald → heralds(name) ON DELETE CASCADE: Tiding без Herald-а
--     бессмыслен, снос канала атомарно уносит его подписки (ADR-052(a),
--     naming-rules.md; симметрично rites.omen ON DELETE CASCADE).
--   - created_by_aid → operators(aid) ON DELETE SET NULL (как heralds).

CREATE TABLE tidings (
    name           TEXT        PRIMARY KEY,
    herald         TEXT        NOT NULL,
    event_types    TEXT[]      NOT NULL,
    only_failures  BOOLEAN     NOT NULL DEFAULT false,
    only_changes   BOOLEAN     NOT NULL DEFAULT false,
    incarnation    TEXT,
    cadence        TEXT,
    enabled        BOOLEAN     NOT NULL DEFAULT true,
    created_at     TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at     TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    created_by_aid TEXT,

    CONSTRAINT tidings_name_format
        CHECK (name ~ '^[a-z0-9-]{1,63}$'),
    CONSTRAINT tidings_event_types_nonempty
        CHECK (cardinality(event_types) > 0),
    CONSTRAINT tidings_herald_fk
        FOREIGN KEY (herald) REFERENCES heralds (name) ON DELETE CASCADE,
    CONSTRAINT tidings_created_by_aid_fk
        FOREIGN KEY (created_by_aid) REFERENCES operators (aid) ON DELETE SET NULL
);

-- Lookup всех Tiding-правил одного Herald-а (CRUD list-by-herald + каскад-аудит).
CREATE INDEX tidings_herald_idx
    ON tidings (herald);

-- Dispatcher (S2) матчит событие только против ВКЛЮЧЁННЫХ правил. Partial-индекс
-- по enabled=true — горячий путь матча не сканирует выключенные подписки.
CREATE INDEX tidings_enabled_idx
    ON tidings (enabled) WHERE enabled = true;

COMMENT ON TABLE tidings IS
    'Реестр правил подписки на уведомления Tiding (ADR-052). event_types area-glob + фильтры only_failures/only_changes + селекторы incarnation/cadence. herald ON DELETE CASCADE.';
