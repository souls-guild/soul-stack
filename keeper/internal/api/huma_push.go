package api

// Registration and spec-dump of the PUSH domain on huma full-typed (ROLLOUT-BATCH-2e following
// operator issue-token + audit-endpoint, ADR-054 §Pattern). apply — WRITE+AUDIT (variant B,
// huma-audit-middleware; event push.applied; 202+body async); get — read (no audit);
// push-runs — read-with-typed-query (no audit). The domain *Typed functions (handlers/push.go)
// are extracted from (w,r); the old (w,r) — a thin strict wrapper (the MCP push-tool keeper.push.apply
// calls pushorch.PushRun directly, bypassing the handler — extraction does not affect it).

import (
	"context"
	"log/slog"

	"github.com/danielgtaylor/huma/v2"
	"github.com/go-chi/chi/v5"

	"github.com/souls-guild/soul-stack/keeper/internal/api/handlers"
	apimiddleware "github.com/souls-guild/soul-stack/keeper/internal/api/middleware"
	"github.com/souls-guild/soul-stack/keeper/internal/api/problem"
	"github.com/souls-guild/soul-stack/shared/audit"
)

// === projection of the push handler's domain views → native wire-DTO (handler-native:
// the api↔handlers boundary builds the wire body from flat domain fields; oapi-generated types
// are not involved). ===

// newPushApplyView projects the flat handlers.PushApplyResultView into the native PushApplyView
// (the 200 body of GET /v1/push/{apply_id}). status — native enum PushApplyViewStatus; pointers/
// timestamps are passed through as is (the handler already truncated date-time to the second).
func newPushApplyView(v handlers.PushApplyResultView) PushApplyView {
	return PushApplyView{
		ApplyID:       v.ApplyID,
		CleanupStale:  v.CleanupStale,
		DestinyRef:    v.DestinyRef,
		FinishedAt:    v.FinishedAt,
		Input:         v.Input,
		InventorySids: v.InventorySids,
		SSHProvider:   v.SSHProvider,
		StartedAt:     v.StartedAt,
		StartedByAID:  v.StartedByAID,
		Status:        PushApplyViewStatus(v.Status),
		Summary:       v.Summary,
	}
}

// newPushSummaryCounts projects the flat *handlers.PushSummaryCountsView into the native
// *PushSummaryCounts (nil → nil).
func newPushSummaryCounts(v *handlers.PushSummaryCountsView) *PushSummaryCounts {
	if v == nil {
		return nil
	}
	return &PushSummaryCounts{FailCount: v.FailCount, SuccessCount: v.SuccessCount, Total: v.Total}
}

// newPushRunListEntry projects the flat handlers.PushRunListEntryView into the native
// PushRunListEntry (a list-envelope element). status — native enum PushRunListEntryStatus.
func newPushRunListEntry(v handlers.PushRunListEntryView) PushRunListEntry {
	return PushRunListEntry{
		ApplyID:       v.ApplyID,
		CleanupStale:  v.CleanupStale,
		DestinyRef:    v.DestinyRef,
		FinishedAt:    v.FinishedAt,
		InventorySids: v.InventorySids,
		SSHProvider:   v.SSHProvider,
		StartedAt:     v.StartedAt,
		StartedByAID:  v.StartedByAID,
		Status:        PushRunListEntryStatus(v.Status),
		SummaryCounts: newPushSummaryCounts(v.SummaryCounts),
	}
}

// newPushRunListReply projects the domain handlers.PushRunListPage into the native envelope
// PushRunListReply. Items: nil → nil, otherwise a non-nil slice (the handler does make([]…, 0, n),
// so on success Items is always a non-nil [] — byte-exact with the former legacy generator).
func newPushRunListReply(p handlers.PushRunListPage) PushRunListReply {
	var items []PushRunListEntry
	if p.Items != nil {
		items = make([]PushRunListEntry, len(p.Items))
		for i := range p.Items {
			items[i] = newPushRunListEntry(p.Items[i])
		}
	}
	return PushRunListReply{Items: items, Limit: p.Limit, Offset: p.Offset, Total: p.Total}
}

// registerHumaPushApply mounts POST /v1/push/apply via huma (WRITE+AUDIT variant B —
// event push.applied). pushH nil → no-op. Handler: claims → convert typed-body → ApplyTyped →
// audit payload on the huma-ctx (SetHumaAuditPayload) → 202 WITH A BODY (apply_id, async).
func registerHumaPushApply(humaAPI huma.API, pushH *handlers.PushHandler) {
	if pushH == nil {
		return
	}
	huma.Register(humaAPI, pushApplyOperation(), func(ctx context.Context, in *pushApplyInput) (*pushApplyOutput, error) {
		claims, ok := apimiddleware.ClaimsFromContext(ctx)
		if !ok {
			return nil, pushMissingClaims()
		}
		reply, err := pushH.ApplyTyped(ctx, claims, toPushApplyInput(in.Body))
		if err != nil {
			return nil, pushProblem(err)
		}
		apimiddleware.SetHumaAuditPayload(ctx, apimiddleware.AuditPayload(reply.AuditPayload()))
		return &pushApplyOutput{Status: 202, Body: PushApplyReply{ApplyID: reply.ApplyID}}, nil
	})
}

// registerHumaPushGet mounts GET /v1/push/{apply_id} via huma (READ-with-path, no
// audit). pushH nil → no-op. Handler: GetTyped(apply_id) → typed output (404/422 via
// problem). RBAC push.read — on the group.
func registerHumaPushGet(humaAPI huma.API, pushH *handlers.PushHandler) {
	if pushH == nil {
		return
	}
	huma.Register(humaAPI, pushGetOperation(), func(ctx context.Context, in *pushGetInput) (*pushGetOutput, error) {
		reply, err := pushH.GetTyped(ctx, in.ApplyID)
		if err != nil {
			return nil, pushProblem(err)
		}
		return &pushGetOutput{Body: newPushApplyView(reply)}, nil
	})
}

// registerHumaPushRunsList mounts GET /v1/push-runs via huma (READ-with-typed-query,
// no audit). pushH nil → no-op. Handler: typed-query → ListRunsTyped → typed envelope output.
// RBAC incarnation.history — on the group. CheckPageBounds → 400 on out-of-range (the range
// is enforced by the DOMAIN, not huma min/max).
func registerHumaPushRunsList(humaAPI huma.API, pushH *handlers.PushHandler) {
	if pushH == nil {
		return
	}
	huma.Register(humaAPI, pushRunsListOperation(), func(ctx context.Context, in *pushRunsListInput) (*pushRunsListOutput, error) {
		reply, err := pushH.ListRunsTyped(ctx, in.Statuses, in.SSHProvider, int(in.Offset), int(in.Limit))
		if err != nil {
			return nil, pushProblem(err)
		}
		return &pushRunsListOutput{Body: newPushRunListReply(reply)}, nil
	})
}

// pushMissingClaims — a defensive response when claims are missing in ctx (unreachable: RequireJWT
// puts claims before huma). problem+json (parity roleMissingClaims).
func pushMissingClaims() huma.StatusError {
	return humaProblemError{Details: problem.New(problem.TypeInternalError, "", "missing claims")}
}

// pushProblem delivers a *Typed-function error through huma as problem+json. A domain
// *handlers.problemError → humaProblemError; non-problem → 500 (parity roleProblem).
func pushProblem(err error) huma.StatusError {
	if d, ok := handlers.AsProblemDetails(err); ok {
		return humaProblemError{Details: d}
	}
	return humaProblemError{Details: problem.New(problem.TypeInternalError, "", "internal error")}
}

// newHumaPushAPI builds a huma.API over a chi group with the huma-audit-middleware (variant B)
// for the given event type (parity newHumaRoleAPI). The single write route of push (apply)
// is mounted on ITS OWN chi group with the event type push.applied.
func newHumaPushAPI(r chi.Router, writer audit.Writer, evt audit.EventType, logger *slog.Logger) huma.API {
	return newHumaAuditAPI(r, writer, evt, logger)
}

// HumaPushSpecYAML builds the OpenAPI fragment of ALL huma-migrated push routes
// (apply/get/push-runs) as a YAML string, WITHOUT mounting on the real router. A hook for
// the rollout spec-merge target and a guard test. Delegates to the generic [humaDumpSpec] via the same
// register functions (a single register path). Returns a 3.1.0 spec (huma default).
func HumaPushSpecYAML() (string, error) {
	return humaDumpSpec(func(api huma.API) error {
		stub := handlers.PushSpecStub()
		registerHumaPushApply(api, stub)
		registerHumaPushGet(api, stub)
		registerHumaPushRunsList(api, stub)
		return nil
	})
}
