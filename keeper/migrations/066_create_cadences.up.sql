-- 066_create_cadences.up.sql
--
-- ADR-046 → S1 schema: Cadence — расписание, по времени спавнящее обычный
-- Voyage-прогон. Отдельная сущность в Postgres, переживает прогоны: по
-- наступлении времени Cadence спавнит НОВЫЙ Voyage (Insert в voyages +
-- voyage_targets), Voyage-инвариант «один Voyage = один прогон» сохранён.
--
-- На S1 — ТОЛЬКО таблица + back-link voyages.cadence_id + CRUD-слой. Scheduler
-- (Reaper-правило spawn_due_cadence, action `spawn`), пересчёт next_run_at
-- (interval/cron), overlap_policy-исполнение и API подключаются в S2-S4.
--
-- Cadence хранит «рецепт» прогона (то же множество, что VoyageCreateRequest,
-- ADR-043) + правило повторения (interval XOR cron) + политику наложения. По
-- стилю parity voyages (миграция 059): nullable-поля рецепта без CHECK на
-- payload-консистентность того, что резолвится оркестратором; XOR-инварианты,
-- которые «оба NULL» не выразят CHECK-ом без ложных отказов, — на стороне
-- CRUD-валидации (parity voyages_batch_size/percent XOR в handler).
--
-- CHECK-инварианты cadences:
--   * cadences_schedule_kind_valid:    interval | cron.
--   * cadences_overlap_policy_valid:   skip | queue | parallel.
--   * cadences_kind_valid:             scenario | command.
--   * cadences_schedule_consistency:   schedule_kind=interval ⇒ interval_seconds
--     NOT NULL И cron_expr NULL; schedule_kind=cron ⇒ cron_expr NOT NULL И
--     interval_seconds NULL (ровно один из двух по виду расписания).
--   * cadences_kind_payload_consistency: kind=scenario ⇒ scenario_name NOT NULL;
--     kind=command ⇒ module NOT NULL (parity voyages).
--   * cadences_interval_seconds_positive / cadences_batch_size_positive /
--     cadences_concurrency_positive / cadences_batch_percent_range /
--     cadences_fail_threshold_positive — sane-bounds (parity voyages 059/065).
--
-- FK:
--   * created_by_aid → operators(aid) (NOT NULL — Cadence всегда заводится
--     конкретным Архонтом; спавн на тике исполняется от его имени, ADR-046 §7).

CREATE TABLE cadences (
    id                     TEXT        PRIMARY KEY,
    name                   TEXT        NOT NULL,
    enabled                BOOLEAN     NOT NULL DEFAULT true,

    -- Правило повторения.
    schedule_kind          TEXT        NOT NULL,
    interval_seconds       INT,
    cron_expr              TEXT,
    overlap_policy         TEXT        NOT NULL,

    -- Рецепт прогона (parity voyages, миграция 059 + 064 + 065).
    kind                   TEXT        NOT NULL,
    scenario_name          TEXT,
    module                 TEXT,
    target                 JSONB       NOT NULL,
    input                  JSONB       NOT NULL DEFAULT '{}',
    batch_mode             TEXT,
    batch_size             INT,
    batch_percent          INT,
    concurrency            INT,
    fail_threshold         INT,
    inter_batch_interval   INTERVAL,
    inter_unit_interval    INTERVAL,
    require_alive          BOOLEAN,
    on_failure             TEXT,

    -- Расчётные тайминги (scheduler — S2; на S1 CRUD пишет/читает as-is).
    next_run_at            TIMESTAMPTZ,
    last_run_at            TIMESTAMPTZ,

    created_by_aid         TEXT        NOT NULL,
    created_at             TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at             TIMESTAMPTZ NOT NULL DEFAULT NOW(),

    CONSTRAINT cadences_schedule_kind_valid
        CHECK (schedule_kind IN ('interval', 'cron')),
    CONSTRAINT cadences_overlap_policy_valid
        CHECK (overlap_policy IN ('skip', 'queue', 'parallel')),
    CONSTRAINT cadences_kind_valid
        CHECK (kind IN ('scenario', 'command')),
    CONSTRAINT cadences_schedule_consistency
        CHECK (
            (schedule_kind = 'interval' AND interval_seconds IS NOT NULL AND cron_expr IS NULL)
            OR (schedule_kind = 'cron' AND cron_expr IS NOT NULL AND interval_seconds IS NULL)
        ),
    CONSTRAINT cadences_kind_payload_consistency
        CHECK (
            (kind = 'scenario' AND scenario_name IS NOT NULL)
            OR (kind = 'command' AND module IS NOT NULL)
        ),
    CONSTRAINT cadences_interval_seconds_positive
        CHECK (interval_seconds IS NULL OR interval_seconds > 0),
    CONSTRAINT cadences_batch_mode_valid
        CHECK (batch_mode IS NULL OR batch_mode IN ('barrier', 'window')),
    CONSTRAINT cadences_batch_size_positive
        CHECK (batch_size IS NULL OR batch_size > 0),
    CONSTRAINT cadences_batch_percent_range
        CHECK (batch_percent IS NULL OR (batch_percent >= 1 AND batch_percent <= 100)),
    CONSTRAINT cadences_concurrency_positive
        CHECK (concurrency IS NULL OR concurrency > 0),
    CONSTRAINT cadences_fail_threshold_positive
        CHECK (fail_threshold IS NULL OR fail_threshold > 0),
    CONSTRAINT cadences_on_failure_valid
        CHECK (on_failure IS NULL OR on_failure IN ('abort', 'continue')),
    CONSTRAINT cadences_created_by_aid_fk
        FOREIGN KEY (created_by_aid) REFERENCES operators (aid)
);

-- Scheduler-скан due-расписаний (Reaper-правило spawn_due_cadence — S2):
-- enabled И next_run_at <= NOW(). Partial-индекс по next_run_at среди enabled.
CREATE INDEX cadences_due_scan_idx
    ON cadences (next_run_at)
    WHERE enabled;

COMMENT ON TABLE cadences IS
    'Реестр Cadence-расписаний (ADR-046, S1). По времени спавнит обычный Voyage-прогон (Insert voyages/voyage_targets, back-link voyages.cadence_id). Хранит рецепт прогона (parity VoyageCreateRequest) + правило повторения (interval XOR cron) + overlap_policy (skip/queue/parallel). На S1 — таблица + CRUD без scheduler (Reaper spawn_due_cadence — S2).';

-- Back-link voyages.cadence_id (ADR-046 §2): ручной прогон ⇒ NULL; спавн от
-- Cadence ⇒ populated. ON DELETE SET NULL — удаление Cadence НЕ уносит историю
-- порождённых прогонов (дети-сироты остаются в All-runs виде). Additive nullable
-- (forward-compat only-add, ADR-012). Partial-индекс под drill «расписание → его
-- прогоны» (GET /v1/cadences/{id}/runs — S4).
ALTER TABLE voyages
    ADD COLUMN cadence_id TEXT;

ALTER TABLE voyages
    ADD CONSTRAINT voyages_cadence_id_fk
        FOREIGN KEY (cadence_id) REFERENCES cadences (id) ON DELETE SET NULL;

CREATE INDEX voyages_cadence_id_idx
    ON voyages (cadence_id)
    WHERE cadence_id IS NOT NULL;

COMMENT ON COLUMN voyages.cadence_id IS
    'Back-link на породившую Cadence (ADR-046 §2). NULL ⇒ ручной прогон; populated ⇒ спавн от Cadence. FK ON DELETE SET NULL — снос Cadence не уносит историю детей.';
