package api

// huma-audit-middleware (VARIANT B, ADR-054 §Audit) + a generic spec-dump helper.
// The KEY rollout pattern for middleware-audit domains (role + operator + souls + synod
// + service + sigil + augur + oracle …): domains whose audit was written by
// [apimiddleware.Audit] (StatusRecorder in the bridge) + the handler's
// [apimiddleware.SetAuditPayload] CANNOT write audit that way on full-typed huma —
// huma writes the response ITSELF via its own Context (chiContext.SetStatus → w.WriteHeader),
// bypassing the [apimiddleware.Audit] StatusRecorder wrapper → rec.status==0 → audit silently
// not written (a recurrence of the S6 regression). The fix — a huma-native middleware that reads
// the status from huma-Context.Status() (a *chiContext field, NOT http.ResponseWriter):
// hctx.Status() is available natively AFTER next, early-flush does not break it, no fallback
// wrapper is needed. Payload is passed handler → middleware via a MUTABLE
// carrier in the request-context (seeded BEFORE next, read from THE SAME pointer after next).
// Full analysis of the Status()/carrier spike — ADR-054 §Audit.

import (
	"context"
	"log/slog"

	"github.com/danielgtaylor/huma/v2"
	"github.com/go-chi/chi/v5"

	apimiddleware "github.com/souls-guild/soul-stack/keeper/internal/api/middleware"
	"github.com/souls-guild/soul-stack/shared/audit"
)

// humaAuditMiddleware — huma-native audit middleware (variant B). Attached to
// huma.API via api.UseMiddleware UNDER a chi group already carrying RequireJWT/
// RequirePermission (RequireJWT puts claims into the request-context BEFORE humachi).
//
// Contract (parity apimiddleware.Audit):
//   - calls next(hctx) with a seeded payload carrier;
//   - AFTER next it reads hctx.Status(): status>=300 || status==0 → skip (4xx/5xx —
//     operation not performed; 0 — panic/early rejection before SetStatus). 2xx → write;
//   - claims from hctx.Context() (apimiddleware.ClaimsFromContext); no claims →
//     warn + skip (parity Audit);
//   - payload from the carrier (the handler set it via SetHumaAuditPayload); empty → write
//     the event with a nil payload (like Audit without a builder/override);
//   - writer.Write(ctx, &audit.Event{EventType:evt, Source:SourceAPI,
//     ArchonAID:claims.Subject, Payload}). ctx — Background (parity Audit: the request
//     ctx may be canceled right after the response — audit must not be lost).
//
// writer nil (dev without audit) → the middleware is transparent (just next). A
// writer.Write error is logged and does not affect the response (already sent) — best-effort, like
// apimiddleware.Audit.
func humaAuditMiddleware(writer audit.Writer, evt audit.EventType, logger *slog.Logger) func(huma.Context, func(huma.Context)) {
	return func(hctx huma.Context, next func(huma.Context)) {
		if writer == nil {
			next(hctx)
			return
		}

		carrier := &apimiddleware.HumaAuditCarrier{}
		hctx = huma.WithValue(hctx, apimiddleware.HumaAuditCarrierKey{}, carrier)

		next(hctx)

		status := hctx.Status()
		if status >= 300 || status == 0 {
			// 4xx/5xx — operation not performed; 0 — the handler did not reach SetStatus
			// (panic / early huma rejection). Do not write to audit_log (parity Audit).
			return
		}

		claims, ok := apimiddleware.ClaimsFromContext(hctx.Context())
		if !ok {
			if logger != nil {
				logger.Warn("huma audit middleware: missing claims in context",
					slog.String("path", hctx.URL().Path),
				)
			}
			return
		}

		ev := &audit.Event{
			EventType: evt,
			Source:    audit.SourceAPI,
			ArchonAID: claims.Subject,
			Payload:   carrier.Payload,
		}
		// Background ctx, not request-ctx: the HTTP server may cancel the request-ctx
		// right after the write response (the client dropped the connection) — audit must not
		// be lost for that reason (parity apimiddleware.Audit).
		if err := writer.Write(context.Background(), ev); err != nil && logger != nil {
			logger.Error("huma audit middleware: write failed",
				slog.String("event_type", string(evt)),
				slog.String("archon_aid", claims.Subject),
				slog.Any("error", err),
			)
		}
	}
}

// newHumaAuditAPI builds a huma.API over a chi group and attaches the variant-B audit
// middleware to ALL operations of that API (api.UseMiddleware). A parallel of
// [newHumaCadenceAPI], but with audit wiring: cadence writes self-audit INSIDE
// CreateTyped (emitWrite), role and the other middleware-audit domains — from the outside, via
// this middleware. One huma.API per chi group with one audit event type.
func newHumaAuditAPI(r chi.Router, writer audit.Writer, evt audit.EventType, logger *slog.Logger) huma.API {
	api := newHumaCadenceAPI(r)
	api.UseMiddleware(humaAuditMiddleware(writer, evt, logger))
	return api
}

// humaDumpSpec — a generic OpenAPI-fragment dump for guard/golden tests and the spec
// merge target of the rollout: builds a huma.API on a temporary chi router (WITHOUT mounting
// on the real router/audit wiring — [newHumaCadenceAPI] is enough to generate the schema),
// registers the domain operations via the passed register
// closure (a single register path per domain, no dump-vs-mount duplication) and emits
// 3.1.0 YAML (huma default). Reduces the per-domain HumaCadenceSpecYAML/HumaRoleSpecYAML
// and future domains to one place (a pilot-2 review nit BEFORE replicating the rollout).
func humaDumpSpec(register func(huma.API) error) (string, error) {
	installHumaErrorOverride()
	api := newHumaCadenceAPI(chi.NewRouter())
	if err := register(api); err != nil {
		return "", err
	}
	y, err := api.OpenAPI().YAML()
	if err != nil {
		return "", err
	}
	return string(y), nil
}
