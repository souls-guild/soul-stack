package api

// FULL-TYPED форма GET /v1/cadences (code-first источник OpenAPI, ADR-054 §Pattern
// ЧЕТВЁРТЫЙ tier — read-with-typed-query). Go-типы — единственный источник правды:
// huma строит из них И JSON Schema OpenAPI-фрагмента (query-параметры с типами/
// границами/enum), И typed-bind входа, И typed-output. Teardown T1 завершает
// перенос cadence-домена на huma (последний live strict-mount в /v1).
//
// КЛЮЧЕВОЙ инвариант tier-а (контракт сохранён, parity legacy List): bad-int
// offset/limit → 400 TypeMalformedRequest (huma parseInto-фейл → error-override
// hasQueryParseError); out-of-range offset/limit → 400 (домен CheckPageBounds, НЕ
// huma min/max); bad enabled/kind enum → 422 TypeValidationFailed (schema-validate
// enum-mismatch — другой Message, в parse-набор не попадает; ровно как leglist).

import (
	"net/http"

	"github.com/danielgtaylor/huma/v2"

	"github.com/souls-guild/soul-stack/keeper/internal/api/handlers"
)

// cadenceListInput — huma-input GET /v1/cadences (FULL-TYPED typed-query). КАЖДОЕ
// поле несёт `query:"<name>"`-тег → huma биндит из url.Values и валидирует по схеме
// из тегов. RequestBody у GET НЕТ (huma не выводит body без Body-поля).
//
// Семантика bind-фазы (parity легаси CadenceHandler.List → ListTyped):
//   - Enabled — string c enum:"true,false": empty (опущен) → фильтр не применять;
//     "true" → только enabled; "false" → без фильтра (показать все). Значение вне
//     набора → 422 (schema-validate enum-mismatch → error-override TypeValidationFailed,
//     ровно как legacy "query 'enabled' must be 'true' or 'false'"). НЕ *bool: легаси-
//     контракт даёт на bad-value 422, а *bool-parse дал бы 400.
//   - Kind — string c enum:"scenario,command": exact-match по kind рецепта; вне
//     набора → 422 (parity legacy ValidKind → 422). Опущен → без фильтра.
//   - Offset/Limit — int32 (НЕ Go-int: huma на int эмитит format:int64, committed-
//     спека несёт int32; пагинация влезает в int32) с `default` (offset 0, limit 50),
//     совпадающим с shared/api.ParsePage. bad-int (нечисловое) → 400 (parseInto).
//     ГРАНИЦЫ диапазона (offset≥0, limit∈[1,1000]) НЕ выражены huma-тегами
//     minimum/maximum СОЗНАТЕЛЬНО: huma отбивал бы out-of-range на 422, а легаси/
//     ParsePage даёт ровно 400 (TypeMalformedRequest). Диапазон enforce-ит доменная
//     ListTyped через CheckPageBounds → 400 — иначе wire-change.
type cadenceListInput struct {
	Enabled string `query:"enabled" enum:"true,false" doc:"фильтр по enabled: true → только включённые, false → все (без фильтра); опущен → все"`
	Kind    string `query:"kind" enum:"scenario,command" doc:"фильтр по типу рецепта (exact); вне набора → 422"`
	Offset  int32  `query:"offset" default:"0" doc:"сдвиг от начала набора, ≥0 (совпадает с shared/api.ParsePage; out-of-range → 400)"`
	Limit   int32  `query:"limit" default:"50" doc:"размер страницы 1..1000 (совпадает с shared/api.ParsePage; out-of-range → 400)"`
}

// cadenceListOutput — huma-output GET /v1/cadences (FULL-TYPED). Body — typed 200-
// envelope (handlers.CadenceListReply = sharedapi.PagedResponse[cadenceDTO], тот же
// конверт {items,offset,limit,total}, что отдавала легаси writeJSON). Wire-форма
// (items non-nil []) зафиксирована golden-JSON byte-exact-тестом (главный guard).
type cadenceListOutput struct {
	Body handlers.CadenceListReply
}

// cadenceListOperation — метаданные GET /v1/cadences. Path = "/" ОТНОСИТЕЛЬНЫЙ к
// chi-группе /v1/cadences (huma.API смонтирован на ней; chi.Walk видит /v1/cadences,
// drift-test зелёный). DefaultStatus=200. READ-роут: audit НЕ навешан. Permission
// cadence.list (read-tier — как legacy strict ListCadences). Errors: 400 (bad int /
// out-of-range pagination), 403 RBAC, 422 (bad enabled/kind enum), 500 (БД).
func cadenceListOperation() huma.Operation {
	return huma.Operation{
		OperationID:   "listCadences",
		Method:        http.MethodGet,
		Path:          "/",
		Summary:       "Список расписаний (Cadence, paged)",
		Description:   "Read-only-список Cadence с фильтрами enabled/kind и пагinацией (sort created_at DESC). Permission cadence.list. Read-only, без audit.",
		Tags:          []string{"cadence"},
		DefaultStatus: http.StatusOK,
		Errors:        []int{http.StatusBadRequest, http.StatusForbidden, http.StatusUnprocessableEntity, http.StatusInternalServerError},
	}
}
