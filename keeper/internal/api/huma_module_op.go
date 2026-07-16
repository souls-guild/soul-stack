package api

// FULL-TYPED form of the MODULE domain (code-first OpenAPI source, ADR-054 §Pattern).
// ROLLOUT BATCH 2e (module entirely on huma, following catalog read-bare + form-prep
// read-with-body): list — read-with-typed-query (errand_safe bool filter), get —
// read-with-path, form-prep — read-with-body (SID-catalog resolve, no audit —
// read-only resolve, service.list pattern). Go types are the single source of truth.
//
// MODULE — a read-only domain ENTIRELY (audit is wired on no route): list/get —
// catalog (RBAC service.list), form-prep — SID resolve for the form (RBAC incarnation.run,
// read-only resolve). There is NO MCP for the module domain (the catalog has no MCP tools — verified).

import (
	"net/http"

	"github.com/danielgtaylor/huma/v2"

	"github.com/souls-guild/soul-stack/keeper/internal/api/handlers"
)

// === GET /v1/modules (list) — READ with typed query (no audit) ===

// moduleListInput — huma input for GET /v1/modules (FULL-TYPED typed query). ErrandSafe —
// bool filter `?errand_safe=true` (legacy read exactly the string "true"; huma bool-bind
// accepts true/false/1/0 — an acceptable extension, the domain filters by the flag).
type moduleListInput struct {
	ErrandSafe bool `query:"errand_safe" doc:"только модули с хотя бы одним errand-safe state (для Run→Command whitelist)"`
}

// moduleListOutput — huma output for GET /v1/modules (FULL-TYPED). Body — typed 200 envelope
// (handlers.ModuleCatalogReply: {items}). The wire shape (items non-nil, kind core|plugin,
// params/source/items nesting) is pinned by a golden-JSON byte-exact test.
type moduleListOutput struct {
	Body handlers.ModuleCatalogReply
}

// moduleListOperation — metadata for GET /v1/modules. Path = "/" relative to the chi group
// /v1/modules. DefaultStatus=200. READ route: audit not wired. Permission service.list.
// Errors: 403 RBAC, 500 (plugin-registry failure).
func moduleListOperation() huma.Operation {
	return huma.Operation{
		OperationID:   "listModules",
		Method:        http.MethodGet,
		Path:          "/",
		Summary:       "Каталог модулей",
		Description:   "Доступные for прогоon модули (core + активные plugin) + input-метаданные (ADR-045). Опц. фильтр errand_safe. Permission service.list. Read-only, no audit.",
		Tags:          []string{"module"},
		DefaultStatus: http.StatusOK,
		Errors:        []int{http.StatusForbidden, http.StatusInternalServerError},
	}
}

// === GET /v1/modules/{name} (get) — READ with path (no audit) ===

// moduleGetInput — huma input for GET /v1/modules/{name}. Name — path (full name without
// the state suffix: core.cmd / official.postgres-user).
type moduleGetInput struct {
	Name string `path:"name" doc:"полbutе имя модуля без state-суффикса"`
}

// moduleGetOutput — huma output for GET /v1/modules/{name} (FULL-TYPED). Body — typed
// 200 body (handlers.ModuleCatalogItem).
type moduleGetOutput struct {
	Body handlers.ModuleCatalogItem
}

// moduleGetOperation — metadata for GET /v1/modules/{name}. DefaultStatus=200. READ route:
// audit not wired. Permission service.list. Errors: 403, 404 (no module), 500.
func moduleGetOperation() huma.Operation {
	return huma.Operation{
		OperationID:   "getModule",
		Method:        http.MethodGet,
		Path:          "/{name}",
		Summary:       "Карточка модуля",
		Description:   "Деталь одbutго модуля по полbutму имени (ADR-045). Permission service.list. Read-only, no audit.",
		Tags:          []string{"module"},
		DefaultStatus: http.StatusOK,
		Errors:        []int{http.StatusForbidden, http.StatusNotFound, http.StatusInternalServerError},
	}
}

// === POST /v1/modules/{name}/form-prep (form-prep) — READ with body (no audit) ===

// moduleFormPrepInput — huma input for POST /v1/modules/{name}/form-prep. Name — path
// (per-module contract, not used in resolve). Body — typed body (source discriminator
// + optional prefix).
type moduleFormPrepInput struct {
	Name string `path:"name" doc:"полbutе имя модуля (per-module контракт, при резолве не используется)"`
	Body ModuleFormPrepRequest
}

// ModuleFormPrepRequest — Go form of the POST /v1/modules/{name}/form-prep body (code-first
// source of schema AND validation). Mirrors ModuleFormPrepRequest: source discriminator
// (exactly one of incarnation_hosts/choir; EMPTY/both → 422 in FormPrepTyped) + optional
// prefix filter. additionalProperties:false (huma default) → unknown body field → 400. Struct
// name = contract schema name (huma DefaultSchemaNamer; hand-written spec ModuleFormPrepRequest,
// N4).
type ModuleFormPrepRequest struct {
	Source ModuleFormPrepSource `json:"source" required:"true" doc:"дискримиonтор source-каталога (ровbut один from incarnation_hosts/choir)"`
	Prefix string               `json:"prefix,omitempty" doc:"префикс SID for автокомплита (LIKE prefix%)"`
}

// ModuleFormPrepSource — Go form of the source discriminator (class C input-only). Both
// sub-keys optional (the domain checks the XOR invariant → 422), value format is domain
// validation. Struct name = contract schema name (hand-written spec ModuleFormPrepSource, N4).
type ModuleFormPrepSource struct {
	IncarnationHosts string                     `json:"incarnation_hosts,omitempty" doc:"имя incarnation — live SID-ы её хостов"`
	Choir            *ModuleFormPrepChoirSource `json:"choir,omitempty" doc:"коордиonты Choir-source (incarnation + Choir name)"`
}

// ModuleFormPrepChoirSource — Go form of the Choir source (incarnation + name; class C input-only).
// Struct name = contract schema name (hand-written spec ModuleFormPrepChoirSource, N4).
type ModuleFormPrepChoirSource struct {
	Incarnation string `json:"incarnation" doc:"имя incarnation Choir-а"`
	Name        string `json:"name" doc:"Choir name"`
}

// ModuleFormPrepReply — native 200 body of POST /v1/modules/{name}/form-prep (flat). Shape 1:1 with
// ModuleFormPrepReply (types.gen.go :2305): sids ([]string, both fields required) + truncated.
// Struct name = contract schema name (huma DefaultSchemaNamer → "ModuleFormPrepReply"). The wire
// shape is pinned by a golden-JSON byte-exact test (huma_module_reply_test.go).
type ModuleFormPrepReply struct {
	Sids      []string `json:"sids"`
	Truncated bool     `json:"truncated"`
}

// moduleFormPrepOutput — huma output for POST /v1/modules/{name}/form-prep (FULL-TYPED).
// Body — huma-native 200 body (ModuleFormPrepReply: {sids, truncated}).
type moduleFormPrepOutput struct {
	Body ModuleFormPrepReply
}

// moduleFormPrepOperation — metadata for POST /v1/modules/{name}/form-prep.
// DefaultStatus=200. READ route (resolve, not mutation): audit not wired. Permission
// incarnation.run. Errors: 400 unknown/malformed, 403 RBAC, 422 invalid source, 500.
func moduleFormPrepOperation() huma.Operation {
	return huma.Operation{
		OperationID:   "moduleFormPrep",
		Method:        http.MethodPost,
		Path:          "/{name}/form-prep",
		Summary:       "Резолв source-каталога формы модуля",
		Description:   "Живые SID-ы под source-поля UI-формы Run→Command (ADR-045 S3). Permission incarnation.run. Read-only-резолв, no audit.",
		Tags:          []string{"module"},
		DefaultStatus: http.StatusOK,
		Errors:        []int{http.StatusBadRequest, http.StatusForbidden, http.StatusUnprocessableEntity, http.StatusInternalServerError},
	}
}

// toModuleFormPrepInput — wrapper typed huma-body → NATIVE request of the module domain
// (handlers.FormPrepInput). The huma form carries value/pointer; the handler form is flat value
// (handler.toFilter treats an empty string as "unset", parity with the legacy decode).
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

// newModuleFormPrepReply projects the domain handlers.FormPrepResult into the native 200 body
// ModuleFormPrepReply (byte-exact passthrough of the shape). sids nil-ness is preserved (the
// handler yields a non-nil sorted slice).
func newModuleFormPrepReply(r handlers.FormPrepResult) ModuleFormPrepReply {
	return ModuleFormPrepReply{Sids: r.Sids, Truncated: r.Truncated}
}
