package api

// FULL-TYPED форма GET /v1/audit (code-first источник OpenAPI, ADR-054 §Pattern
// ЧЕТВЁРТЫЙ tier — read-with-typed-query). Go-типы — единственный источник правды:
// huma строит из них И JSON Schema OpenAPI-фрагмента (query-параметры с типами/
// границами/enum), И typed-bind входа, И typed-output. ЭТАЛОН ~13-15 list-эндпоинтов
// с типизированной query.
//
// КЛЮЧЕВОЙ инвариант tier-а (контракт сохранён, решение A 2026-06-13): bad-value
// typed-query (started_after/before date-time, offset/limit int) → 400
// TypeMalformedRequest (huma parseInto-фейл → error-override hasQueryParseError); bad
// source-enum → 422 TypeValidationFailed (schema-validate enum-mismatch — другой
// Message, в parse-набор не попадает). Это продолжение ADR-051 Amendment (strict
// bind-фаза давала тот же 400/422), без product-fork.

import (
	"net/http"
	"time"

	"github.com/danielgtaylor/huma/v2"
)

// auditListInput — huma-input GET /v1/audit (FULL-TYPED typed-query). КАЖДОЕ поле
// несёт `query:"<name>"`-тег → huma биндит из url.Values и валидирует по схеме из
// тегов. RequestBody у GET НЕТ (huma не выводит body без Body-поля).
//
// Семантика bind-фазы (parity легаси AuditHandler.List → ListTyped):
//   - Types/Sources — multi-value (`?type=X&type=Y`), OR-семантика в доменной
//     ListTyped (event_type/source IN (...)); пустой slice → фильтр не применять.
//     `query:"…,explode"` ОБЯЗАТЕЛЕН на этих []string-полях: huma-дефолт query-
//     array — explode=false (читает comma-separated `?source=a,b` КАК ОДНО "a,b"
//     и эмитит `explode: false` в спеку → сгенерённый клиент закодирует составное
//     значение → сломанный OR-фильтр). `,explode` → huma читает повторяющийся
//     ключ `?source=a&source=b` И эмитит `explode: true` (huma v2.38 huma.go:157,
//     openapi.go Param.Explode); совпадает с committed-спекой (style:form +
//     explode:true) — match multi-value контракта тиража;
//   - Sources несёт `enum:"…"` — huma отбивает значение вне набора на 422
//     (schema-validate, НЕ parseInto) → error-override классифицирует как
//     TypeValidationFailed (КЛЮЧЕВОЙ контракт-инвариант: enum→422, не 400). Набор
//     enum = ПОЛНЫЙ доменный valid-set audit.Source.Valid() (signal/api/mcp/
//     keeper_internal/soul_grpc/background/config_bootstrap): config_bootstrap
//     реально эмитится (push/auto_import.go) и принимается доменом → его пропуск
//     отбивал бы рабочий фильтр `?source=config_bootstrap` на 422 (wire-regression
//     200→422). enum-тег синхронизируется С ДОМЕНОМ, committed-спека — следом;
//   - StartedAfter/StartedBefore — time.Time: huma parseInto на bad-value даёт
//     "invalid date/time for format …" → 400 (hasQueryParseError); zero-time
//     (параметр опущен) → доменный фильтр не применяет границу (filter.IsZero);
//   - Offset/Limit — int32 (НЕ Go-int: huma на int эмитит format:int64, committed-
//     спека/OffsetQuery/LimitQuery несут int32; пагинация влезает в int32) с
//     `default` (offset 0, limit 50), совпадающим с shared/api.
//     ParsePage. bad-int (нечисловое) → 400 (parseInto). ГРАНИЦЫ диапазона (offset≥0,
//     limit∈[1,1000]) НЕ выражены huma-тегами `minimum`/`maximum` СОЗНАТЕЛЬНО: huma
//     отбивал бы out-of-range на 422 (schema-validate), а легаси/strict-контракт
//     даёт на limit=0/1001/offset<0 ровно 400 (ParsePage TypeMalformedRequest). Диапазон
//     enforce-ит ДОМЕННАЯ ListTyped тем же сообщением ParsePage → 400 — иначе
//     wire-change. Документация диапазона несётся через `doc:` (не через schema-min/max).
type auditListInput struct {
	Types         []string  `query:"type,explode" doc:"multi-value ?type=X&type=Y — exact-match OR по event_type"`
	Sources       []string  `query:"source,explode" enum:"signal,api,mcp,keeper_internal,soul_grpc,background,config_bootstrap" doc:"multi-value ?source=api&source=mcp — exact-match OR; значение вне enum → 422"`
	ArchonAID     string    `query:"archon_aid" doc:"AID Архонта-инициатора (case-insensitive substring, ILIKE)"`
	CorrelationID string    `query:"correlation_id" doc:"ULID цепочки связанных событий (case-insensitive substring, ILIKE)"`
	PayloadHerald string    `query:"payload_herald" doc:"имя Herald-канала из payload->>'herald' (exact match)"`
	PayloadVoyage string    `query:"payload_voyage" doc:"voyage_id из payload->>'voyage_id' (exact match)"`
	StartedAfter  time.Time `query:"started_after" doc:"created_at >= started_after (RFC3339, включающая); bad-value → 400"`
	StartedBefore time.Time `query:"started_before" doc:"created_at <= started_before (RFC3339, включающая); bad-value → 400"`
	Offset        int32     `query:"offset" default:"0" doc:"сдвиг от начала набора, ≥0 (совпадает с shared/api.ParsePage; out-of-range → 400)"`
	Limit         int32     `query:"limit" default:"50" doc:"размер страницы 1..1000 (совпадает с shared/api.ParsePage; out-of-range → 400)"`
}

// auditListOutput — huma-output GET /v1/audit (FULL-TYPED). Body — native 200-тело
// (AuditEventListReply, тот же envelope {items,offset,limit,total}, что отдавала легаси
// writeJSON). Проекция доменной handlers.AuditListPage → native — на границе register-func
// (newAuditEventListReply). Wire-форма (items non-nil [], Source native enum-тип, created_at
// секундной точности) зафиксирована golden-JSON byte-exact-тестом (главный guard tier-а).
type auditListOutput struct {
	Body AuditEventListReply
}

// auditListOperation — метаданные GET /v1/audit. Path = "/audit" относительно
// chi-группы /v1 (huma.API смонтирован на ней; chi.Walk видит /v1/audit, drift-test
// зелёный — абсолютный, как permissionsListOperation, чтобы distinct-path исключал
// коллизию операций на общей /v1-API). DefaultStatus=200. READ-роут: audit НЕ навешан
// (чтение audit_log само audit-event не порождает — рекурсия). Errors: 400 (bad
// typed-query bind), 422 (bad source-enum / out-of-range pagination), 500 (БД).
func auditListOperation() huma.Operation {
	return huma.Operation{
		OperationID:   "listAuditEvents",
		Method:        http.MethodGet,
		Path:          "/audit",
		Summary:       "Лента audit-events (paged + фильтры)",
		Description:   "Read-only-лента audit_log с фильтрами (type/source multi-OR, archon_aid/correlation_id case-insensitive substring ILIKE, payload_herald/payload_voyage exact, started_after/before RFC3339) и пагинацией. Permission audit.read. Read-only, без audit (чтение не пишется — рекурсия).",
		Tags:          []string{"audit"},
		DefaultStatus: http.StatusOK,
		Errors:        []int{http.StatusBadRequest, http.StatusForbidden, http.StatusUnprocessableEntity, http.StatusInternalServerError},
	}
}
