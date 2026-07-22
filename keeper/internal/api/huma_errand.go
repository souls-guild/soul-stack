package api

// Registration and spec-dump of the ERRAND domain (list + get + cancel) on huma full-typed,
// handler-native (T5d-2c-full: 0 legacy generator in the errand files). list — read with typed
// query (started_after date-time→400, offset/limit→400, status enum, sid/module string/array),
// get — read with path (200 ErrandResult / 202 running), cancel — WRITE+AUDIT (variant B,
// huma-audit-middleware; event errand.cancelled). list/get build the native wire-DTO
// (ErrandResult/ErrandListReply, huma_errand_reply.go) DIRECTLY from the handler's flat domain
// views (ErrandResultView/ErrandListPage); there are no legacy-generator→native converters.
// The mutating POST /v1/souls/{sid}/exec is served by the SOUL domain (huma_soul.go, ExecTyped) —
// outside this batch. MCP errand-tools call errand.Dispatcher/Store directly, bypassing the
// handler — the (w,r) wrappers for list/get/cancel are removed.

import (
	"context"
	"encoding/json"
	"log/slog"

	"github.com/danielgtaylor/huma/v2"
	"github.com/go-chi/chi/v5"

	"github.com/souls-guild/soul-stack/keeper/internal/api/handlers"
	apimiddleware "github.com/souls-guild/soul-stack/keeper/internal/api/middleware"
	"github.com/souls-guild/soul-stack/keeper/internal/api/problem"
	"github.com/souls-guild/soul-stack/shared/audit"
)

// registerHumaErrandList mounts GET /v1/errands via huma (READ with typed query, no
// audit). errandH nil → no-op. Handler: typed-query → ListTyped → typed envelope output.
// RBAC errand.list — on the group. CheckPageBounds → 400 on out-of-range (a known
// blocker — the range is enforced by the DOMAIN, not huma min/max).
func registerHumaErrandList(humaAPI huma.API, errandH *handlers.ErrandHandler) {
	if errandH == nil {
		return
	}
	huma.Register(humaAPI, errandListOperation(), func(ctx context.Context, in *errandListInput) (*errandListOutput, error) {
		page, err := errandH.ListTyped(ctx, handlers.ErrandListInput{
			SID:          in.SID,
			Status:       in.Status,
			StartedAfter: in.StartedAfter,
			Modules:      in.Modules,
			Offset:       int(in.Offset),
			Limit:        int(in.Limit),
		})
		if err != nil {
			return nil, errandProblem(err)
		}
		return &errandListOutput{Body: newErrandListReply(page)}, nil
	})
}

// registerHumaErrandGet mounts GET /v1/errands/{errand_id} via huma (READ with path, no
// audit). errandH nil → no-op. Handler: GetTyped → 200 terminal ErrandResult or 202
// running ErrandAccepted (dual success code, Body pre-marshaled into json.RawMessage,
// Status override). RBAC errand.list — on the group.
func registerHumaErrandGet(humaAPI huma.API, errandH *handlers.ErrandHandler) {
	if errandH == nil {
		return
	}
	huma.Register(humaAPI, errandGetOperation(), func(ctx context.Context, in *errandGetInput) (*errandGetOutput, error) {
		reply, err := errandH.GetTyped(ctx, in.ErrandID)
		if err != nil {
			return nil, errandProblem(err)
		}
		if reply.Running {
			body, merr := json.Marshal(newErrandAccepted(reply.Accepted))
			if merr != nil {
				return nil, errandProblem(merr)
			}
			return &errandGetOutput{Status: 202, Body: body}, nil
		}
		body, merr := json.Marshal(newErrandResult(reply.Result))
		if merr != nil {
			return nil, errandProblem(merr)
		}
		return &errandGetOutput{Status: 200, Body: body}, nil
	})
}

// registerHumaErrandCancel mounts DELETE /v1/errands/{errand_id} via huma (WRITE+AUDIT
// variant B — event errand.cancelled). errandH nil → no-op. Handler: claims →
// CancelTyped → audit payload on the huma ctx (SetHumaAuditPayload) → empty 204 output.
// The dispatcher also writes its own audit event inside Cancel (single source of truth
// for archon_aid + payload); the middleware event here is a security navigation-trail.
func registerHumaErrandCancel(humaAPI huma.API, errandH *handlers.ErrandHandler) {
	if errandH == nil {
		return
	}
	huma.Register(humaAPI, errandCancelOperation(), func(ctx context.Context, in *errandCancelInput) (*errandNoContentOutput, error) {
		claims, ok := apimiddleware.ClaimsFromContext(ctx)
		if !ok {
			return nil, errandMissingClaims()
		}
		reply, err := errandH.CancelTyped(ctx, claims, in.ErrandID)
		if err != nil {
			return nil, errandProblem(err)
		}
		apimiddleware.SetHumaAuditPayload(ctx, apimiddleware.AuditPayload(reply.AuditPayload()))
		return &errandNoContentOutput{Status: 204}, nil
	})
}

// === projection of the handler's domain views → native wire-DTO (handler-native: the
// api↔handlers boundary builds the wire body from flat domain fields; oapi-generated types
// are not involved). ===

// newErrandResult projects the flat handlers.ErrandResultView into a native ErrandResult
// (200 body of errand-get-terminal / a list element). Pointers/timestamps pass through
// as-is — the handler already truncated date-time to the second in the projection from
// errand.Row (byte-exact with the former legacy generator).
func newErrandResult(v handlers.ErrandResultView) ErrandResult {
	return ErrandResult{
		DurationMs:      v.DurationMs,
		ErrandID:        v.ErrandID,
		ErrorMessage:    v.ErrorMessage,
		ExitCode:        v.ExitCode,
		FinishedAt:      v.FinishedAt,
		Module:          v.Module,
		Output:          v.Output,
		SID:             v.SID,
		StartedAt:       v.StartedAt,
		StartedByAID:    v.StartedByAID,
		Status:          ErrandResultStatus(v.Status),
		Stderr:          v.Stderr,
		StderrTruncated: v.StderrTruncated,
		Stdout:          v.Stdout,
		StdoutTruncated: v.StdoutTruncated,
	}
}

// newErrandListReply projects the domain handlers.ErrandListPage into the native envelope
// ErrandListReply. Items: nil → nil, otherwise a non-nil slice (the handler does
// make([]…, 0, n), so on success Items is always a non-nil [] — byte-exact with the former
// legacy generator).
func newErrandListReply(p handlers.ErrandListPage) ErrandListReply {
	var items []ErrandResult
	if p.Items != nil {
		items = make([]ErrandResult, len(p.Items))
		for i := range p.Items {
			items[i] = newErrandResult(p.Items[i])
		}
	}
	return ErrandListReply{Items: items, Limit: p.Limit, Offset: p.Offset, Total: p.Total}
}

// ErrandAccepted — native 202 body of errand-get-running (errand_id + status). Shape 1:1
// with the former ErrandAccepted; on the wire it is serialized by the get route's register
// function via json.RawMessage (errandGetOutput.Body). The schema in components/schemas is
// emitted by a separate schema-builder pre-seed (errandAccepted, huma_errand_accepted.go) —
// this type does NOT take part in spec emission, only in the wire serialization of the 202 body.
type ErrandAccepted struct {
	ErrandID string `json:"errand_id"`
	Status   string `json:"status"`
}

// newErrandAccepted projects the flat handlers.ErrandAcceptedView into the native 202 body.
func newErrandAccepted(v handlers.ErrandAcceptedView) ErrandAccepted {
	return ErrandAccepted{ErrandID: v.ErrandID, Status: v.Status}
}

// errandMissingClaims — a defensive response when claims are missing from ctx (unreachable:
// RequireJWT puts claims in before huma). problem+json (parity roleMissingClaims).
func errandMissingClaims() huma.StatusError {
	return humaProblemError{Details: problem.New(problem.TypeInternalError, "", "missing claims")}
}

// errandProblem delivers a *Typed-function error through huma as problem+json. A domain
// *handlers.problemError → humaProblemError; non-problem → 500 (parity roleProblem).
func errandProblem(err error) huma.StatusError {
	if d, ok := handlers.AsProblemDetails(err); ok {
		return humaProblemError{Details: d}
	}
	return humaProblemError{Details: problem.New(problem.TypeInternalError, "", "internal error")}
}

// newHumaErrandAPI assembles a huma.API over a chi group with the huma-audit-middleware
// (variant B) under the given event type (parity newHumaRoleAPI). The single write route
// of errand (cancel) is mounted on its OWN chi group with the event type errand.cancelled.
func newHumaErrandAPI(r chi.Router, writer audit.Writer, evt audit.EventType, logger *slog.Logger) huma.API {
	return newHumaAuditAPI(r, writer, evt, logger)
}

// HumaErrandSpecYAML assembles the OpenAPI fragment of ALL errand routes migrated to huma
// (list/get/cancel) as a YAML string, WITHOUT mounting on a real router. A hook for the
// rollout's spec merge target and a guard test. Delegates to the generic [humaDumpSpec]
// through the same register functions (a single register path). Returns a 3.1.0 spec (huma default).
func HumaErrandSpecYAML() (string, error) {
	return humaDumpSpec(func(api huma.API) error {
		stub := handlers.ErrandSpecStub()
		registerHumaErrandList(api, stub)
		registerHumaErrandGet(api, stub)
		registerHumaErrandCancel(api, stub)
		return nil
	})
}
