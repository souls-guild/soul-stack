package api

// Регистрация и spec-dump ERRAND-домена (list + get + cancel) на huma full-typed,
// handler-native (T5d-2c-full: 0 legacy-генерата в errand-файлах). list — read-with-typed-query
// (started_after date-time→400, offset/limit→400, status enum, sid/module string/array),
// get — read-with-path (200 ErrandResult / 202 running), cancel — WRITE+AUDIT (вариант B,
// huma-audit-middleware; event errand.cancelled). list/get строят native wire-DTO
// (ErrandResult/ErrandListReply, huma_errand_reply.go) НАПРЯМУЮ из плоских доменных
// view-ов handler-а (ErrandResultView/ErrandListPage), конвертеров legacy-генерата→native нет.
// Mutating POST /v1/souls/{sid}/exec обслуживает SOUL-домен (huma_soul.go, ExecTyped) —
// вне этого батча. MCP errand-tools зовут errand.Dispatcher/Store напрямую, мимо handler
// — (w,r)-оболочки list/get/cancel сняты.

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

// registerHumaErrandList монтирует GET /v1/errands через huma (READ-with-typed-query,
// БЕЗ audit). errandH nil → no-op. Handler: typed-query → ListTyped → typed envelope-
// output. RBAC errand.list — на группе. CheckPageBounds → 400 на out-of-range (на этом
// блокере ловили — диапазон enforce-ит ДОМЕН, не huma-min/max).
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

// registerHumaErrandGet монтирует GET /v1/errands/{errand_id} через huma (READ-with-
// path, БЕЗ audit). errandH nil → no-op. Handler: GetTyped → 200 терминал ErrandResult
// либо 202 running ErrandAccepted (двойной success-код, Body пред-маршалится в
// json.RawMessage, Status override). RBAC errand.list — на группе.
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

// registerHumaErrandCancel монтирует DELETE /v1/errands/{errand_id} через huma
// (WRITE+AUDIT вариант B — event errand.cancelled). errandH nil → no-op. Handler:
// claims → CancelTyped → audit-payload на huma-ctx (SetHumaAuditPayload) → пустой
// 204-output. dispatcher также пишет свой audit-event внутри Cancel (single source of
// truth для archon_aid + payload); middleware-event здесь — security navigation-trail.
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

// === проекция доменных view-ов handler-а → native wire-DTO (handler-native: граница
// api↔handlers строит wire-тело из плоских доменных полей; oapi-генерёные типы не
// участвуют). ===

// newErrandResult проецирует плоский handlers.ErrandResultView в native ErrandResult
// (200-тело errand-get-терминал / element list-а). Указатели/таймстампы пробрасываются
// как есть — handler уже усёк date-time до секунды в проекции из errand.Row (byte-exact
// с прежним legacy-генерата).
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

// newErrandListReply проецирует доменный handlers.ErrandListPage в native envelope
// ErrandListReply. Items: nil → nil, иначе non-nil срез (handler делает make([]…, 0, n),
// поэтому на success Items всегда non-nil [] — byte-exact с прежним legacy-генерата).
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

// ErrandAccepted — native 202-тело errand-get-running (errand_id + status). Форма 1:1 с
// прежним ErrandAccepted; на wire сериализуется register-func-ом get-роута через
// json.RawMessage (errandGetOutput.Body). Схема в components/schemas эмитится отдельным
// schema-builder pre-seed (errandAccepted, huma_errand_accepted.go) — этот тип в
// spec-emission НЕ участвует, только в wire-сериализации 202-тела.
type ErrandAccepted struct {
	ErrandID string `json:"errand_id"`
	Status   string `json:"status"`
}

// newErrandAccepted проецирует плоский handlers.ErrandAcceptedView в native 202-тело.
func newErrandAccepted(v handlers.ErrandAcceptedView) ErrandAccepted {
	return ErrandAccepted{ErrandID: v.ErrandID, Status: v.Status}
}

// errandMissingClaims — defensive-ответ при отсутствии claims в ctx (недостижим:
// RequireJWT кладёт claims до huma). problem+json (parity roleMissingClaims).
func errandMissingClaims() huma.StatusError {
	return humaProblemError{Details: problem.New(problem.TypeInternalError, "", "missing claims")}
}

// errandProblem доставляет ошибку *Typed-функции через huma как problem+json.
// Доменный *handlers.problemError → humaProblemError; не-problem → 500 (parity
// roleProblem).
func errandProblem(err error) huma.StatusError {
	if d, ok := handlers.AsProblemDetails(err); ok {
		return humaProblemError{Details: d}
	}
	return humaProblemError{Details: problem.New(problem.TypeInternalError, "", "internal error")}
}

// newHumaErrandAPI собирает huma.API поверх chi-группы с huma-audit-middleware
// (вариант B) под переданный event-тип (parity newHumaRoleAPI). Единственный write-
// роут errand (cancel) монтируется на СВОЕЙ chi-группе с event-типом errand.cancelled.
func newHumaErrandAPI(r chi.Router, writer audit.Writer, evt audit.EventType, logger *slog.Logger) huma.API {
	return newHumaAuditAPI(r, writer, evt, logger)
}

// HumaErrandSpecYAML собирает OpenAPI-фрагмент ВСЕХ мигрированных-на-huma errand-роутов
// (list/get/cancel) как YAML-строку, БЕЗ монтирования на реальный router. Хук для
// спека-мерж-таргета тиража и guard-теста. Делегирует generic [humaDumpSpec] через те
// же register-функции (единый register-путь). Возвращает 3.1.0-спеку (huma-дефолт).
func HumaErrandSpecYAML() (string, error) {
	return humaDumpSpec(func(api huma.API) error {
		stub := handlers.ErrandSpecStub()
		registerHumaErrandList(api, stub)
		registerHumaErrandGet(api, stub)
		registerHumaErrandCancel(api, stub)
		return nil
	})
}
