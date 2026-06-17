package api

// Регистрация и spec-dump GET /v1/audit на huma full-typed (ADR-054 §Pattern
// ЧЕТВЁРТЫЙ tier — read-with-typed-query). READ-вариант (БЕЗ audit-middleware:
// чтение audit_log само audit-event не порождает). huma валидирует typed-query →
// конверт в доменный handlers.AuditListFilter → ListTyped → typed envelope-output.
// Доменные problem-ошибки (невалидный source → 422) доставляются через
// humaProblemError тем же error-контрактом, что huma-bind (bad date-time/int → 400).

import (
	"context"

	"github.com/danielgtaylor/huma/v2"

	"github.com/souls-guild/soul-stack/keeper/internal/api/handlers"
	"github.com/souls-guild/soul-stack/keeper/internal/api/problem"
)

// registerHumaAuditList монтирует GET /v1/audit через huma на переданный chi.Router
// (та группа, что уже несёт RequireJWT/RequirePermission(audit.read)/maxBody/metrics).
// auditH — доменный handler; nil → no-op (паттерн opt-in-домена router.go: роут
// подключается только при non-nil auditH).
//
// READ-вариант tier-а: huma биндит/валидирует typed-query (bad date-time/int → 400
// через error-override; bad source-enum → 422), конвертит в handlers.AuditListFilter
// → ListTyped → typed output. Без claims-чтения (audit.read навешан на группе; AID-
// фильтр идёт query-параметром, не из JWT).
func registerHumaAuditList(humaAPI huma.API, auditH *handlers.AuditHandler) {
	if auditH == nil {
		return
	}
	huma.Register(humaAPI, auditListOperation(), func(ctx context.Context, in *auditListInput) (*auditListOutput, error) {
		reply, err := auditH.ListTyped(ctx, toAuditListFilter(in))
		if err != nil {
			return nil, auditProblem(err)
		}
		return &auditListOutput{Body: newAuditEventListReply(reply)}, nil
	})
}

// toAuditListFilter — конверт typed huma-input → доменный handlers.AuditListFilter
// (ADR-054 §Pattern шаг 3, тонкий клей). zero-time StartedAfter/Before huma выставляет
// при опущенном параметре (он не вызывает parseInto) → доменная ListTyped трактует
// IsZero как «без временной границы» (parity легаси `if param != ""`). Offset/Limit
// huma уже подставил default (0 / 50) при опущенных — совпадает с ParsePage.
// int32→int — расширяющий каст без потери (пагинация ≤ int32-диапазона); границы
// (offset≥0, limit∈[1,1000]) проверяет доменная CheckPageBounds → 400 (НЕ huma).
func toAuditListFilter(in *auditListInput) handlers.AuditListFilter {
	return handlers.AuditListFilter{
		Types:         in.Types,
		Sources:       in.Sources,
		ArchonAID:     in.ArchonAID,
		CorrelationID: in.CorrelationID,
		PayloadHerald: in.PayloadHerald,
		PayloadVoyage: in.PayloadVoyage,
		StartedAfter:  in.StartedAfter,
		StartedBefore: in.StartedBefore,
		Offset:        int(in.Offset),
		Limit:         int(in.Limit),
	}
}

// auditProblem доставляет ошибку ListTyped через huma как problem+json. Доменная
// *handlers.problemError → humaProblemError (его Details, статус из таблицы: невалидный
// source → 422; БД-сбой → 500). Не-problem (нештатный путь) → 500 internal (parity
// roleProblem / cadenceProblem).
func auditProblem(err error) huma.StatusError {
	if d, ok := handlers.AsProblemDetails(err); ok {
		return humaProblemError{Details: d}
	}
	return humaProblemError{Details: problem.New(problem.TypeInternalError, "", "internal error")}
}

// HumaAuditSpecYAML собирает OpenAPI-фрагмент мигрированного-на-huma GET /v1/audit как
// YAML-строку, БЕЗ монтирования на реальный router. Хук для спека-мерж-таргета тиража
// и guard-теста. Делегирует generic [humaDumpSpec], регистрируя операцию через тот же
// registerHumaAuditList (единый register-путь — нет дубля dump-vs-mount): handler при
// dump не вызывается. Возвращает 3.1.0-спеку (huma-дефолт).
func HumaAuditSpecYAML() (string, error) {
	return humaDumpSpec(func(api huma.API) error {
		registerHumaAuditList(api, handlers.AuditSpecStub())
		return nil
	})
}
