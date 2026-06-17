-- 059_create_voyages.up.sql
--
-- ADR-043 → S1 schema: Voyage PG-таблицы фундамента (рядом со старыми
-- tides / errand_runs / apply_runs, которые продолжают работать до S7).
--
-- Voyage — унифицированный батчевый прогон, поглощающий Tide (kind=scenario) +
-- ErrandRun (kind=command). Единица батча — Leg (один «отрезок пути»),
-- идентифицируется voyage_targets.batch_index. Имена таблиц — производные от
-- locked-сущности Voyage/Leg (эскиз ADR-043 `runs`/`run_targets` уточнён до
-- `voyages`/`voyage_targets`, чтобы имя несло сущность, а не было общим `runs`).
--
-- Failover-resilient через PG-based claim+lease (parity Tide / ErrandRun /
-- Ward-claim ADR-027(d)): pending → claimed_by_kid + claim_expires_at →
-- running; протухший claim возвращается Reaper-правилом обратно в pending для
-- пере-claim другим Keeper-инстансом (тираж — пост-S1). attempt++ на каждый
-- claim — fencing-epoch.
--
-- На S1 воркер НЕ исполняет реальную работу (config-gated OFF по умолчанию):
-- таблицы и claim-loop существуют как фундамент, реальное исполнение
-- scenario/command подключается в S2/S3.
--
-- CHECK-инварианты voyages:
--   * voyages_kind_valid:           scenario | command.
--   * voyages_status_valid:         closed-set 7 статусов (+ scheduled для
--                                   отложенного старта, S4).
--   * voyages_on_failure_valid:     abort | continue.
--   * voyages_kind_payload_consistency: kind=scenario ⇒ scenario_name NOT NULL;
--                                   kind=command ⇒ module NOT NULL.
--   * voyages_running_claim_consistency: running ⇒ claim-поля NOT NULL.
--   * voyages_terminal_finished_at: terminal-статус ⇒ finished_at NOT NULL.
--   * voyages_batch_index_within_total: 0 ≤ current_batch_index ≤ total_batches.
--   * voyages_attempt_non_negative / voyages_batch_size_positive /
--     voyages_concurrency_positive — sane-bounds.
--
-- FK:
--   * started_by_aid → operators(aid) (NOT NULL, без ON DELETE — парность
--     tides/errand_runs; Voyage всегда инициируется конкретным Архонтом).

CREATE TABLE voyages (
    voyage_id              TEXT        PRIMARY KEY,
    kind                   TEXT        NOT NULL,
    scenario_name          TEXT,
    module                 TEXT,
    input                  JSONB       NOT NULL DEFAULT '{}',
    target_resolved        JSONB       NOT NULL,
    target_origin          JSONB,
    batch_size             INT,
    concurrency            INT,
    dry_run                BOOLEAN     NOT NULL DEFAULT false,
    schedule_at            TIMESTAMPTZ,
    inter_batch_interval   INTERVAL,
    on_failure             TEXT,
    total_batches          INT         NOT NULL DEFAULT 0,
    current_batch_index    INT         NOT NULL DEFAULT 0,
    status                 TEXT        NOT NULL,
    claimed_by_kid         TEXT,
    last_renewed_at        TIMESTAMPTZ,
    claim_expires_at       TIMESTAMPTZ,
    attempt                INT         NOT NULL DEFAULT 0,
    started_by_aid         TEXT        NOT NULL,
    created_at             TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    started_at             TIMESTAMPTZ,
    finished_at            TIMESTAMPTZ,
    summary                JSONB,

    CONSTRAINT voyages_kind_valid
        CHECK (kind IN ('scenario', 'command')),
    CONSTRAINT voyages_status_valid
        CHECK (status IN ('scheduled', 'pending', 'running', 'succeeded', 'failed', 'partial_failed', 'cancelled')),
    CONSTRAINT voyages_on_failure_valid
        CHECK (on_failure IS NULL OR on_failure IN ('abort', 'continue')),
    CONSTRAINT voyages_kind_payload_consistency
        CHECK (
            (kind = 'scenario' AND scenario_name IS NOT NULL)
            OR (kind = 'command' AND module IS NOT NULL)
        ),
    CONSTRAINT voyages_batch_size_positive
        CHECK (batch_size IS NULL OR batch_size > 0),
    CONSTRAINT voyages_concurrency_positive
        CHECK (concurrency IS NULL OR concurrency > 0),
    CONSTRAINT voyages_total_batches_non_negative
        CHECK (total_batches >= 0),
    CONSTRAINT voyages_batch_index_within_total
        CHECK (current_batch_index >= 0 AND current_batch_index <= total_batches),
    CONSTRAINT voyages_attempt_non_negative
        CHECK (attempt >= 0),
    CONSTRAINT voyages_running_claim_consistency
        CHECK (
            (status <> 'running')
            OR (claimed_by_kid IS NOT NULL AND claim_expires_at IS NOT NULL)
        ),
    CONSTRAINT voyages_terminal_finished_at
        CHECK (
            (status NOT IN ('succeeded', 'failed', 'partial_failed', 'cancelled'))
            OR (finished_at IS NOT NULL)
        ),
    CONSTRAINT voyages_started_by_aid_fk
        FOREIGN KEY (started_by_aid) REFERENCES operators (aid)
);

-- Pickup pending Voyage-ов по FIFO created_at
-- (VoyageWorker.ClaimNext, FOR UPDATE SKIP LOCKED).
CREATE INDEX voyages_pending_pickup_idx
    ON voyages (created_at)
    WHERE status = 'pending';

-- Recovery-скан: только активные running с истёкшим claim
-- (Reaper-правило reclaim_voyages — пост-S1).
CREATE INDEX voyages_claim_scan_idx
    ON voyages (claim_expires_at)
    WHERE status = 'running';

-- voyage_targets — единицы прогона (Leg-разбиение). Для kind=scenario
-- target_kind='incarnation' + back-link apply_id на apply_runs (per-incarnation
-- scenario-run); для kind=command target_kind='sid' + back-link errand_id на
-- errands (per-host exec). Даёт единый All-runs вид с two-level drill (S5).
--
-- CHECK-инварианты voyage_targets:
--   * voyage_targets_target_kind_valid: incarnation | sid.
--   * voyage_targets_status_valid: closed-set (awaiting/running/succeeded/
--     failed/cancelled/no_match — последний для целей, не попавших под match).
--   * voyage_targets_batch_index_non_negative: batch_index ≥ 0.
--
-- PK (voyage_id, target_kind, target_id) — одна цель уникальна в рамках прогона.
-- FK voyage_id → voyages ON DELETE CASCADE: снос Voyage сносит его targets.
CREATE TABLE voyage_targets (
    voyage_id     TEXT        NOT NULL,
    target_kind   TEXT        NOT NULL,
    target_id     TEXT        NOT NULL,
    batch_index   INT         NOT NULL,
    status        TEXT        NOT NULL DEFAULT 'awaiting',
    apply_id      TEXT,
    errand_id     TEXT,
    finished_at   TIMESTAMPTZ,

    CONSTRAINT voyage_targets_pkey
        PRIMARY KEY (voyage_id, target_kind, target_id),
    CONSTRAINT voyage_targets_target_kind_valid
        CHECK (target_kind IN ('incarnation', 'sid')),
    CONSTRAINT voyage_targets_status_valid
        CHECK (status IN ('awaiting', 'running', 'succeeded', 'failed', 'cancelled', 'no_match')),
    CONSTRAINT voyage_targets_batch_index_non_negative
        CHECK (batch_index >= 0),
    CONSTRAINT voyage_targets_voyage_fk
        FOREIGN KEY (voyage_id) REFERENCES voyages (voyage_id) ON DELETE CASCADE
);

-- Drill по Leg-у: единицы одного batch_index в порядке исполнения.
CREATE INDEX voyage_targets_batch_idx
    ON voyage_targets (voyage_id, batch_index);

COMMENT ON TABLE voyages IS
    'Реестр Voyage-прогонов (унифицированный батчевый прогон, ADR-043, S1). Дискриминатор kind=scenario|command поглощает Tide+ErrandRun. PG-based claim+lease для failover-resilience: pending→running→terminal; протухший claim возвращается Reaper в pending. На S1 воркер config-gated OFF (фундамент без реального исполнения).';

COMMENT ON TABLE voyage_targets IS
    'Единицы Voyage-прогона (Leg-разбиение по batch_index, ADR-043, S1). target_kind=incarnation (kind=scenario, back-link apply_id) | sid (kind=command, back-link errand_id). Two-level drill для All-runs вида (S5).';
