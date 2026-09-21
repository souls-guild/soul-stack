// Operator API handler `GET /v1/audit` — read-only feed of audit events
// for UI iteration 2 (placeholder /audit). Read-only: the read itself is NOT
// written to audit_log (avoids recursion — every request would double the
// table). RBAC — `audit.read`, selector NoSelector (the archon_aid filter is
// passed as a query param, not an RBAC scope).
//
// T5d (handler-native): the audit domain is decoupled from the legacy generator.
// ListTyped returns a domain [AuditListPage] with FLAT wire fields; the native
// wire-DTO (OpenAPI schema + serialization) is built by package api from those
// fields (register func). The (w,r) wrapper is gone — HTTP is served by huma
// full-typed (api/huma_audit_endpoint.go).
package handlers

import (
	"context"
	"io"
	"log/slog"
	"time"

	"github.com/souls-guild/soul-stack/keeper/internal/api/problem"
	"github.com/souls-guild/soul-stack/keeper/internal/auditpg"
	"github.com/souls-guild/soul-stack/shared/api"
	"github.com/souls-guild/soul-stack/shared/audit"
)

// AuditHandler — `GET /v1/audit`. Delegates reads to [auditpg.Reader]
// (a narrow QueryRower client over `audit_log`). All dependencies are immutable;
// safe for concurrent use.
type AuditHandler struct {
	reader *auditpg.Reader
	logger *slog.Logger
}

// NewAuditHandler constructs the handler. reader is required; router.go wires
// the audit route only when reader is non-nil (the PushHandler/ErrandHandler
// pattern), so the handler does not validate nil deps itself.
func NewAuditHandler(reader *auditpg.Reader, logger *slog.Logger) *AuditHandler {
	if logger == nil {
		logger = slog.New(slog.NewJSONHandler(io.Discard, nil))
	}
	return &AuditHandler{reader: reader, logger: logger}
}

// AuditSpecStub — a non-nil *AuditHandler stub for generating the huma OpenAPI
// fragment (HumaAuditSpecYAML): on dump the domain handler is not called (huma.
// Register does not execute it), but the register function requires non-nil for
// its no-op check. reader/logger nil — the handler never executes in spec mode.
func AuditSpecStub() *AuditHandler {
	return &AuditHandler{logger: slog.New(slog.NewJSONHandler(io.Discard, nil))}
}

// AuditEventView — a FLAT domain projection of one audit_log row
// (element AuditListPage.Items), handler-native T5d. Package api projects it into
// the native AuditEvent schema (register func). ArchonAID/CorrelationID — `*string`
// (nil → key omitted in native wire); Source — RAW domain string (the native type
// in api holds the enum form); CreatedAt — already truncated to seconds (parity
// with the legacy wire). There is no `keeper_kid` field (migration 001 has no
// column; always-null — field omitted).
type AuditEventView struct {
	ArchonAID     *string
	CorrelationID *string
	CreatedAt     time.Time
	ID            string
	Payload       map[string]any
	Source        string
	Type          string
}

// AuditListPage — the domain result of `GET /v1/audit` (handler-native T5d).
// Package api projects {Items, Offset, Limit, Total} → native AuditEventListReply.
type AuditListPage struct {
	Items  []AuditEventView
	Offset int
	Limit  int
	Total  int
}

// AuditListFilter — the domain parameters of `GET /v1/audit` (typed query, the
// fourth tier of ADR-054). Symmetric with auditpg.ListFilter but also carries
// pagination (Offset/Limit) and is decoupled from the read-side layer: the huma
// handler (huma_audit_endpoint.go) and the legacy (w,r) wrapper both assemble
// THIS one type, and ListTyped is the single domain function. Empty string/slice
// fields = "do not apply the filter"; zero-time StartedAfter/Before = "no time
// bound" (parity with legacy `if param != ""`).
type AuditListFilter struct {
	Types         []string
	Sources       []string
	ArchonAID     string
	CorrelationID string
	PayloadHerald string
	PayloadVoyage string
	StartedAfter  time.Time
	StartedBefore time.Time
	Offset        int
	Limit         int
}

// ListTyped — the domain function of `GET /v1/audit` (ADR-054 §Pattern step 2):
// validates the source enum, builds auditpg.ListFilter, reads a page via Reader,
// projects into the typed envelope {items, offset, limit, total}. No
// http.ResponseWriter/*http.Request — shared code for the huma handler and the
// legacy (w,r) wrapper.
//
// Errors are *problemError (delivered by both paths through AsProblemDetails with
// the same error contract): invalid source → 422 TypeValidationFailed (the
// string-enum semantics stay 422 even when the huma query enum would already have
// rejected it with 422 — defense in depth for a direct call); DB failure → 500
// TypeInternalError.
func (h *AuditHandler) ListTyped(ctx context.Context, f AuditListFilter) (AuditListPage, error) {
	var zero AuditListPage
	if h.reader == nil {
		return zero, &problemError{problem.New(problem.TypeInternalError, "", "audit reader is not configured")}
	}

	// Pagination range (offset≥0, limit∈[1,1000]) — a SINGLE source of bounds,
	// api.CheckPageBounds (same as ParsePage). Out-of-range → 400
	// TypeMalformedRequest (contract invariant: the huma typed-int carries NO
	// schema minimum/maximum, otherwise it would return 422 — a wire change vs the
	// legacy/strict 400).
	if err := api.CheckPageBounds(f.Offset, f.Limit); err != nil {
		return zero, &problemError{problem.New(problem.TypeMalformedRequest, "", err.Error())}
	}

	for _, s := range f.Sources {
		if !audit.Source(s).Valid() {
			return zero, &problemError{problem.New(problem.TypeValidationFailed, "",
				"query 'source' must be one of signal/api/mcp/keeper_internal/soul_grpc/background/config_bootstrap")}
		}
	}

	filter := auditpg.ListFilter{
		ArchonAID:     f.ArchonAID,
		CorrelationID: f.CorrelationID,
		PayloadHerald: f.PayloadHerald,
		PayloadVoyage: f.PayloadVoyage,
		StartedAfter:  f.StartedAfter,
		StartedBefore: f.StartedBefore,
	}
	if len(f.Types) > 0 {
		filter.Types = f.Types
	}
	if len(f.Sources) > 0 {
		filter.Sources = f.Sources
	}

	rows, total, err := h.reader.List(ctx, filter, f.Offset, f.Limit)
	if err != nil {
		h.logger.Error("audit.list: reader failed",
			slog.Int("offset", f.Offset),
			slog.Int("limit", f.Limit),
			slog.Any("error", err))
		return zero, &problemError{problem.New(problem.TypeInternalError, "", "list audit failed")}
	}

	items := make([]AuditEventView, 0, len(rows))
	for _, row := range rows {
		items = append(items, auditEventView(row))
	}

	return AuditListPage{
		Items:  items,
		Offset: f.Offset,
		Limit:  f.Limit,
		Total:  total,
	}, nil
}

// auditEventView projects [auditpg.Row] into the flat domain [AuditEventView].
// Source — RAW domain string ([audit.Source]); CreatedAt — UTC, truncated to
// seconds (the former `.Format(time.RFC3339)` also dropped the fractional part —
// a second-granular wire). ArchonAID / CorrelationID — pointer-optional (nil →
// field omitted by the native type).
func auditEventView(row *auditpg.Row) AuditEventView {
	return AuditEventView{
		ID:            row.AuditID,
		Type:          row.EventType,
		Source:        string(row.Source),
		ArchonAID:     row.ArchonAID,
		CorrelationID: row.CorrelationID,
		CreatedAt:     row.CreatedAt.UTC().Truncate(time.Second),
		Payload:       row.Payload,
	}
}
