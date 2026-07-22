-- 052_create_errands.up.sql
--
-- Registry of Errands (`POST /v1/souls/{sid}/exec`, MCP `keeper.errand.run`,
-- ADR-033): one row = one pull-ad-hoc exec of a single module on a specific
-- Soul over mTLS EventStream. A table separate from `apply_runs` (that's for per-host
-- apply scenarios with state_changes/barrier) and from `push_runs` (that's for multi-host
-- ad-hoc destiny over SSH); an Errand is single-host, without incarnation/scenario.
--
-- Fields:
--   - `errand_id`         - ULID, PK.
--   - `sid`               - target Soul.
--   - `module`            - fully-qualified `<ns>.<name>.<state>` (whitelist
--                            is checked both Keeper-side on receipt and
--                            as defense-in-depth Soul-side by the errand runner).
--   - `input`             - the module's input JSON object (jsonb).
--   - `status`            - terminal state (CHECK below).
--   - `exit_code`         - the verb module's exit code (NULL for read-safe non-shell).
--   - `stdout`/`stderr`   - captured output (cap 64 KiB, masked).
--   - `*_truncated`       - flags for exceeding the cap (for UI/observability).
--   - `duration_ms`       - the Errand's duration Soul-side.
--   - `error_message`     - masked reason for FAILED/TIMED_OUT/MODULE_NOT_ALLOWED.
--   - `output`            - structured output of read-safe modules (jsonb); for
--                            shell/exec - NULL.
--   - `started_by_aid`    - initiating Archon (FK to operators).
--   - `started_by_kid`    - KID of the Keeper instance (for a future sweep
--                            of orphaned running Errands on restart).
--   - `started_at`        - when Keeper accepted the request.
--   - `finished_at`       - terminal state (NULL while running).
--   - `ttl_at`            - `started_at + reaper.errands.ttl` (default 7d);
--                            used by the reaper rule `purge_old_errands`.
--
-- Statuses (CHECK): parity with the ErrandStatus enum in proto/keeper/v1/errand.proto.
--   running              - recorded, awaiting an ErrandResult from the Soul (or an async cap).
--   success              - ErrandResult{status:SUCCESS}.
--   failed               - ErrandResult{status:FAILED}.
--   timed_out            - ErrandResult{status:TIMED_OUT}.
--   cancelled            - ErrandResult{status:CANCELLED} (slice E5).
--   module_not_allowed   - the module failed the whitelist (defense-in-depth Soul-side).
--
-- Indexes:
--   - errands_running_idx - partial on running, a cheap sweep of orphaned
--     entries on Keeper restart (slice E2/E4).
--   - errands_sid_started_idx - the list API `GET /v1/errands?sid=<sid>` (E2).
--   - errands_ttl_idx - `purge_old_errands` (E4).
--
-- FK to operators: ON DELETE RESTRICT - Errand history requires a valid AID
-- for the initiator (revoking an Archon shouldn't break the audit link; a revoked Archon
-- stays in `operators` with `revoked_at`, the FK is preserved).

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
    'Registry of pull-ad-hoc Errands (ADR-033). One row = one POST /v1/souls/{sid}/exec.';
