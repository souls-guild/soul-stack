-- 001_create_audit_log.up.sql
--
-- Audit-log table per ADR-022. Source of truth for all write-path
-- initiators of the Keeper (HTTP middleware Operator API, MCP handler, Reaper,
-- hot-reload pipeline, keeper.cloud, keeper.push, bootstrap, Soul gRPC
-- event forwarder) via the shared helper shared/audit.
--
-- FK audit_log.archon_aid -> operators(aid) is NOT created in M0.4.0 - the
-- operators table will appear in M0.4.x; a separate migration will add the constraint without
-- rewriting this schema.

CREATE TABLE audit_log (
    audit_id        TEXT        PRIMARY KEY,                       -- ULID (26 chars, sortable timestamp prefix)
    created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    event_type      TEXT        NOT NULL,                          -- <area>.<action>, see docs/naming-rules.md -> Audit-events
    source          TEXT        NOT NULL,                          -- closed enum: signal | api | mcp | keeper_internal | soul_grpc
    archon_aid      TEXT,                                          -- nullable; FK to operators(aid) will be added by a separate migration
    correlation_id  TEXT,                                          -- ULID, nullable; reuse apply_id for source='soul_grpc'
    payload         JSONB       NOT NULL DEFAULT '{}'::jsonb
);

CREATE INDEX audit_log_event_type_created_at_idx
    ON audit_log (event_type, created_at DESC);

CREATE INDEX audit_log_archon_aid_created_at_idx
    ON audit_log (archon_aid, created_at DESC)
    WHERE archon_aid IS NOT NULL;

CREATE INDEX audit_log_correlation_id_idx
    ON audit_log (correlation_id)
    WHERE correlation_id IS NOT NULL;
