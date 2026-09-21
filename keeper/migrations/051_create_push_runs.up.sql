-- 051_create_push_runs.up.sql
--
-- Registry of push runs (`POST /v1/push/apply`, MCP `keeper.push.apply`):
-- one row = one async run of the Variant C orchestrator (keeper/internal/
-- pushorch). A separate table from `apply_runs` - that one is per-(apply_id, sid) with
-- the pull semantics of the Soul EventStream; here - per-apply_id with inventory[] and
-- a per-host summary in jsonb (push is a synchronous oneshot, it doesn't go through the
-- apply_runs barrier).
--
-- Fields:
--   - `apply_id`         - ULID, PK.
--   - `inventory_sids`   - array of push-host SIDs (resolved from the request).
--   - `destiny_ref`      - "<name>@<git-ref>" (as in the request).
--   - `ssh_provider`     - SshProvider name from keeper.yml::plugins.ssh_providers[]
--                          ("" - registry default).
--   - `input`            - destiny input for rendering (jsonb).
--   - `cleanup_stale`    - flag for extra cleanup on hosts after success.
--   - `status`           - run terminal state (see CHECK below).
--   - `started_at`       - when the orchestrator accepted the request.
--   - `finished_at`      - when all per-host tasks finished (NULL while running).
--   - `started_by_aid`   - initiating Archon (FK to operators).
--   - `started_by_kid`   - KID of the Keeper instance (for Reaper purge_orphan_push_runs).
--   - `summary`          - per-host map sid -> {status, error?, run_status?} (jsonb).
--
-- Statuses (CHECK):
--   pending           - recorded, not yet picked up by a goroutine
--   running           - a goroutine has started per-host dispatch
--   success           - all per-host runs succeeded
--   failed            - all per-host runs failed
--   partial_failed    - some succeeded, some failed
--   cancelled         - terminated by Reaper purge_orphan_push_runs (Keeper died mid-run)
--
-- Indexes:
--   - status partial indexes (in-flight only): cheap for WHERE status IN
--     ('pending','running') - purge_orphan_push_runs and viewing active runs.
--   - started_by_kid - Reaper filters by kid (whether its own instance is alive).
--
-- FK to operators: ON DELETE SET NULL - we don't lose the run history when an
-- Archon is revoked; archon_aid stays NULL (the audit trail in audit_log is preserved).

CREATE TABLE push_runs (
    apply_id          TEXT        PRIMARY KEY,
    inventory_sids    TEXT[]      NOT NULL,
    destiny_ref       TEXT        NOT NULL,
    ssh_provider      TEXT,
    input             JSONB       NOT NULL DEFAULT '{}'::jsonb,
    cleanup_stale     BOOLEAN     NOT NULL DEFAULT FALSE,
    status            TEXT        NOT NULL,
    started_at        TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    finished_at       TIMESTAMPTZ,
    started_by_aid    TEXT,
    started_by_kid    TEXT        NOT NULL,
    summary           JSONB,
    CONSTRAINT push_runs_status_valid CHECK (status IN
        ('pending','running','success','partial_failed','failed','cancelled')),
    CONSTRAINT push_runs_started_by_aid_fk
        FOREIGN KEY (started_by_aid) REFERENCES operators (aid) ON DELETE SET NULL
);

CREATE INDEX push_runs_status_idx
    ON push_runs (status)
    WHERE status IN ('pending', 'running');

CREATE INDEX push_runs_started_by_kid_idx
    ON push_runs (started_by_kid)
    WHERE status IN ('pending', 'running');

COMMENT ON TABLE push_runs IS
    'Registry of keeper.push async runs (Variant C orchestrator). One row = one POST /v1/push/apply.';
