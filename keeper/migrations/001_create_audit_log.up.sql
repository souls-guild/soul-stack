-- 001_create_audit_log.up.sql
--
-- Audit-log таблица под ADR-022. Source of truth для всех write-path
-- инициаторов Keeper-а (HTTP-middleware Operator API, MCP-handler, Reaper,
-- hot-reload pipeline, keeper.cloud, keeper.push, bootstrap, Soul gRPC
-- event forwarder) через общий helper shared/audit.
--
-- FK audit_log.archon_aid → operators(aid) НЕ создаётся в M0.4.0 — таблица
-- operators появится в M0.4.x; отдельная миграция добавит constraint без
-- перезаписи этой схемы.

CREATE TABLE audit_log (
    audit_id        TEXT        PRIMARY KEY,                       -- ULID (26 chars, sortable timestamp prefix)
    created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    event_type      TEXT        NOT NULL,                          -- <area>.<action>, см. docs/naming-rules.md → Audit-events
    source          TEXT        NOT NULL,                          -- closed enum: signal | api | mcp | keeper_internal | soul_grpc
    archon_aid      TEXT,                                          -- nullable; FK на operators(aid) добавится отдельной миграцией
    correlation_id  TEXT,                                          -- ULID, nullable; reuse apply_id для source='soul_grpc'
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
