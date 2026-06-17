-- 061_drop_tides.down.sql
--
-- Откат удаления Tide (Wave 5 Pass 1): пересоздаёт реестр `tides` и back-link-
-- колонки apply_runs ровно в форме миграции 055 (образец recreate). Данные не
-- восстанавливаются (forward-drop их удалил) — down лишь возвращает схему.

CREATE TABLE tides (
    tide_id                TEXT PRIMARY KEY,
    incarnation_name       TEXT NOT NULL,
    scenario_name          TEXT NOT NULL,
    total_surges           INT NOT NULL,
    current_surge_index    INT NOT NULL DEFAULT 0,
    surge_size             INT NOT NULL,
    target_resolved_souls  JSONB NOT NULL,
    target_coven_override  JSONB,
    target_where_override  TEXT,
    concurrency_override   INT,
    on_surge_failure       TEXT NOT NULL,
    status                 TEXT NOT NULL,
    current_apply_id       TEXT,
    claimed_by_kid         TEXT,
    last_renewed_at        TIMESTAMPTZ,
    claim_expires_at       TIMESTAMPTZ,
    attempt                INT NOT NULL DEFAULT 0,
    started_by_aid         TEXT NOT NULL,
    started_at             TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    finished_at            TIMESTAMPTZ,
    summary                JSONB,

    CONSTRAINT tides_total_surges_positive
        CHECK (total_surges > 0),
    CONSTRAINT tides_current_surge_index_non_negative
        CHECK (current_surge_index >= 0),
    CONSTRAINT tides_surge_size_positive
        CHECK (surge_size > 0),
    CONSTRAINT tides_concurrency_override_positive
        CHECK (concurrency_override IS NULL OR concurrency_override > 0),
    CONSTRAINT tides_on_surge_failure_valid
        CHECK (on_surge_failure IN ('abort', 'continue')),
    CONSTRAINT tides_status_valid
        CHECK (status IN ('pending', 'running', 'succeeded', 'failed', 'partial_failed', 'cancelled')),
    CONSTRAINT tides_running_claim_consistency
        CHECK (
            (status = 'running' AND claimed_by_kid IS NOT NULL AND claim_expires_at IS NOT NULL)
            OR status <> 'running'
        ),
    CONSTRAINT tides_surge_index_within_total
        CHECK (current_surge_index <= total_surges),
    CONSTRAINT tides_started_by_aid_fk
        FOREIGN KEY (started_by_aid) REFERENCES operators (aid)
);

CREATE INDEX tides_claim_scan_idx
    ON tides (claim_expires_at)
    WHERE status = 'running' AND claim_expires_at IS NOT NULL;

CREATE INDEX tides_pending_pickup_idx
    ON tides (started_at)
    WHERE status = 'pending';

ALTER TABLE apply_runs ADD COLUMN IF NOT EXISTS tide_id TEXT;
ALTER TABLE apply_runs ADD COLUMN IF NOT EXISTS surge_index INT;
CREATE INDEX IF NOT EXISTS apply_runs_tide_idx ON apply_runs (tide_id) WHERE tide_id IS NOT NULL;
