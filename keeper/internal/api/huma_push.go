package api

// Регистрация и spec-dump PUSH-домена на huma full-typed (ТИРАЖ-БАТЧ-2e по эталонам
// operator issue-token + audit-endpoint, ADR-054 §Pattern). apply — WRITE+AUDIT (вариант B,
// huma-audit-middleware; событие push.applied; 202+body async); get — read (БЕЗ audit);
// push-runs — read-with-typed-query (БЕЗ audit). Доменные *Typed-функции (handlers/push.go)
// извлечены из (w,r); старый (w,r) — тонкая strict-оболочка (MCP push-tool keeper.push.apply
// зовёт pushorch.PushRun напрямую, мимо handler — извлечение не затрагивает).

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

// === проекция доменных view-ов handler-а push → native wire-DTO (handler-native:
// граница api↔handlers строит wire-тело из плоских доменных полей; oapi-генерёные типы
// не участвуют). ===

// newPushApplyView проецирует плоский handlers.PushApplyResultView в native PushApplyView
// (200-тело GET /v1/push/{apply_id}). status — native enum PushApplyViewStatus; указатели/
// таймстампы пробрасываются как есть (handler уже усёк date-time до секунды).
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

// newPushSummaryCounts проецирует плоский *handlers.PushSummaryCountsView в native
// *PushSummaryCounts (nil → nil).
func newPushSummaryCounts(v *handlers.PushSummaryCountsView) *PushSummaryCounts {
	if v == nil {
		return nil
	}
	return &PushSummaryCounts{FailCount: v.FailCount, SuccessCount: v.SuccessCount, Total: v.Total}
}

// newPushRunListEntry проецирует плоский handlers.PushRunListEntryView в native
// PushRunListEntry (element list-envelope). status — native enum PushRunListEntryStatus.
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

// newPushRunListReply проецирует доменный handlers.PushRunListPage в native envelope
// PushRunListReply. Items: nil → nil, иначе non-nil срез (handler делает make([]…, 0, n),
// поэтому на success Items всегда non-nil [] — byte-exact с прежним legacy-генерата).
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

// registerHumaPushApply монтирует POST /v1/push/apply через huma (WRITE+AUDIT вариант B —
// event push.applied). pushH nil → no-op. Handler: claims → конверт typed-body → ApplyTyped →
// audit-payload на huma-ctx (SetHumaAuditPayload) → 202 С ТЕЛОМ (apply_id, async).
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

// registerHumaPushGet монтирует GET /v1/push/{apply_id} через huma (READ-with-path, БЕЗ
// audit). pushH nil → no-op. Handler: GetTyped(apply_id) → typed output (404/422 через
// problem). RBAC push.read — на группе.
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

// registerHumaPushRunsList монтирует GET /v1/push-runs через huma (READ-with-typed-query,
// БЕЗ audit). pushH nil → no-op. Handler: typed-query → ListRunsTyped → typed envelope-output.
// RBAC incarnation.history — на группе. CheckPageBounds → 400 на out-of-range (диапазон
// enforce-ит ДОМЕН, не huma-min/max).
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

// pushMissingClaims — defensive-ответ при отсутствии claims в ctx (недостижим: RequireJWT
// кладёт claims до huma). problem+json (parity roleMissingClaims).
func pushMissingClaims() huma.StatusError {
	return humaProblemError{Details: problem.New(problem.TypeInternalError, "", "missing claims")}
}

// pushProblem доставляет ошибку *Typed-функции через huma как problem+json. Доменный
// *handlers.problemError → humaProblemError; не-problem → 500 (parity roleProblem).
func pushProblem(err error) huma.StatusError {
	if d, ok := handlers.AsProblemDetails(err); ok {
		return humaProblemError{Details: d}
	}
	return humaProblemError{Details: problem.New(problem.TypeInternalError, "", "internal error")}
}

// newHumaPushAPI собирает huma.API поверх chi-группы с huma-audit-middleware (вариант B)
// под переданный event-тип (parity newHumaRoleAPI). Единственный write-роут push (apply)
// монтируется на СВОЕЙ chi-группе с event-типом push.applied.
func newHumaPushAPI(r chi.Router, writer audit.Writer, evt audit.EventType, logger *slog.Logger) huma.API {
	return newHumaAuditAPI(r, writer, evt, logger)
}

// HumaPushSpecYAML собирает OpenAPI-фрагмент ВСЕХ мигрированных-на-huma push-роутов
// (apply/get/push-runs) как YAML-строку, БЕЗ монтирования на реальный router. Хук для
// спека-мерж-таргета тиража и guard-теста. Делегирует generic [humaDumpSpec] через те же
// register-функции (единый register-путь). Возвращает 3.1.0-спеку (huma-дефолт).
func HumaPushSpecYAML() (string, error) {
	return humaDumpSpec(func(api huma.API) error {
		stub := handlers.PushSpecStub()
		registerHumaPushApply(api, stub)
		registerHumaPushGet(api, stub)
		registerHumaPushRunsList(api, stub)
		return nil
	})
}
