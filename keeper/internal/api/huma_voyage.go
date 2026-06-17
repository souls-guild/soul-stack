package api

// Регистрация и spec-dump VOYAGE-домена на huma full-typed (БАТЧ-2f WRITE-SELF-AUDIT
// по эталону cadence pilot + audit-endpoint read-with-query + soul dual-status, ADR-054
// §Pattern). create/cancel — WRITE-SELF-AUDIT: handler пишет scenario_run.started/
// command_run.invoked/.cancelled ВНУТРИ извлечённых CreateTyped/CancelTyped (emitCreated/
// emitCancelled), audit-middleware НЕ навешан (отличие от middleware-audit-доменов role/
// operator). preview — read-like dry-resolve БЕЗ audit. list/get/targets — read (БЕЗ audit).
// ВСЕ роуты монтируются через newHumaCadenceAPI (self-audit НЕ нужен audit-middleware).
// RBAC-by-kind (ADR-043 §6) — ВНУТРИ handler-а; router навешивает base auth + Tempo
// (create/preview). MCP voyage-tools зовут (w,r)-handler через httptest-recorder (keeper/
// internal/mcp/voyage.go) — извлечение НЕ затрагивает: (w,r) остался тонкой оболочкой.

import (
	"context"
	"net/http"

	"github.com/danielgtaylor/huma/v2"

	"github.com/souls-guild/soul-stack/keeper/internal/api/handlers"
	apimiddleware "github.com/souls-guild/soul-stack/keeper/internal/api/middleware"
	"github.com/souls-guild/soul-stack/keeper/internal/api/problem"
	sharedapi "github.com/souls-guild/soul-stack/shared/api"
)

// registerHumaVoyageCreate монтирует POST /v1/voyages через huma (WRITE-SELF-AUDIT:
// scenario_run.started/command_run.invoked пишет САМ handler внутри CreateTyped).
// voyageH nil → no-op. Handler: claims → конверт typed-body → CreateTyped (RBAC-by-kind
// + resolve + persist + self-audit) → 202 С ТЕЛОМ + Location.
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

// registerHumaVoyagePreview монтирует POST /v1/voyages/preview через huma (READ-like,
// БЕЗ audit). voyageH nil → no-op. Handler: claims → конверт → PreviewTyped → 200 С ТЕЛОМ.
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

// registerHumaVoyageList монтирует GET /v1/voyages через huma (READ-with-typed-query, БЕЗ
// audit). CheckPageBounds → 400 (диапазон enforce-ит ДОМЕН, parity (w,r) ParsePage).
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

// registerHumaVoyageGet монтирует GET /v1/voyages/{id} через huma (READ-with-path, БЕЗ audit).
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

// registerHumaVoyageTargets монтирует GET /v1/voyages/{id}/targets через huma (READ-with-
// path, БЕЗ audit).
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

// registerHumaVoyageCancel монтирует DELETE /v1/voyages/{id} через huma (WRITE-SELF-AUDIT:
// scenario_run.cancelled/command_run.cancelled пишет САМ handler внутри CancelTyped).
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

// toVoyageCreateRequest — конверт typed huma-body → доменная модель
// handlers.VoyageCreateRequest (FULL-TYPED §Pattern шаг 3). Поле-в-поле; вложенные
// target/notify — доменные VoyageTargetRequest/VoyageNotifyRequest (общие с Cadence;
// notify-форма несёт []byte annotations — marshal через marshalAnnotations, derefBool
// для *bool→bool, как cadence-конверт).
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

// toVoyageNotifyRequests конвертит huma notify[] в доменную форму (ephemeral). nil/пусто
// → nil (без уведомлений). annotations: map → []byte (marshalAnnotations, parity cadence);
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

// voyageMissingClaims — defensive-ответ при отсутствии claims (недостижим: RequireJWT
// кладёт claims до huma). problem+json (parity cadenceMissingClaims).
func voyageMissingClaims() huma.StatusError {
	return humaProblemError{Details: problem.New(problem.TypeInternalError, "", "missing claims")}
}

// voyageProblem доставляет ошибку *Typed-функции через huma как problem+json. Доменный
// *handlers.problemError → humaProblemError; не-problem → 500 (parity cadenceProblem).
func voyageProblem(err error) huma.StatusError {
	if d, ok := handlers.AsProblemDetails(err); ok {
		return humaProblemError{Details: d}
	}
	return humaProblemError{Details: problem.New(problem.TypeInternalError, "", "internal error")}
}

// HumaVoyageSpecYAML собирает OpenAPI-фрагмент ВСЕХ мигрированных-на-huma voyage-роутов
// (create/preview/list/get/targets/cancel) как YAML-строку, БЕЗ монтирования на реальный
// router. Хук для спека-мерж-таргета тиража и guard-теста. Делегирует generic
// [humaDumpSpec]. Возвращает 3.1.0-спеку (huma-дефолт).
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
