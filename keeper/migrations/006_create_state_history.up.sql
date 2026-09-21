-- 006_create_state_history.up.sql
--
-- Change log for `incarnation.state` under ADR-009 / ADR-019. Snapshot
-- per-change: every state change writes state_before / state_after
-- + scenario / apply_id / changed_by_aid / at.
--
-- PK = history_id (ULID, same format as audit_log.audit_id and apply_id).
-- Foreign keys:
--   - incarnation_name -> incarnation(name) ON DELETE CASCADE (the log
--     dies together with the incarnation; recovery from an unstructured backup).
--   - changed_by_aid   -> operators(aid)   ON DELETE SET NULL (history
--     survives operator deletion).

CREATE TABLE state_history (
    history_id         TEXT        PRIMARY KEY,
    incarnation_name   TEXT        NOT NULL,
    scenario           TEXT        NOT NULL,
    state_before       JSONB       NOT NULL DEFAULT '{}'::jsonb,
    state_after        JSONB       NOT NULL DEFAULT '{}'::jsonb,
    changed_by_aid     TEXT,
    apply_id           TEXT        NOT NULL,
    at                 TIMESTAMPTZ NOT NULL DEFAULT NOW(),

    CONSTRAINT state_history_incarnation_fk
        FOREIGN KEY (incarnation_name) REFERENCES incarnation (name) ON DELETE CASCADE,
    CONSTRAINT state_history_changed_by_aid_fk
        FOREIGN KEY (changed_by_aid) REFERENCES operators (aid) ON DELETE SET NULL
);

-- Typical query - history feed of a specific incarnation in reverse
-- chronological order (GET /v1/incarnations/{name}/history).
CREATE INDEX state_history_incarnation_at_idx
    ON state_history (incarnation_name, at DESC);

-- Lookup a record by apply_id (polling the status of an async operation from
-- POST /v1/incarnations).
CREATE INDEX state_history_apply_id_idx
    ON state_history (apply_id);

COMMENT ON TABLE state_history IS
    'Snapshot per-change log of incarnation.state (ADR-009 / ADR-019).';
