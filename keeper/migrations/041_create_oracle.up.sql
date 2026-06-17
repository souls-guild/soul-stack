-- 041_create_oracle.up.sql
--
-- Реестры Oracle-контура (ADR-030, beacons + reactor, срез S2): `vigils`
-- (Soul-side проверки, раздаются хосту через VigilSnapshot) + `decrees`
-- (правила reactor «событие → scenario», default-deny) + `oracle_fires`
-- (per-(decree, subject) cooldown-state, loop-prevention).
--
-- Паттерн storage перенят у Augur (миграции 032 omens / 033 rites, ADR-025):
-- managed-через-API записи, субъект `coven` XOR `sid` (как Rite), FK
-- created_by_aid → operators ON DELETE SET NULL (запись переживает удаление
-- оператора). OpenAPI/MCP CRUD + RBAC-perms — отдельный срез S3, здесь только
-- схема + repository.

-- ---------------------------------------------------------------------------
-- vigils — реестр Soul-side проверок (beacon-определений).
--
-- PK — `name` (kebab-case, уникальное в кластере; едет обратно в
-- PortentEvent.beacon_name). `check_addr` — адрес core-beacon
-- (`core.beacon.file_changed` / `core.beacon.service_down` / …; маппится на
-- VigilDef.check; колонка НЕ названа `check` — это reserved keyword PG).
-- `interval_spec` — duration-конвенция (config.ParseDuration), валидируется на
-- service-слое (колонка НЕ названа `interval` — это имя типа PG, требует
-- цитирования; маппится на VigilDef.interval). `params` — JSONB-параметры
-- проверки (path / service-name / порог), форма зависит от check_addr,
-- валидируется на service-слое (как rites.allow к source_type).
--
-- Субъект — строго XOR (как Rite): ровно одно из coven / sid непусто. coven —
-- text[] меток (Vigil раздаётся всем Soul-ам с любой из этих меток); sid —
-- один конкретный хост. coven-метки в формате kebab-case проверяются на
-- service-слое (per-element CHECK для text[] без триггера декларативно не
-- выразить).
--
-- enabled — toggle (managed через API, S3); дефолт true. Read-only по
-- конструкции Vigil-а гарантируется на Soul-стороне (S1), не схемой.
CREATE TABLE vigils (
    name           TEXT        PRIMARY KEY,
    coven          TEXT[],
    sid            TEXT,
    interval_spec  TEXT        NOT NULL,
    check_addr     TEXT        NOT NULL,
    params         JSONB       NOT NULL DEFAULT '{}'::jsonb,
    enabled        BOOLEAN     NOT NULL DEFAULT true,
    created_at     TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at     TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    created_by_aid TEXT,

    CONSTRAINT vigils_name_format
        CHECK (name ~ '^[a-z0-9-]{1,63}$'),
    -- Субъект — строго XOR: ровно одно из coven / sid непусто. coven=NULL и
    -- coven='{}' оба считаются «не задан» (пустой массив бессмысленен как
    -- субъект): требуем непустой массив, когда coven задан.
    CONSTRAINT vigils_subject_xor
        CHECK ((coven IS NOT NULL AND array_length(coven, 1) > 0) <> (sid IS NOT NULL)),
    CONSTRAINT vigils_created_by_aid_fk
        FOREIGN KEY (created_by_aid) REFERENCES operators (aid) ON DELETE SET NULL
);

-- Lookup активных Vigil по SID-субъекту (резолв VigilSnapshot на connect).
CREATE INDEX vigils_sid_idx
    ON vigils (sid) WHERE sid IS NOT NULL;

-- Lookup активных Vigil по coven-меткам (GIN по text[] для оператора &&).
CREATE INDEX vigils_coven_idx
    ON vigils USING GIN (coven) WHERE coven IS NOT NULL;

COMMENT ON TABLE vigils IS
    'Реестр Soul-side проверок Oracle-контура (ADR-030). Субъект coven XOR sid; активный набор едет хосту через VigilSnapshot (ReplaceAll).';

-- ---------------------------------------------------------------------------
-- decrees — реестр правил reactor (Decree). Default-deny: нет матчащего
-- Decree → событие не вызывает действия.
--
-- PK — `name` (kebab-case). `on_beacon` — имя Vigil-а, на чей Portent правило
-- реагирует (матчится с PortentEvent.beacon_name). `where_cel` — опц.
-- предикат над payload события (event.data); пустой → всегда match (субъект
-- уже отфильтровал).
--
-- Субъект — строго XOR (как Rite): subject_coven (text[]) ИЛИ subject_sid.
-- Ограничивает, какие хосты вообще могут триггерить правило (security-слой
-- ADR-030(b)).
--
-- incarnation_name — таргет-incarnation реакции (РЕШЕНИЕ #1, вариант b):
-- ServiceRef сценария резолвится ИЗ неё на enqueue-е (incarnation.service →
-- реестр сервисов), а НЕ дублируется в Decree. Тот же формат, что
-- incarnation.name (миграция 005, CHECK incarnation_name_format) — оно же
-- корневая Coven-метка (ADR-008): membership-проверка субъекта на enqueue-е
-- сводится к incarnation_name ∈ covens отправителя. БЕЗ FK на incarnation —
-- Decree managed-реестр, может пережить пересоздание incarnation; существование
-- проверяется при enqueue fail-closed (incarnation не найдена → skip + warn).
-- Индекс не нужен: горячий путь идёт по on_beacon, incarnation_name читается
-- только у уже сматчивших Decree-ов.
--
-- action_scenario — named scenario (whitelist; raw-команда отвергнута как
-- RCE-вектор, ADR-030(b)). action_input — JSONB-вход сценария (vault-ref КАК
-- ЕСТЬ, инвариант A ADR-027). cooldown — duration-конвенция, минимальный
-- интервал между срабатываниями per-(decree, subject); валидируется на
-- service-слое.
CREATE TABLE decrees (
    name             TEXT        PRIMARY KEY,
    on_beacon        TEXT        NOT NULL,
    where_cel        TEXT,
    subject_coven    TEXT[],
    subject_sid      TEXT,
    incarnation_name TEXT        NOT NULL,
    action_scenario  TEXT        NOT NULL,
    action_input     JSONB       NOT NULL DEFAULT '{}'::jsonb,
    cooldown         TEXT        NOT NULL DEFAULT '0s',
    enabled          BOOLEAN     NOT NULL DEFAULT true,
    created_at       TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at       TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    created_by_aid   TEXT,

    CONSTRAINT decrees_name_format
        CHECK (name ~ '^[a-z0-9-]{1,63}$'),
    CONSTRAINT decrees_incarnation_name_format
        CHECK (incarnation_name ~ '^[a-z0-9][a-z0-9-]{0,62}$'),
    CONSTRAINT decrees_scenario_format
        CHECK (action_scenario ~ '^[a-z][a-z0-9_]*$'),
    -- Субъект — строго XOR (как Rite): ровно одно из subject_coven /
    -- subject_sid непусто.
    CONSTRAINT decrees_subject_xor
        CHECK ((subject_coven IS NOT NULL AND array_length(subject_coven, 1) > 0) <> (subject_sid IS NOT NULL)),
    CONSTRAINT decrees_created_by_aid_fk
        FOREIGN KEY (created_by_aid) REFERENCES operators (aid) ON DELETE SET NULL
);

-- Lookup Decree по on_beacon (горячий путь match-флоу: Oracle на каждый
-- Portent делает SELECT … WHERE on_beacon = $1 AND enabled).
CREATE INDEX decrees_on_beacon_idx
    ON decrees (on_beacon) WHERE enabled;

COMMENT ON TABLE decrees IS
    'Реестр правил reactor Oracle (ADR-030). Default-deny; субъект coven XOR sid; таргет incarnation_name (ServiceRef резолвится из неё); action = named scenario (whitelist).';

-- ---------------------------------------------------------------------------
-- oracle_fires — cooldown-state per-(decree, subject), loop-prevention
-- (ADR-030(a)).
--
-- Колонка last_fired_at на decrees НЕ годится: один Decree срабатывает для
-- МНОГИХ субъектов (coven-Decree покрывает десятки хостов), cooldown нужен
-- per-(decree, subject), а не per-decree. Отдельная таблица с PK
-- (decree, subject) — минимальная структура: ровно одна строка на пару
-- (UPSERT ON CONFLICT, НЕ append-only), читается match-флоу перед enqueue,
-- пишется после fire.
--
-- subject здесь — авторитетный SID хоста-отправителя (из mTLS peer cert, НЕ
-- payload-echo): cooldown привязан к конкретному хосту, для которого ставится
-- scenario.
--
-- decree → decrees(name) ON DELETE CASCADE: cooldown-state без Decree-а
-- бессмыслен, удаление Decree-а атомарно чистит его историю срабатываний.
CREATE TABLE oracle_fires (
    decree     TEXT        NOT NULL,
    subject    TEXT        NOT NULL,
    fired_at   TIMESTAMPTZ NOT NULL DEFAULT NOW(),

    CONSTRAINT oracle_fires_pk
        PRIMARY KEY (decree, subject),
    CONSTRAINT oracle_fires_decree_fk
        FOREIGN KEY (decree) REFERENCES decrees (name) ON DELETE CASCADE
);

COMMENT ON TABLE oracle_fires IS
    'Cooldown-state Oracle per-(decree, subject) (ADR-030(a), loop-prevention). UPSERT, одна строка на пару; decree ON DELETE CASCADE.';
