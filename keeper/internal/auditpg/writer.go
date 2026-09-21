// Package auditpg implements [audit.Writer] on top of pgxpool.Pool.
//
// Moved out of `shared/audit/` into `keeper/internal/auditpg/` by the M0.4.0
// architecture decision: importing `pgx/v5` pulls in ~1.5 MB of pgx code and
// `pgtype.init.0` registrations; including it in `shared/` would transitively
// drag them into the `soul` binary through the future
// `soul -> shared/config -> shared/audit` path, violating ADR-011 "Soul
// isolation is guaranteed by the compiler".
//
// `shared/audit/` stays pgx-free: types, the [audit.Writer] interface, the
// [audit.Source] enum, masking, and the ULID helper. Write-path implementations
// live in binary modules: this package in `keeper/internal/`, future ones
// (multi-writer + OTel dual-write, ADR-022(f)) there as well.
package auditpg

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/souls-guild/soul-stack/shared/audit"
)

// execer is the narrow subset of the pgxpool.Pool interface needed for
// INSERT INTO audit_log. Narrowing allows unit-testing the writer with a fake
// implementation without starting Postgres; the real pool from keeper/internal/pg
// satisfies the interface automatically.
type execer interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}

// pgxWriter is a Writer implementation over pgxpool.Pool (or compatible
// pgx.Conn in tests). One instance per Keeper process; safe for concurrent use
// because the pool itself provides thread safety.
type pgxWriter struct {
	pool execer
}

// NewWriter wraps an already initialized pgxpool.Pool as [audit.Writer].
// Ownership of the pool remains with the caller: the writer does not close the
// pool; lifecycle is keeper/internal/pg -> keeper/cmd/keeper.
func NewWriter(pool execer) audit.Writer {
	return &pgxWriter{pool: pool}
}

// insertSQL is a single INSERT into audit_log. Columns are strictly in
// ADR-022(a) order; audit_id is required (generated before Exec, otherwise PG
// cannot apply DEFAULT because the PK has none).
const insertSQL = `
INSERT INTO audit_log (audit_id, created_at, event_type, source, archon_aid, correlation_id, payload)
VALUES ($1, COALESCE($2, NOW()), $3, $4, $5, $6, $7)
`

// Write records an event in audit_log. Contract:
//
//   - event.EventType and event.Source are required; empty values return an
//     error without INSERT.
//   - event.Source is validated by [audit.Source.Valid], a closed enum per
//     ADR-022(b); casting an arbitrary string to audit.Source is caught here.
//   - empty event.AuditID generates [audit.NewULID].
//   - zero event.CreatedAt is passed as NULL, and PG sets DEFAULT NOW().
//   - empty event.ArchonAID / event.CorrelationID become NULL in the DB.
//   - nil event.Payload becomes empty JSONB `{}`; nan/inf/non-serializable
//     values are handed to json.Marshal as-is and propagated to the caller as
//     errors. This is the initiator's contract: payload must be JSON-serializable.
//   - Secrets in Payload are masked through [audit.MaskSecrets] before marshaling.
//
// pgxWriter does not retry any error; that is the caller's responsibility
// (Reaper loop / hot-reload handle failures differently).
func (w *pgxWriter) Write(ctx context.Context, event *audit.Event) error {
	if event == nil {
		return fmt.Errorf("audit: nil event")
	}
	if event.EventType == "" {
		return fmt.Errorf("audit: event_type is empty")
	}
	if !event.Source.Valid() {
		return fmt.Errorf("audit: invalid source %q", event.Source)
	}

	auditID := event.AuditID
	if auditID == "" {
		auditID = audit.NewULID()
	}

	var createdAt any
	if !event.CreatedAt.IsZero() {
		createdAt = event.CreatedAt.UTC()
	}

	var archonAID any
	if event.ArchonAID != "" {
		archonAID = event.ArchonAID
	}

	var correlationID any
	if event.CorrelationID != "" {
		correlationID = event.CorrelationID
	}

	payloadBytes, err := marshalPayload(event.Payload)
	if err != nil {
		return fmt.Errorf("audit: marshal payload: %w", err)
	}

	if _, err := w.pool.Exec(ctx, insertSQL,
		auditID,
		createdAt,
		string(event.EventType),
		string(event.Source),
		archonAID,
		correlationID,
		payloadBytes,
	); err != nil {
		return fmt.Errorf("audit: insert: %w", err)
	}
	return nil
}

// marshalPayload masks secrets and serializes payload into JSON bytes suitable
// for direct insertion into a JSONB column by pgx. nil-payload -> `[]byte("{}")`
// (valid JSONB, not NULL; the column is NOT NULL DEFAULT '{}'::jsonb).
func marshalPayload(payload map[string]any) ([]byte, error) {
	masked := audit.MaskSecrets(payload)
	if masked == nil {
		return []byte("{}"), nil
	}
	return json.Marshal(masked)
}

// compileTimeAssertExecer guarantees that *pgx.Conn satisfies the execer
// interface. It is not used at runtime; it catches pgx/v5 contract breakage on
// version upgrades. (`*pgxpool.Pool` is a separate candidate, checked through
// NewWriter initialization in keeper/cmd/keeper.)
var _ execer = (*pgx.Conn)(nil)
