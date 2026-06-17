package api

// huma-audit-middleware (ВАРИАНТ B, ADR-054 §Audit) + generic spec-dump хелпер.
// КЛЮЧЕВОЙ pattern тиража middleware-audit-доменов (role + operator + souls + synod
// + service + sigil + augur + oracle …): домены, чей audit писался
// [apimiddleware.Audit] (StatusRecorder в bridge) + handler-овский
// [apimiddleware.SetAuditPayload], НЕ могут так писать audit на full-typed huma —
// huma САМ пишет ответ через свой Context (chiContext.SetStatus → w.WriteHeader),
// минуя StatusRecorder-обёртку [apimiddleware.Audit] → rec.status==0 → audit молча
// не пишется (рецидив S6-регрессии). Решение — huma-native middleware, читающий
// статус из huma-Context.Status() (поле *chiContext, НЕ http.ResponseWriter):
// hctx.Status() доступен нативно ПОСЛЕ next, early-flush его не ломает, fallback-
// обёртка не нужна. Payload пробрасывается handler → middleware через МУТИРУЕМЫЙ
// carrier в request-context (seed ДО next, чтение ТОГО ЖЕ указателя после next).
// Полный разбор спайка Status()/carrier — ADR-054 §Audit.

import (
	"context"
	"log/slog"

	"github.com/danielgtaylor/huma/v2"
	"github.com/go-chi/chi/v5"

	apimiddleware "github.com/souls-guild/soul-stack/keeper/internal/api/middleware"
	"github.com/souls-guild/soul-stack/shared/audit"
)

// humaAuditMiddleware — huma-native audit-middleware (вариант B). Навешивается на
// huma.API через api.UseMiddleware ПОД chi-группой, уже несущей RequireJWT/
// RequirePermission (claims кладёт RequireJWT в request-context ДО humachi).
//
// Контракт (parity apimiddleware.Audit):
//   - вызывает next(hctx) с seed-нутым payload-carrier-ом;
//   - ПОСЛЕ next читает hctx.Status(): status>=300 || status==0 → skip (4xx/5xx —
//     операция не выполнена; 0 — panic/ранний отказ до SetStatus). 2xx → пишем;
//   - claims из hctx.Context() (apimiddleware.ClaimsFromContext); нет claims →
//     warn + skip (parity Audit);
//   - payload из carrier (handler положил через SetHumaAuditPayload); пуст → пишем
//     event с nil-payload (как Audit без builder/override);
//   - writer.Write(ctx, &audit.Event{EventType:evt, Source:SourceAPI,
//     ArchonAID:claims.Subject, Payload}). ctx — Background (parity Audit: request-
//     ctx может быть отменён сразу после ответа — audit не должен теряться).
//
// writer nil (dev без audit) → middleware прозрачен (только next). Ошибка
// writer.Write логируется, на ответ не влияет (он уже улетел) — best-effort, как
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
			// 4xx/5xx — операция не выполнена; 0 — handler не дошёл до SetStatus
			// (panic / ранний huma-отказ). В audit_log не пишем (parity Audit).
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
		// Background-ctx, не request-ctx: HTTP-сервер может отменить request-ctx
		// сразу после write-ответа (клиент разорвал соединение) — audit не должен
		// теряться по этой причине (parity apimiddleware.Audit).
		if err := writer.Write(context.Background(), ev); err != nil && logger != nil {
			logger.Error("huma audit middleware: write failed",
				slog.String("event_type", string(evt)),
				slog.String("archon_aid", claims.Subject),
				slog.Any("error", err),
			)
		}
	}
}

// newHumaAuditAPI собирает huma.API поверх chi-группы и навешивает audit-
// middleware варианта B на ВСЕ операции этой API (api.UseMiddleware). Параллель
// [newHumaCadenceAPI], но с audit-навеской: cadence пишет self-audit ВНУТРИ
// CreateTyped (emitWrite), role и прочие middleware-audit-домены — снаружи, через
// этот middleware. Одна huma.API на chi-группу с одним audit-event-типом.
func newHumaAuditAPI(r chi.Router, writer audit.Writer, evt audit.EventType, logger *slog.Logger) huma.API {
	api := newHumaCadenceAPI(r)
	api.UseMiddleware(humaAuditMiddleware(writer, evt, logger))
	return api
}

// humaDumpSpec — generic OpenAPI-фрагмент-дамп для guard/golden-тестов и спека-
// мерж-таргета тиража: собирает huma.API на временном chi-роутере (БЕЗ монтирования
// на реальный router/audit-навески — для генерации схемы достаточно
// [newHumaCadenceAPI]), регистрирует операции домена через переданный register-
// замыкатель (единый register-путь домена, нет дубля dump-vs-mount) и эмитит
// 3.1.0-YAML (huma-дефолт). Сводит per-domain HumaCadenceSpecYAML/HumaRoleSpecYAML
// и будущие домены к одной точке (ревью-nit pilot-2 ДО размножения тиража).
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
