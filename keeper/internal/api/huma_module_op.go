package api

// FULL-TYPED форма MODULE-домена (code-first источник OpenAPI, ADR-054 §Pattern).
// ТИРАЖ-БАТЧ-2e (module целиком на huma по эталону catalog read-bare + form-prep
// read-with-body): list — read-with-typed-query (errand_safe bool-фильтр), get —
// read-with-path, form-prep — read-with-body (резолв SID-каталога, БЕЗ audit —
// read-only-резолв, паттерн service.list). Go-типы — единственный источник правды.
//
// MODULE — read-only-домен ЦЕЛИКОМ (audit НЕ навешивается ни на один роут): list/get —
// каталог (RBAC service.list), form-prep — резолв SID под форму (RBAC incarnation.run,
// read-only-резолв). MCP module-домена НЕТ (каталог не имеет MCP-tool-ов — проверено).

import (
	"net/http"

	"github.com/danielgtaylor/huma/v2"

	"github.com/souls-guild/soul-stack/keeper/internal/api/handlers"
)

// === GET /v1/modules (list) — READ-with-typed-query (БЕЗ audit) ===

// moduleListInput — huma-input GET /v1/modules (FULL-TYPED typed-query). ErrandSafe —
// bool-фильтр `?errand_safe=true` (legacy читал ровно строку "true"; huma bool-bind
// принимает true/false/1/0 — расширение допустимое, домен фильтрует по флагу).
type moduleListInput struct {
	ErrandSafe bool `query:"errand_safe" doc:"только модули с хотя бы одним errand-safe state (для Run→Command whitelist)"`
}

// moduleListOutput — huma-output GET /v1/modules (FULL-TYPED). Body — typed 200-envelope
// (handlers.ModuleCatalogReply: {items}). Wire-форма (items non-nil, kind core|plugin,
// params/source/items вложенность) зафиксирована golden-JSON byte-exact-тестом.
type moduleListOutput struct {
	Body handlers.ModuleCatalogReply
}

// moduleListOperation — метаданные GET /v1/modules. Path = "/" относительно chi-группы
// /v1/modules. DefaultStatus=200. READ-роут: audit НЕ навешан. Permission service.list.
// Errors: 403 RBAC, 500 (сбой plugin-реестра).
func moduleListOperation() huma.Operation {
	return huma.Operation{
		OperationID:   "listModules",
		Method:        http.MethodGet,
		Path:          "/",
		Summary:       "Каталог модулей",
		Description:   "Доступные для прогона модули (core + активные plugin) + input-метаданные (ADR-045). Опц. фильтр errand_safe. Permission service.list. Read-only, без audit.",
		Tags:          []string{"module"},
		DefaultStatus: http.StatusOK,
		Errors:        []int{http.StatusForbidden, http.StatusInternalServerError},
	}
}

// === GET /v1/modules/{name} (get) — READ-with-path (БЕЗ audit) ===

// moduleGetInput — huma-input GET /v1/modules/{name}. Name — path (полное имя без
// state-суффикса: core.cmd / official.postgres-user).
type moduleGetInput struct {
	Name string `path:"name" doc:"полное имя модуля без state-суффикса"`
}

// moduleGetOutput — huma-output GET /v1/modules/{name} (FULL-TYPED). Body — typed
// 200-тело (handlers.ModuleCatalogItem).
type moduleGetOutput struct {
	Body handlers.ModuleCatalogItem
}

// moduleGetOperation — метаданные GET /v1/modules/{name}. DefaultStatus=200. READ-роут:
// audit НЕ навешан. Permission service.list. Errors: 403, 404 (нет модуля), 500.
func moduleGetOperation() huma.Operation {
	return huma.Operation{
		OperationID:   "getModule",
		Method:        http.MethodGet,
		Path:          "/{name}",
		Summary:       "Карточка модуля",
		Description:   "Деталь одного модуля по полному имени (ADR-045). Permission service.list. Read-only, без audit.",
		Tags:          []string{"module"},
		DefaultStatus: http.StatusOK,
		Errors:        []int{http.StatusForbidden, http.StatusNotFound, http.StatusInternalServerError},
	}
}

// === POST /v1/modules/{name}/form-prep (form-prep) — READ-with-body (БЕЗ audit) ===

// moduleFormPrepInput — huma-input POST /v1/modules/{name}/form-prep. Name — path
// (per-module контракт, при резолве не используется). Body — typed тело (source-
// дискриминатор + опц. prefix).
type moduleFormPrepInput struct {
	Name string `path:"name" doc:"полное имя модуля (per-module контракт, при резолве не используется)"`
	Body ModuleFormPrepRequest
}

// ModuleFormPrepRequest — Go-форма тела POST /v1/modules/{name}/form-prep (code-first
// источник схемы И валидации). Повторяет ModuleFormPrepRequest: source-дискриминатор
// (ровно один из incarnation_hosts/choir; ПУСТОЙ/двойной → 422 в FormPrepTyped) + опц.
// prefix-фильтр. additionalProperties:false (huma-дефолт) → unknown поле тела → 400. Имя
// структуры = контрактное имя схемы (huma DefaultSchemaNamer; рукопись ModuleFormPrepRequest,
// N4).
type ModuleFormPrepRequest struct {
	Source ModuleFormPrepSource `json:"source" required:"true" doc:"дискриминатор source-каталога (ровно один из incarnation_hosts/choir)"`
	Prefix string               `json:"prefix,omitempty" doc:"префикс SID для автокомплита (LIKE prefix%)"`
}

// ModuleFormPrepSource — Go-форма source-дискриминатора (класс C input-only). Оба под-ключа
// опц. (XOR-инвариант проверяет домен → 422), формат значений — доменная валидация. Имя
// структуры = контрактное имя схемы (рукопись ModuleFormPrepSource, N4).
type ModuleFormPrepSource struct {
	IncarnationHosts string                     `json:"incarnation_hosts,omitempty" doc:"имя incarnation — live SID-ы её хостов"`
	Choir            *ModuleFormPrepChoirSource `json:"choir,omitempty" doc:"координаты Choir-source (incarnation + имя Choir-а)"`
}

// ModuleFormPrepChoirSource — Go-форма Choir-source (incarnation + name; класс C input-only).
// Имя структуры = контрактное имя схемы (рукопись ModuleFormPrepChoirSource, N4).
type ModuleFormPrepChoirSource struct {
	Incarnation string `json:"incarnation" doc:"имя incarnation Choir-а"`
	Name        string `json:"name" doc:"имя Choir-а"`
}

// ModuleFormPrepReply — native 200-тело POST /v1/modules/{name}/form-prep (flat). Форма 1:1 с
// ModuleFormPrepReply (types.gen.go :2305): sids ([]string, оба поля required) + truncated.
// Имя структуры = контрактное имя схемы (huma DefaultSchemaNamer → "ModuleFormPrepReply"). Wire-
// форма зафиксирована golden-JSON byte-exact-тестом (huma_module_reply_test.go).
type ModuleFormPrepReply struct {
	Sids      []string `json:"sids"`
	Truncated bool     `json:"truncated"`
}

// moduleFormPrepOutput — huma-output POST /v1/modules/{name}/form-prep (FULL-TYPED).
// Body — huma-native 200-тело (ModuleFormPrepReply: {sids, truncated}).
type moduleFormPrepOutput struct {
	Body ModuleFormPrepReply
}

// moduleFormPrepOperation — метаданные POST /v1/modules/{name}/form-prep.
// DefaultStatus=200. READ-роут (резолв, не мутация): audit НЕ навешан. Permission
// incarnation.run. Errors: 400 unknown/malformed, 403 RBAC, 422 невалидный source, 500.
func moduleFormPrepOperation() huma.Operation {
	return huma.Operation{
		OperationID:   "moduleFormPrep",
		Method:        http.MethodPost,
		Path:          "/{name}/form-prep",
		Summary:       "Резолв source-каталога формы модуля",
		Description:   "Живые SID-ы под source-поля UI-формы Run→Command (ADR-045 S3). Permission incarnation.run. Read-only-резолв, без audit.",
		Tags:          []string{"module"},
		DefaultStatus: http.StatusOK,
		Errors:        []int{http.StatusBadRequest, http.StatusForbidden, http.StatusUnprocessableEntity, http.StatusInternalServerError},
	}
}

// toModuleFormPrepInput — конверт typed huma-body → NATIVE request module-домена
// (handlers.FormPrepInput). huma-форма несёт value/pointer; handler-форма — плоская value
// (handler.toFilter трактует пустую строку как «не задано», parity легаси-декода).
func toModuleFormPrepInput(b ModuleFormPrepRequest) handlers.FormPrepInput {
	out := handlers.FormPrepInput{Prefix: b.Prefix}
	out.Source.IncarnationHosts = b.Source.IncarnationHosts
	if b.Source.Choir != nil {
		out.Source.Choir = &handlers.FormPrepChoirSource{
			Incarnation: b.Source.Choir.Incarnation,
			Name:        b.Source.Choir.Name,
		}
	}
	return out
}

// newModuleFormPrepReply проецирует доменный handlers.FormPrepResult в native 200-тело
// ModuleFormPrepReply (byte-exact passthrough формы). sids nil-ность сохраняется (handler
// даёт non-nil отсортированный срез).
func newModuleFormPrepReply(r handlers.FormPrepResult) ModuleFormPrepReply {
	return ModuleFormPrepReply{Sids: r.Sids, Truncated: r.Truncated}
}
