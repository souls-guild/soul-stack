package api

// Registration and spec-dump of the VOYAGE domain on huma full-typed (BATCH-2f WRITE-SELF-AUDIT
// following the cadence pilot reference + audit-endpoint read-with-query + soul dual-status, ADR-054
// §Pattern). create/cancel — WRITE-SELF-AUDIT: the handler writes scenario_run.started/
// command_run.invoked/.cancelled INSIDE the extracted CreateTyped/CancelTyped (emitCreated/
// emitCancelled), the audit middleware is NOT wired (unlike the middleware-audit domains role/
// operator). preview — read-like dry-resolve, no audit. list/get/targets — read (no audit).
// ALL routes are mounted via newHumaCadenceAPI (self-audit does not need the audit middleware).
// RBAC-by-kind (ADR-043 §6) — INSIDE the handler; the router wires base auth + Tempo
// (create/preview). MCP voyage tools call the (w,r) handler via an httptest recorder (keeper/
// internal/mcp/voyage.go) — the extraction does not affect them: (w,r) stayed a thin shell.

import (
	"context"
	"net/http"

	"github.com/danielgtaylor/huma/v2"

	"github.com/souls-guild/soul-stack/keeper/internal/api/handlers"
	apimiddleware "github.com/souls-guild/soul-stack/keeper/internal/api/middleware"
	"github.com/souls-guild/soul-stack/keeper/internal/api/problem"
	sharedapi "github.com/souls-guild/soul-stack/shared/api"
)

// registerHumaVoyageCreate mounts POST /v1/voyages via huma (WRITE-SELF-AUDIT:
// scenario_run.started/command_run.invoked is written by the handler itself inside CreateTyped).
// voyageH nil → no-op. Handler: claims → convert typed body → CreateTyped (RBAC-by-kind
// + resolve + persist + self-audit) → 202 WITH BODY + Location.
func registerHumaVoyageCreate(humaAPI huma.API, voyageH *handlers.VoyageHandler) {
	if voyageH == nil {
		return
	}
	huma.Register(humaAPI, voyageCreateOperation(), func(ctx context.Context, in *voyageCreateInput) (*voyageCreateOutput, error) {
		claims, ok := apimiddleware.ClaimsFromContext(ctx)
		if !ok {
			return nil, voyageMissingClaims()
		}
		req := toVoyageCreateRequest(in.Body)
		reply, err := voyageH.CreateTyped(ctx, claims, &req)
		if err != nil {
			return nil, voyageProblem(err)
		}
		return &voyageCreateOutput{Status: http.StatusAccepted, Location: reply.Location, Body: toVoyageCreateReply(reply)}, nil
	})
}

// registerHumaVoyagePreview mounts POST /v1/voyages/preview via huma (READ-like,
// no audit). voyageH nil → no-op. Handler: claims → convert → PreviewTyped → 200 WITH BODY.
func registerHumaVoyagePreview(humaAPI huma.API, voyageH *handlers.VoyageHandler) {
	if voyageH == nil {
		return
	}
	huma.Register(humaAPI, voyagePreviewOperation(), func(ctx context.Context, in *voyagePreviewInput) (*voyagePreviewOutput, error) {
		claims, ok := apimiddleware.ClaimsFromContext(ctx)
		if !ok {
			return nil, voyageMissingClaims()
		}
		req := toVoyageCreateRequest(in.Body)
		reply, err := voyageH.PreviewTyped(ctx, claims, &req)
		if err != nil {
			return nil, voyageProblem(err)
		}
		return &voyagePreviewOutput{Body: toVoyagePreviewReply(reply)}, nil
	})
}

// registerHumaVoyageList mounts GET /v1/voyages via huma (READ with typed query, no
// audit). CheckPageBounds → 400 (the range is enforced by the DOMAIN, parity with (w,r) ParsePage).
func registerHumaVoyageList(humaAPI huma.API, voyageH *handlers.VoyageHandler) {
	if voyageH == nil {
		return
	}
	huma.Register(humaAPI, voyageListOperation(), func(ctx context.Context, in *voyageListInput) (*voyageListOutput, error) {
		if err := sharedapi.CheckPageBounds(int(in.Offset), int(in.Limit)); err != nil {
			return nil, humaProblemError{Details: problem.New(problem.TypeMalformedRequest, "", err.Error())}
		}
		reply, err := voyageH.ListTyped(ctx, handlers.VoyageListInput{
			Kind:     in.Kind,
			Statuses: in.Statuses,
			Page:     sharedapi.Page{Offset: int(in.Offset), Limit: int(in.Limit)},
		})
		if err != nil {
			return nil, voyageProblem(err)
		}
		return &voyageListOutput{Body: toVoyageListReply(reply)}, nil
	})
}

// registerHumaVoyageGet mounts GET /v1/voyages/{id} via huma (READ with path, no audit).
func registerHumaVoyageGet(humaAPI huma.API, voyageH *handlers.VoyageHandler) {
	if voyageH == nil {
		return
	}
	huma.Register(humaAPI, voyageGetOperation(), func(ctx context.Context, in *voyageGetInput) (*voyageGetOutput, error) {
		dto, err := voyageH.GetTyped(ctx, in.ID)
		if err != nil {
			return nil, voyageProblem(err)
		}
		return &voyageGetOutput{Body: toVoyage(dto)}, nil
	})
}

// registerHumaVoyageTargets mounts GET /v1/voyages/{id}/targets via huma (READ with
// path, no audit).
func registerHumaVoyageTargets(humaAPI huma.API, voyageH *handlers.VoyageHandler) {
	if voyageH == nil {
		return
	}
	huma.Register(humaAPI, voyageTargetsOperation(), func(ctx context.Context, in *voyageTargetsInput) (*voyageTargetsOutput, error) {
		reply, err := voyageH.TargetsTyped(ctx, in.ID)
		if err != nil {
			return nil, voyageProblem(err)
		}
		return &voyageTargetsOutput{Body: toVoyageTargetsReply(reply)}, nil
	})
}

// registerHumaVoyageCancel mounts DELETE /v1/voyages/{id} via huma (WRITE-SELF-AUDIT:
// scenario_run.cancelled/command_run.cancelled is written by the handler itself inside CancelTyped).
func registerHumaVoyageCancel(humaAPI huma.API, voyageH *handlers.VoyageHandler) {
	if voyageH == nil {
		return
	}
	huma.Register(humaAPI, voyageCancelOperation(), func(ctx context.Context, in *voyageCancelInput) (*voyageCancelOutput, error) {
		claims, ok := apimiddleware.ClaimsFromContext(ctx)
		if !ok {
			return nil, voyageMissingClaims()
		}
		reply, err := voyageH.CancelTyped(ctx, claims, in.ID)
		if err != nil {
			return nil, voyageProblem(err)
		}
		return &voyageCancelOutput{Status: http.StatusAccepted, Body: toVoyageCancelReply(reply)}, nil
	})
}

// toVoyageCreateRequest — converts the typed huma body → domain model
// handlers.VoyageCreateRequest (FULL-TYPED §Pattern step 3). Field-by-field; the nested
// target/notify — domain VoyageTargetRequest/VoyageNotifyRequest (shared with Cadence;
// the notify shape carries []byte annotations — marshalled via marshalAnnotations, derefBool
// for *bool→bool, like the cadence converter).
func toVoyageCreateRequest(b VoyageCreateRequest) handlers.VoyageCreateRequest {
	return handlers.VoyageCreateRequest{
		Kind:         b.Kind,
		ScenarioName: b.ScenarioName,
		Module:       b.Module,
		Input:        b.Input,
		Target: &handlers.VoyageTargetRequest{
			Incarnations: b.Target.Incarnations,
			Service:      b.Target.Service,
			SIDs:         b.Target.SIDs,
			Where:        b.Target.Where,
			Coven:        b.Target.Coven,
		},
		Batch:                b.Batch,
		BatchSize:            b.BatchSize,
		BatchPercent:         b.BatchPercent,
		Concurrency:          b.Concurrency,
		BatchMode:            b.BatchMode,
		DryRun:               b.DryRun,
		ScheduleAt:           b.ScheduleAt,
		InterBatchIntervalMS: b.InterBatchIntervalMS,
		InterUnitIntervalMS:  b.InterUnitIntervalMS,
		MaxFailures:          b.MaxFailures,
		FailThreshold:        b.FailThreshold,
		RequireAlive:         b.RequireAlive,
		OnFailure:            b.OnFailure,
		Notify:               toVoyageNotifyRequests(b.Notify),
	}
}

// toVoyageNotifyRequests converts huma notify[] into the domain shape (ephemeral). nil/empty
// → nil (no notifications). annotations: map → []byte (marshalAnnotations, parity cadence);
// *bool → bool (derefBool).
func toVoyageNotifyRequests(in []VoyageNotify) []handlers.VoyageNotifyRequest {
	if len(in) == 0 {
		return nil
	}
	out := make([]handlers.VoyageNotifyRequest, len(in))
	for i, n := range in {
		out[i] = handlers.VoyageNotifyRequest{
			Herald:       n.Herald,
			On:           n.On,
			OnlyFailures: derefBool(n.OnlyFailures),
			OnlyChanges:  derefBool(n.OnlyChanges),
			Annotations:  marshalAnnotations(n.Annotations),
			Projection:   n.Projection,
		}
	}
	return out
}

// voyageMissingClaims — defensive response when claims are absent (unreachable: RequireJWT
// puts claims in before huma). problem+json (parity cadenceMissingClaims).
func voyageMissingClaims() huma.StatusError {
	return humaProblemError{Details: problem.New(problem.TypeInternalError, "", "missing claims")}
}

// voyageProblem delivers a *Typed function error through huma as problem+json. A domain
// *handlers.problemError → humaProblemError; non-problem → 500 (parity cadenceProblem).
func voyageProblem(err error) huma.StatusError {
	if d, ok := handlers.AsProblemDetails(err); ok {
		return humaProblemError{Details: d}
	}
	return humaProblemError{Details: problem.New(problem.TypeInternalError, "", "internal error")}
}

// HumaVoyageSpecYAML assembles the OpenAPI fragment of ALL voyage routes migrated to huma
// (create/preview/list/get/targets/cancel) as a YAML string, without mounting on a real
// router. Hook for the rollout spec merge target and the guard test. Delegates to the generic
// [humaDumpSpec]. Returns a 3.1.0 spec (huma default).
func HumaVoyageSpecYAML() (string, error) {
	return humaDumpSpec(func(api huma.API) error {
		stub := handlers.VoyageSpecStub()
		registerHumaVoyageCreate(api, stub)
		registerHumaVoyagePreview(api, stub)
		registerHumaVoyageList(api, stub)
		registerHumaVoyageGet(api, stub)
		registerHumaVoyageTargets(api, stub)
		registerHumaVoyageCancel(api, stub)
		return nil
	})
}
