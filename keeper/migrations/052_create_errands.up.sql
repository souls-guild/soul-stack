-- 052_create_errands.up.sql
--
-- Реестр Errand-ов (`POST /v1/souls/{sid}/exec`, MCP `keeper.errand.run`,
-- ADR-033): одна строка = один pull-ad-hoc exec одиночного модуля на конкретном
-- Soul через mTLS EventStream. Отдельная таблица от `apply_runs` (там per-host
-- apply scenario с state_changes/barrier) и от `push_runs` (там multi-host
-- ad-hoc destiny по SSH); Errand — single-host, без incarnation/scenario.
--
-- Поля:
--   - `errand_id`         — ULID, PK.
--   - `sid`               — целевой Soul.
--   - `module`            — fully-qualified `<ns>.<name>.<state>` (whitelist
--                            проверяется и на Keeper-side при приёме, и
--                            defense-in-depth на Soul-side errand-runner-ом).
--   - `input`             — JSON-объект input-а модуля (jsonb).
--   - `status`            — терминал (CHECK ниже).
--   - `exit_code`         — exit-код verb-модуля (NULL для read-safe non-shell).
--   - `stdout`/`stderr`   — captured вывод (cap 64 KiB, маскированный).
--   - `*_truncated`       — флаги превышения cap (для UI/observability).
--   - `duration_ms`       — длительность Errand-а на Soul-side.
--   - `error_message`     — маскированная причина FAILED/TIMED_OUT/MODULE_NOT_ALLOWED.
--   - `output`            — структурный output read-safe модулей (jsonb); для
--                            shell/exec — NULL.
--   - `started_by_aid`    — Архонт-инициатор (FK на operators).
--   - `started_by_kid`    — KID Keeper-инстанса (для будущего sweep-а
--                            осиротевших running-Errand-ов при рестарте).
--   - `started_at`        — когда Keeper принял запрос.
--   - `finished_at`       — терминал (NULL пока running).
--   - `ttl_at`            — `started_at + reaper.errands.ttl` (default 7д);
--                            используется reaper-правилом `purge_old_errands`.
--
-- Статусы (CHECK): паритет с ErrandStatus enum в proto/keeper/v1/errand.proto.
--   running              — записан, ждём ErrandResult от Soul (или async cap).
--   success              — ErrandResult{status:SUCCESS}.
--   failed               — ErrandResult{status:FAILED}.
--   timed_out            — ErrandResult{status:TIMED_OUT}.
--   cancelled            — ErrandResult{status:CANCELLED} (slice E5).
--   module_not_allowed   — модуль не прошёл whitelist (defense-in-depth Soul-side).
--
-- Индексы:
--   - errands_running_idx — partial по running, дешёвый sweep осиротевших
--     при рестарте Keeper-а (slice E2/E4).
--   - errands_sid_started_idx — list-API `GET /v1/errands?sid=<sid>` (E2).
--   - errands_ttl_idx — `purge_old_errands` (E4).
--
-- FK на operators: ON DELETE RESTRICT — Errand-история требует валидный AID
-- инициатора (revoke архонта не должен сносить аудит-связку; revoked-Архонт
-- остаётся в `operators` с `revoked_at`, FK сохраняется).

CREATE TABLE errands (
    errand_id         TEXT        PRIMARY KEY,
    sid               TEXT        NOT NULL,
    module            TEXT        NOT NULL,
    input             JSONB       NOT NULL DEFAULT '{}'::jsonb,
    status            TEXT        NOT NULL,
    exit_code         INT,
    stdout            TEXT,
    stderr            TEXT,
    stdout_truncated  BOOLEAN     NOT NULL DEFAULT FALSE,
    stderr_truncated  BOOLEAN     NOT NULL DEFAULT FALSE,
    duration_ms       BIGINT,
    error_message     TEXT,
    output            JSONB,
    started_by_aid    TEXT        NOT NULL,
    started_by_kid    TEXT        NOT NULL,
    started_at        TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    finished_at       TIMESTAMPTZ,
    ttl_at            TIMESTAMPTZ NOT NULL,
    CONSTRAINT errands_status_valid CHECK (status IN
        ('running', 'success', 'failed', 'timed_out', 'cancelled', 'module_not_allowed')),
    CONSTRAINT errands_started_by_aid_fk
        FOREIGN KEY (started_by_aid) REFERENCES operators (aid) ON DELETE RESTRICT
);

CREATE INDEX errands_running_idx
    ON errands (errand_id)
    WHERE status = 'running';

CREATE INDEX errands_sid_started_idx
    ON errands (sid, started_at DESC);

CREATE INDEX errands_ttl_idx
    ON errands (ttl_at);

COMMENT ON TABLE errands IS
    'Реестр pull-ad-hoc Errand-ов (ADR-033). Одна строка = один POST /v1/souls/{sid}/exec.';
