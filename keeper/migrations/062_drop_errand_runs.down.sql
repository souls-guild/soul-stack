-- 062_drop_errand_runs.down.sql
--
-- Откат удаления ErrandRun (Wave 5 Pass 2): пересоздаёт реестр `errand_runs` и
-- back-link errands.errand_run_id ровно в форме миграции 057 (образец recreate).
-- Данные не восстанавливаются (forward-drop их удалил) — down лишь возвращает
-- схему.

CREATE TABLE errand_runs (
    errand_run_id          TEXT        PRIMARY KEY,
    module                 TEXT        NOT NULL,
    input                  JSONB       NOT NULL,
    target_resolved_souls  JSONB       NOT NULL,
    target_origin          JSONB,
    concurrency            INT         NOT NULL DEFAULT 50,
    on_failure             TEXT        NOT NULL DEFAULT 'continue',
    total_errands          INT         NOT NULL,
    current_done           INT         NOT NULL DEFAULT 0,
    status                 TEXT        NOT NULL DEFAULT 'pending',
    claimed_by_kid         TEXT,
    last_renewed_at        TIMESTAMPTZ,
    claim_expires_at       TIMESTAMPTZ,
    attempt                INT         NOT NULL DEFAULT 0,
    started_by_aid         TEXT        NOT NULL,
    started_at             TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    finished_at            TIMESTAMPTZ,
    summary                JSONB,

    CONSTRAINT errand_runs_status_valid
        CHECK (status IN ('pending', 'running', 'succeeded', 'failed', 'partial_failed', 'cancelled')),
    CONSTRAINT errand_runs_on_failure_valid
        CHECK (on_failure IN ('abort', 'continue')),
    CONSTRAINT errand_runs_concurrency_positive
        CHECK (concurrency >= 1 AND concurrency <= 500),
    CONSTRAINT errand_runs_total_positive
        CHECK (total_errands >= 1),
    CONSTRAINT errand_runs_done_bounds
        CHECK (current_done >= 0 AND current_done <= total_errands),
    CONSTRAINT errand_runs_attempt_positive
        CHECK (attempt >= 0),
    CONSTRAINT errand_runs_running_claim_consistency
        CHECK (
            (status <> 'running')
            OR (claimed_by_kid IS NOT NULL AND claim_expires_at IS NOT NULL)
        ),
    CONSTRAINT errand_runs_terminal_finished_at
        CHECK (
            (status NOT IN ('succeeded', 'failed', 'partial_failed', 'cancelled'))
            OR (finished_at IS NOT NULL)
        ),
    CONSTRAINT errand_runs_started_by_aid_fk
        FOREIGN KEY (started_by_aid) REFERENCES operators (aid)
);

CREATE INDEX errand_runs_pending_pickup_idx
    ON errand_runs (started_at)
    WHERE status = 'pending';

CREATE INDEX errand_runs_claim_scan_idx
    ON errand_runs (claim_expires_at)
    WHERE status = 'running';

ALTER TABLE errands ADD COLUMN IF NOT EXISTS errand_run_id TEXT;
ALTER TABLE errands ADD CONSTRAINT errands_errand_run_id_fkey
    FOREIGN KEY (errand_run_id) REFERENCES errand_runs (errand_run_id) ON DELETE CASCADE;

CREATE INDEX IF NOT EXISTS errands_errand_run_id_idx
    ON errands (errand_run_id)
    WHERE errand_run_id IS NOT NULL;
