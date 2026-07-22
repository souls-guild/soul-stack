-- 039_create_incarnation_archive.up.sql
--
-- S-D3 (incarnation.destroy, cascade V3): archive tables for the physical removal
-- of an incarnation row. User decision - do NOT keep a tombstone in the live
-- registry, instead copy the compliance-minimum into separate archive tables
-- BEFORE DELETE, after which DELETE FROM incarnation cascades to remove live
-- state_history / apply_runs / apply_task_register. The archive survives the
-- cascade because it has NO FK to the live incarnation.
--
-- Two tables:
--   * incarnation_archive    - a snapshot of the key incarnation columns at the
--     moment of destroy (name / service / version / spec / state / status +
--     timestamps) + archived_at.
--   * state_history_archive   - a snapshot of the state_history log of the
--     removed incarnation (the transition history matters for compliance) +
--     archived_at.
--
-- Why archive tables instead of a tombstone flag on incarnation: the live
-- registry stays clean (the status enum does not grow a "deleted" value, FK
-- integrity of apply_runs/state_history does not hang off a dead row), while
-- compliance data is retained indefinitely (a separate Reaper retention rule
-- for the archive is backlog).
--
-- FK invariant: archive tables do NOT reference incarnation(name) - otherwise
-- deleting the parent would either fail (RESTRICT) or wipe out the archive row
-- just written (CASCADE). created_by_aid / changed_by_aid are also NOT FKs to
-- operators: the archive is a frozen snapshot, it is not obligated to outlive
-- the referential integrity of the operator registry (AID is stored as a plain
-- string value for audit purposes, not as a live reference).
--
-- name is NOT a PK: the same incarnation name can be re-created and destroyed
-- again (repeated destroy of the same name) - the archive accumulates all
-- incarnations of that name. The PK is a surrogate IDENTITY (archive_id),
-- unique per row.

CREATE TABLE incarnation_archive (
    archive_id            BIGINT      GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    name                  TEXT        NOT NULL,
    service               TEXT        NOT NULL,
    service_version       TEXT        NOT NULL,
    state_schema_version  INTEGER     NOT NULL,
    spec                  JSONB       NOT NULL DEFAULT '{}'::jsonb,
    state                 JSONB       NOT NULL DEFAULT '{}'::jsonb,
    status                TEXT        NOT NULL,
    status_details        JSONB,
    created_by_aid        TEXT,
    created_at            TIMESTAMPTZ NOT NULL,
    updated_at            TIMESTAMPTZ NOT NULL,
    archived_at           TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

-- Lookup of the archive by the name of a removed incarnation (compliance query
-- "what did redis-prod look like before deletion").
CREATE INDEX incarnation_archive_name_idx
    ON incarnation_archive (name);

COMMENT ON TABLE incarnation_archive IS
    'Archive of removed incarnations (S-D3, cascade V3). NO FK to the live incarnation - survives DELETE.';

CREATE TABLE state_history_archive (
    archive_id         BIGINT      GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    history_id         TEXT        NOT NULL,
    incarnation_name   TEXT        NOT NULL,
    scenario           TEXT        NOT NULL,
    state_before       JSONB       NOT NULL DEFAULT '{}'::jsonb,
    state_after        JSONB       NOT NULL DEFAULT '{}'::jsonb,
    changed_by_aid     TEXT,
    apply_id           TEXT        NOT NULL,
    at                 TIMESTAMPTZ NOT NULL,
    archived_at        TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

-- Archived history feed of state_history for a specific removed incarnation.
CREATE INDEX state_history_archive_incarnation_idx
    ON state_history_archive (incarnation_name);

COMMENT ON TABLE state_history_archive IS
    'Archive of the state_history log for removed incarnations (S-D3). NO FK to the live incarnation.';
