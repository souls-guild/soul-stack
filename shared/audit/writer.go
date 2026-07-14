package audit

import "context"

// Writer abstracts the audit pipeline's write path. It takes an [Event],
// fills zero fields (AuditID, CreatedAt), masks secrets in Payload, and
// commits the event to the backing store.
//
// Implementations:
//   - keeper/internal/auditpg.NewWriter — the Postgres impl. Lives in the
//     keeper module so the pgx dependency does not leak transitively into
//     the soul binary via shared/ (ADR-011 "Soul isolation is guaranteed by
//     the compiler").
//   - A future multi-writer for OTel dual-write (M0.4.1, ADR-022(f)) and a
//     noop writer (for unit tests of initiators) — same place, through the
//     same interface without breaking changes.
type Writer interface {
	Write(ctx context.Context, event *Event) error
}
