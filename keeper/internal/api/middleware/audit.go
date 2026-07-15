package middleware

import (
	"context"
	"log/slog"
	"net/http"

	"github.com/souls-guild/soul-stack/shared/audit"
)

// AuditPayloadBuilder — a function that assembles an audit event's payload
// after the handler runs successfully. It receives claims (via
// [ClaimsFromContext]), the request, and the response status, and returns a map for
// `Event.Payload`. Secret masking is done by the audit.Writer side
// (via MaskSecrets); raw values may be placed here.
//
// nil return → empty payload (the event is written, payload = `{}`).
//
// Via a builder rather than capturing the response body, to avoid duplicating
// the streaming logic of ResponseRecorders: it's cheaper for the handler to place
// the needed fields directly in the context before next() or return them from a local
// closed-over variable.
type AuditPayloadBuilder func(r *http.Request, status int) map[string]any

// auditCtxKey — non-exported context key for handler payload overrides.
type auditCtxKey struct{}

// AuditPayload — a payload the handler wants to attach to an audit event.
// Used via [SetAuditPayload] and merged with what the builder returns
// in [Audit].
type AuditPayload map[string]any

// SetAuditPayload places the payload in the context. The handler calls it after
// running successfully; the audit middleware reads it in the after-handler phase.
//
// Idempotent overwrite: a repeat call replaces the previous value entirely.
func SetAuditPayload(r *http.Request, payload AuditPayload) {
	ctx := context.WithValue(r.Context(), auditCtxKey{}, payload)
	*r = *r.WithContext(ctx)
}

// auditPayloadFromContext returns the payload placed by the handler.
// nil — the handler didn't call SetAuditPayload.
func auditPayloadFromContext(ctx context.Context) AuditPayload {
	p, _ := ctx.Value(auditCtxKey{}).(AuditPayload)
	return p
}

// HumaAuditCarrier — a mutable carrier of the audit payload for full-typed huma routes
// (ADR-054 §Audit, variant B). huma middleware runs OUTSIDE the handler closure,
// and huma.Context is immutable (huma.WithValue creates a new context), so the
// classic [SetAuditPayload] (which does *r.WithContext on *http.Request) is inapplicable
// to huma. Instead the huma-audit-middleware seeds a *HumaAuditCarrier into the
// request context BEFORE next; the huma handler places the payload via [SetHumaAuditPayload];
// the middleware reads the SAME pointer after next. The pointer is shared → the mutation is visible.
type HumaAuditCarrier struct {
	Payload AuditPayload
}

// HumaAuditCarrierKey — the context key for [HumaAuditCarrier]. Exported: the huma-
// audit-middleware (package api) seeds the carrier by it, [SetHumaAuditPayload]
// (the same middleware package) reads by it.
type HumaAuditCarrierKey struct{}

// SetHumaAuditPayload places the audit payload on the huma context (the parallel of
// [SetAuditPayload] for full-typed huma routes, ADR-054 §Audit). The huma handler
// calls it inside its closure; the huma-audit-middleware seeds the carrier BEFORE next
// and reads the payload after. carrier absent (route without huma-audit wiring / a direct
// handler call) → no-op (the payload isn't recorded, audit is written without it).
//
// Idempotent overwrite: a repeat call replaces the previous value.
func SetHumaAuditPayload(ctx context.Context, payload AuditPayload) {
	if c, ok := ctx.Value(HumaAuditCarrierKey{}).(*HumaAuditCarrier); ok {
		c.Payload = payload
	}
}

// sourceCtxKey — non-exported context key for the audit source passed into a
// REST handler bypassing the HTTP router (MCP tools call the handler in-memory
// via httptest, bypassing the Operator-API chain where source = api by default).
type sourceCtxKey struct{}

// WithScenarioInvocationSource places the audit source in the context for REST handlers
// called not from the HTTP router but directly (an MCP tool via httptest). The handler
// reads the value via [ScenarioInvocationSource] and writes it into the audit event
// instead of the default [audit.SourceAPI].
//
// Symmetric to [SetAuditPayload]: the same in-handler-context idiom for audit
// metadata, only source is set BEFORE the handler call (caller side), not
// inside it (handler side).
func WithScenarioInvocationSource(ctx context.Context, source audit.Source) context.Context {
	return context.WithValue(ctx, sourceCtxKey{}, source)
}

// ScenarioInvocationSource returns the audit source from the context. Fallback —
// [audit.SourceAPI]: a normal HTTP request through the Operator-API chain doesn't set
// the key, and source stays `api` (behavior preserved). An MCP tool
// sets [audit.SourceMCP] via [WithScenarioInvocationSource].
func ScenarioInvocationSource(ctx context.Context) audit.Source {
	if s, ok := ctx.Value(sourceCtxKey{}).(audit.Source); ok && s.Valid() {
		return s
	}
	return audit.SourceAPI
}

// StatusRecorder — a wrap for http.ResponseWriter that records the status
// actually written by the handler. Needed by the audit middleware to know
// success/failure after ServeHTTP without modifying handler code.
//
// Exported so the strict-layer [bridgeMiddleware] (package api) can wrap the
// incoming writer in the SAME recorder that Audit reads, and place it in the bridge
// context (br.w) — otherwise a domain handler writing into br.w bypasses Audit's
// recorder, and rec.status stays 0 (S6 regression: audit silently isn't written on
// strict routes). The constructor [NewStatusRecorder] + ctx passing
// [WithStatusRecorder]/[StatusRecorderFromContext] share ONE object between
// the bridge (created it, gave it to the domain handler) and Audit (reads status from ctx).
//
// Does not buffer the body — those are potentially large payloads (instance
// lists, JWT tokens). Audit writes only status + handler-overridden
// payload.
type StatusRecorder struct {
	http.ResponseWriter
	status      int
	wroteHeader bool
}

// NewStatusRecorder wraps w. The idempotent WriteHeader (see below)
// prevents a double WriteHeader when bridge+Audit share it.
func NewStatusRecorder(w http.ResponseWriter) *StatusRecorder {
	return &StatusRecorder{ResponseWriter: w}
}

// Status returns the recorded status (0 — nothing written: panic /
// early rejection before WriteHeader/Write).
func (s *StatusRecorder) Status() int { return s.status }

func (s *StatusRecorder) WriteHeader(code int) {
	if !s.wroteHeader {
		s.status = code
		s.wroteHeader = true
	}
	s.ResponseWriter.WriteHeader(code)
}

func (s *StatusRecorder) Write(b []byte) (int, error) {
	if !s.wroteHeader {
		// stdlib does an implicit WriteHeader(200) before the first Write.
		s.status = http.StatusOK
		s.wroteHeader = true
	}
	return s.ResponseWriter.Write(b)
}

// NIM-37: SSE flush passthrough
func (s *StatusRecorder) Unwrap() http.ResponseWriter { return s.ResponseWriter }

func (s *StatusRecorder) Flush() {
	if f, ok := s.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// recorderCtxKey — non-exported context key for the shared [StatusRecorder]
// that the bridge (strict layer) and Audit share.
type recorderCtxKey struct{}

// WithStatusRecorder places the recorder in the context. Called by the strict-layer
// [bridgeMiddleware]: it wraps the incoming writer in a recorder, gives THAT recorder
// to the domain handler (via br.w) and places it in ctx — Audit further down the chain
// reads from ctx the status actually written by the domain handler.
func WithStatusRecorder(ctx context.Context, rec *StatusRecorder) context.Context {
	return context.WithValue(ctx, recorderCtxKey{}, rec)
}

// StatusRecorderFromContext returns the recorder placed by [WithStatusRecorder]
// (nil — no key: a non-strict route / a direct handler call). When non-nil, Audit
// reads the status from IT, not from its own rec (which on a strict route would stay
// 0 — the domain handler writes into br.w=this recorder, bypassing Audit's wrapper).
func StatusRecorderFromContext(ctx context.Context) *StatusRecorder {
	rec, _ := ctx.Value(recorderCtxKey{}).(*StatusRecorder)
	return rec
}

// Audit — a middleware factory that writes an audit event with eventType after
// the handler runs successfully.
//
// Contract:
//   - Must come after [RequireJWT] (needs claims.Subject for archon_aid).
//   - Must come after [RequirePermission] (audit is written only on
//     requests that passed the RBAC check; otherwise the writer would flood audit_log
//     with unauthorized-attempt events — a separate channel, post-MVP).
//   - source = api (rbac.md → § Usage). archon_aid = claims.Subject.
//
// builder is called only on a "success" status (2xx); 4xx/5xx are
// skipped (RBAC-deny / validation-error / internal-error go to
// logs and the problem response, not to audit). On status≥300 no event is
// recorded.
//
// A writer.Write error is logged via logger and does NOT affect the response
// (the response already left for the client — too late). The audit pipeline is
// best-effort at this level (durability comes from the PG COMMIT inside
// auditpg.Writer).
func Audit(writer audit.Writer, eventType audit.EventType, builder AuditPayloadBuilder, logger *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// On strict routes bridgeMiddleware has ALREADY wrapped the writer in a shared
			// StatusRecorder, placed it in br.w (the domain handler writes into it)
			// and in ctx. Then we read the status from IT — our own wrapper would see
			// 0 (the handler writes past it into br.w). On non-strict routes there's no recorder
			// in ctx — we wrap it ourselves (legacy path preserved).
			rec := StatusRecorderFromContext(r.Context())
			if rec == nil {
				rec = NewStatusRecorder(w)
			}
			next.ServeHTTP(rec, r)

			if rec.status >= 300 || rec.status == 0 {
				// 0 = handler wrote nothing (panic, early rejection).
				// 4xx/5xx — operation not performed, we don't write to audit_log.
				return
			}

			claims, ok := ClaimsFromContext(r.Context())
			if !ok {
				logger.Warn("audit middleware: missing claims in context",
					slog.String("path", r.URL.Path),
				)
				return
			}

			payload := mergeAuditPayload(builder, r, rec.status, auditPayloadFromContext(r.Context()))

			ev := &audit.Event{
				EventType: eventType,
				Source:    audit.SourceAPI,
				ArchonAID: claims.Subject,
				Payload:   payload,
			}
			// Using r.Context() directly is dangerous: the HTTP server
			// may cancel it right after the response is written (the client
			// dropped the connection). Audit must not be lost for this
			// reason → we use Background. Trade-off: on shutdown the
			// 10s grace won't cover an audit write longer than the grace; for
			// the MVP single-transaction INSERT that's fine.
			if err := writer.Write(context.Background(), ev); err != nil {
				logger.Error("audit middleware: write failed",
					slog.String("event_type", string(eventType)),
					slog.String("archon_aid", claims.Subject),
					slog.Any("error", err),
				)
			}
		})
	}
}

// mergeAuditPayload merges the builder payload with the payload override
// from the handler context (handler overrides win). nil/nil → nil.
func mergeAuditPayload(builder AuditPayloadBuilder, r *http.Request, status int, override AuditPayload) map[string]any {
	var base map[string]any
	if builder != nil {
		base = builder(r, status)
	}
	if override == nil {
		return base
	}
	if base == nil {
		return map[string]any(override)
	}
	for k, v := range override {
		base[k] = v
	}
	return base
}
