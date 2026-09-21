// Package audit — the structured audit log of Soul Stack: canonical EventType names
// (ADR-022), payload serialization to jsonb, secret masking via MaskSecrets (see mask.go).
//
// The canon for event wire-format names is docs/naming-rules.md → Audit-events. Every
// event is written through the audit.Writer interface; for tests there's an in-memory
// fake (the audittest package).
package audit

import "time"

// Event — one record of the audit pipeline. Corresponds to a row in `audit_log` in
// Postgres (ADR-022(a)).
//
// Populated by the write-path initiator (HTTP middleware, MCP handler, hot-reload
// pipeline, Reaper, bootstrap, Soul gRPC forwarder) and passed to `Writer.Write` (see
// [writer.go]). The [Writer] implementation (`keeper/internal/auditpg`) masks secrets in
// `Payload` via [MaskSecrets] and fills zero fields (`AuditID`, `CreatedAt`) before INSERT.
type Event struct {
	// AuditID — ULID (26 chars). If empty, the write-path implementation generates one
	// via [NewULID] before INSERT.
	AuditID string

	// EventType — `<area>.<action>` (see [EventType]).
	EventType EventType

	// Source — who initiated the event (closed enum [Source]).
	Source Source

	// ArchonAID — the AID of the Archon that initiated the event. An empty string means
	// "not applicable" (for `signal` / `keeper_internal` / `soul_grpc`) — the write-path
	// implementation writes NULL.
	ArchonAID string

	// CorrelationID — ULID of a chain of related events. Optional (see ADR-022(c)). An
	// empty string → NULL in `audit_log.correlation_id`.
	CorrelationID string

	// Payload — kind-specific payload for `audit_log.payload` (JSONB). May be nil — the
	// write-path implementation writes `'{}'::jsonb`. Secrets in Payload (by the
	// known-keys list and `vault:`-prefix value) are masked before INSERT — see [MaskSecrets].
	Payload map[string]any

	// CreatedAt — when the event occurred. Zero-value → Postgres DEFAULT `NOW()`
	// (via a nil parameter in INSERT).
	CreatedAt time.Time
}
