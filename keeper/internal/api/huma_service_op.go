package api

// FULL-TYPED form of the SERVICE domain (Service registry; code-first source of OpenAPI,
// ADR-054 §Pattern). ROLLOUT-BATCH-2d (service entirely on huma following the role/
// operator/augur/herald blueprints): register — WRITE+AUDIT (service.registered, 201 WITH BODY);
// update — WRITE+AUDIT (service.updated, 200 WITH BODY); deregister — WRITE+AUDIT
// (service.deregistered, 204); list/get — read; refs/scenarios/state-schema/
// dependencies — read-with-path (opt. ?ref=, tier 502 on git-loader failure). Go types —
// the single source of truth (JSON Schema + validation + typed output).
//
// update — PATCH replace semantics for the mutable fields git/ref/refresh (NOT presence-tier:
// git/ref are required, refresh *string omitempty). list/get/refs — without pagination
// (ServiceListReply carries only items).

import (
	"net/http"
	"regexp"
	"strings"

	"github.com/danielgtaylor/huma/v2"

	"github.com/souls-guild/soul-stack/keeper/internal/api/handlers"
)

// === POST /v1/services (register) — WRITE+AUDIT service.registered ===

// serviceRegisterInput — huma-input POST /v1/services (FULL-TYPED). Body —
// the typed body: huma decodes and validates it against the schema from huma tags.
type serviceRegisterInput struct {
	Body ServiceRegisterRequest
}

// ServiceRegisterRequest — the Go form of the POST /v1/services body (code-first source
// of the schema AND validation). Mirrors the domain ServiceRegisterRequest: name+git+ref
// are required, refresh is optional (auto-refresh duration). The name/git/ref/refresh format —
// domain validation lives in RegisterTyped (422/409/404). The struct name = the contract
// schema name in OpenAPI (committed hand-written spec → ServiceRegisterRequest).
type ServiceRegisterRequest struct {
	Name    string  `json:"name" required:"true" pattern:"^[a-z][a-z0-9-]*$" doc:"Service name (kebab-case)"`
	Git     string  `json:"git" required:"true" doc:"git source of the service repo (URL; not a secret)"`
	Ref     string  `json:"ref" required:"true" doc:"git ref (tag/branch) - Service version (ADR-007)"`
	Refresh *string `json:"refresh,omitempty" doc:"opt. auto-refresh duration ('5m'); omitted - no auto-refresh"`
}

// serviceRegisterOutput — huma-output POST /v1/services (FULL-TYPED). Status=201;
// Body — the native 201 body (ServiceView). The wire shape (created_by_aid omitempty,
// created_at/updated_at at second precision) is pinned by a golden-JSON byte-exact test.
type serviceRegisterOutput struct {
	Status int `json:"-"`
	Body   ServiceView
}

// serviceRegisterOperation — metadata for POST /v1/services. Path = "/" relative to
// the chi group /v1/services. DefaultStatus=201. Permission service.register + audit
// service.registered. Errors: 400 unknown/malformed, 403 RBAC, 404 caller-not-found
// (FK), 409 service-exists, 422 name/git/ref/refresh validation, 500.
func serviceRegisterOperation() huma.Operation {
	return huma.Operation{
		OperationID:   "registerService",
		Method:        http.MethodPost,
		Path:          "/",
		Summary:       "Register a Service",
		Description:   "Registers the Service in the service_registry (ADR-028). Permission service.register. 409 - name taken. 404 - caller AID missing from the operator registry.",
		Tags:          []string{"service"},
		DefaultStatus: http.StatusCreated,
		Errors:        []int{http.StatusBadRequest, http.StatusForbidden, http.StatusNotFound, http.StatusConflict, http.StatusUnprocessableEntity, http.StatusInternalServerError},
	}
}

// === GET /v1/services (list) — READ (WITHOUT audit) ===

// serviceListInput — huma-input GET /v1/services. No parameters (the registry has no
// filters/pagination — ServiceListReply carries only items).
type serviceListInput struct{}

// serviceListOutput — huma-output GET /v1/services (FULL-TYPED). Body —
// the native 200 body (ServiceListReply: items under `items`, WITHOUT offset/limit/total).
// The wire shape of items is pinned by a golden-JSON byte-exact test.
type serviceListOutput struct {
	Body ServiceListReply
}

// serviceListOperation — metadata for GET /v1/services. Path = "/" relative to
// the chi group /v1/services. DefaultStatus=200. READ route: audit is NOT attached. Errors:
// 403 RBAC, 500.
func serviceListOperation() huma.Operation {
	return huma.Operation{
		OperationID:   "listServices",
		Method:        http.MethodGet,
		Path:          "/",
		Summary:       "List of Services",
		Description:   "Registry of Services (sort name ASC, ADR-028). Permission service.list. Read-only, no audit.",
		Tags:          []string{"service"},
		DefaultStatus: http.StatusOK,
		Errors:        []int{http.StatusForbidden, http.StatusInternalServerError},
	}
}

// === GET /v1/services/{name} (get) — READ-with-path (WITHOUT audit) ===

// serviceGetInput — huma-input GET /v1/services/{name}. Name — path parameter.
type serviceGetInput struct {
	Name string `path:"name" doc:"Service name"`
}

// serviceGetOutput — huma-output GET /v1/services/{name} (FULL-TYPED). Body —
// the native 200 body (ServiceView). The wire shape is pinned by a golden test.
type serviceGetOutput struct {
	Body ServiceView
}

// serviceGetOperation — metadata for GET /v1/services/{name}. DefaultStatus=200.
// READ route: audit is NOT attached. Permission service.list (read is covered by the list permission).
// Errors: 403 RBAC, 404 not-found, 500.
func serviceGetOperation() huma.Operation {
	return huma.Operation{
		OperationID:   "getService",
		Method:        http.MethodGet,
		Path:          "/{name}",
		Summary:       "Service card",
		Description:   "Metadata of a single registry entry by name (ADR-028). Permission service.list. Read-only, no audit.",
		Tags:          []string{"service"},
		DefaultStatus: http.StatusOK,
		Errors:        []int{http.StatusForbidden, http.StatusNotFound, http.StatusInternalServerError},
	}
}

// === PATCH /v1/services/{name} (update) — WRITE+AUDIT service.updated ===

// serviceUpdateInput — huma-input PATCH /v1/services/{name}. Name — path; Body —
// the typed body (replace of the mutable fields git/ref/refresh).
type serviceUpdateInput struct {
	Name string `path:"name" doc:"Service name (immutable)"`
	Body ServiceUpdateRequest
}

// ServiceUpdateRequest — the Go form of the PATCH /v1/services/{name} body (replace semantics
// for the mutable fields: git/ref required, refresh optional; name is immutable — comes from path). The struct
// name = the contract schema name in OpenAPI (committed hand-written spec → ServiceUpdateRequest).
type ServiceUpdateRequest struct {
	Git     string  `json:"git" required:"true" doc:"new git source"`
	Ref     string  `json:"ref" required:"true" doc:"new git ref"`
	Refresh *string `json:"refresh,omitempty" doc:"opt. auto-refresh duration ('5m')"`
}

// serviceUpdateOutput — huma-output PATCH /v1/services/{name} (FULL-TYPED).
// Status=200 WITH BODY (native ServiceView — the updated record). The wire shape
// is pinned by a golden test.
type serviceUpdateOutput struct {
	Status int `json:"-"`
	Body   ServiceView
}

// serviceUpdateOperation — metadata for PATCH /v1/services/{name}. DefaultStatus=200.
// Permission service.update + audit service.updated. Errors: 400 unknown/malformed,
// 403 RBAC, 404 not-found/caller-not-found, 422 git/ref/refresh validation, 500.
func serviceUpdateOperation() huma.Operation {
	return huma.Operation{
		OperationID:   "updateService",
		Method:        http.MethodPatch,
		Path:          "/{name}",
		Summary:       "Update a Service (replace mutable-fields)",
		Description:   "Replace semantics for git/ref/refresh, name is immutable (ADR-028). Permission service.update. 404 - entry absent.",
		Tags:          []string{"service"},
		DefaultStatus: http.StatusOK,
		Errors:        []int{http.StatusBadRequest, http.StatusForbidden, http.StatusNotFound, http.StatusUnprocessableEntity, http.StatusInternalServerError},
	}
}

// === DELETE /v1/services/{name} (deregister) — WRITE+AUDIT service.deregistered ===

// serviceDeregisterInput — huma-input DELETE /v1/services/{name}. Name — path. No Body.
type serviceDeregisterInput struct {
	Name string `path:"name" doc:"Service name"`
}

// serviceNoContentOutput — huma-output for the 204-write route deregister. WITHOUT Body
// (legacy contract: 204 No Content). huma on an output without Body does SetStatus(204)
// → empty body (wire-identical to the former WriteHeader(204)).
type serviceNoContentOutput struct {
	Status int `json:"-"`
}

// serviceDeregisterOperation — metadata for DELETE /v1/services/{name}.
// DefaultStatus=204. Permission service.deregister + audit service.deregistered.
// Errors: 403 RBAC, 404 not-found, 500.
func serviceDeregisterOperation() huma.Operation {
	return huma.Operation{
		OperationID:   "deregisterService",
		Method:        http.MethodDelete,
		Path:          "/{name}",
		Summary:       "Remove a Service from the registry",
		Description:   "Deletes the registry entry by name + invalidates caches (ADR-028). Permission service.deregister. 404 - entry absent.",
		Tags:          []string{"service"},
		DefaultStatus: http.StatusNoContent,
		Errors:        []int{http.StatusForbidden, http.StatusNotFound, http.StatusInternalServerError},
	}
}

// === GET /v1/services/{name}/refs (list-refs) — READ-with-path (WITHOUT audit) ===

// serviceRefsInput — huma-input GET /v1/services/{name}/refs. Name — path. No
// ?ref= (refs lists ALL tags+branches of the remote repo).
type serviceRefsInput struct {
	Name string `path:"name" doc:"Service name"`
}

// serviceRefsOutput — huma-output GET /v1/services/{name}/refs (FULL-TYPED). Body —
// the native 200 body (ServiceRefsListReply: service + refs[]). The wire shape
// is pinned by a golden test.
type serviceRefsOutput struct {
	Body ServiceRefsListReply
}

// serviceRefsOperation — metadata for GET /v1/services/{name}/refs. DefaultStatus=200.
// READ route: audit is NOT attached. Permission service.list (refs — a projection of the record).
// Errors: 403 RBAC, 404 not-found, 500 (no lister/registry failure), 502 ls-remote failed.
func serviceRefsOperation() huma.Operation {
	return huma.Operation{
		OperationID:   "listServiceRefs",
		Method:        http.MethodGet,
		Path:          "/{name}/refs",
		Summary:       "git tags + branches of a Service",
		Description:   "List of git refs of the Service remote repository for the UI Upgrade-modal (ADR-028). Permission service.list. Read-only, no audit. 502 - git source unreachable.",
		Tags:          []string{"service"},
		DefaultStatus: http.StatusOK,
		Errors:        []int{http.StatusForbidden, http.StatusNotFound, http.StatusInternalServerError, http.StatusBadGateway},
	}
}

// === GET /v1/services/{name}/scenarios (list-scenarios) — READ-with-path+query (WITHOUT audit) ===

// serviceScenariosInput — huma-input GET /v1/services/{name}/scenarios. Name — path;
// Ref — optional query override (omitted → ref from the registry).
type serviceScenariosInput struct {
	Name string `path:"name" doc:"Service name"`
	Ref  string `query:"ref" doc:"opt. git-ref override (omitted → ref from registry)"`
}

// serviceScenariosOutput — huma-output GET /v1/services/{name}/scenarios (FULL-TYPED).
// Body — handlers.ServiceScenariosReply (NOT an oapi alias: the element is artifact.Scenario with
// a plain-string Kind, see handlers/service.go). The wire shape is pinned by a golden test.
type serviceScenariosOutput struct {
	Body handlers.ServiceScenariosReply
}

// serviceScenariosOperation — metadata for GET /v1/services/{name}/scenarios.
// DefaultStatus=200. READ route: audit is NOT attached. Permission service.list. Errors:
// 403 RBAC, 404 not-found, 500 (no lister/registry failure), 502 loader failed.
func serviceScenariosOperation() huma.Operation {
	return huma.Operation{
		OperationID:   "listServiceScenarios",
		Method:        http.MethodGet,
		Path:          "/{name}/scenarios",
		Summary:       "scenario from a snapshot of the Service repo",
		Description:   "List of scenarios from a materialized snapshot of the Service git repo for the UI Run-modal (ADR-028). Permission service.list. Read-only, no audit. 502 - loader failed.",
		Tags:          []string{"service"},
		DefaultStatus: http.StatusOK,
		Errors:        []int{http.StatusForbidden, http.StatusNotFound, http.StatusInternalServerError, http.StatusBadGateway},
	}
}

// === GET /v1/services/{name}/state-schema (list-state-schema) — READ-with-path+query (WITHOUT audit) ===

// serviceStateSchemaInput — huma-input GET /v1/services/{name}/state-schema. Name —
// path; Ref — optional query override.
type serviceStateSchemaInput struct {
	Name string `path:"name" doc:"Service name"`
	Ref  string `query:"ref" doc:"opt. git-ref override (omitted → ref from registry)"`
}

// serviceStateSchemaOutput — huma-output GET /v1/services/{name}/state-schema
// (FULL-TYPED). Body — the native 200 body (ServiceStateSchemaReply). The wire shape
// is pinned by a golden test.
type serviceStateSchemaOutput struct {
	Body ServiceStateSchemaReply
}

// serviceStateSchemaOperation — metadata for GET /v1/services/{name}/state-schema.
// DefaultStatus=200. READ route: audit is NOT attached. Permission service.list. Errors:
// 403 RBAC, 404 not-found, 500 (no lister/registry failure), 502 loader failed.
func serviceStateSchemaOperation() huma.Operation {
	return huma.Operation{
		OperationID:   "listServiceStateSchema",
		Method:        http.MethodGet,
		Path:          "/{name}/state-schema",
		Summary:       "state_schema metadata of a Service",
		Description:   "state_schema version + structure declaration + migration chain (metadata-only) for the UI Schema explorer (ADR-019/028). Permission service.list. Read-only, no audit. 502 - loader failed.",
		Tags:          []string{"service"},
		DefaultStatus: http.StatusOK,
		Errors:        []int{http.StatusForbidden, http.StatusNotFound, http.StatusInternalServerError, http.StatusBadGateway},
	}
}

// === GET /v1/services/{name}/dependencies (list-dependencies) — READ-with-path+query (WITHOUT audit) ===

// serviceDependenciesInput — huma-input GET /v1/services/{name}/dependencies. Name —
// path; Ref — optional query override.
type serviceDependenciesInput struct {
	Name string `path:"name" doc:"Service name"`
	Ref  string `query:"ref" doc:"opt. git-ref override (omitted → ref from registry)"`
}

// serviceDependenciesOutput — huma-output GET /v1/services/{name}/dependencies
// (FULL-TYPED). Body — the native 200 body (ServiceDependenciesReply: service/ref +
// destiny[]/modules[]). The wire shape is pinned by a golden test.
type serviceDependenciesOutput struct {
	Body ServiceDependenciesReply
}

// serviceDependenciesOperation — metadata for GET /v1/services/{name}/dependencies.
// DefaultStatus=200. READ route: audit is NOT attached. Permission service.list. Errors:
// 403 RBAC, 404 not-found, 500 (no lister/registry failure), 502 loader failed.
func serviceDependenciesOperation() huma.Operation {
	return huma.Operation{
		OperationID:   "listServiceDependencies",
		Method:        http.MethodGet,
		Path:          "/{name}/dependencies",
		Summary:       "git dependencies of a Service",
		Description:   "Destiny building blocks + custom modules declared in service.yml with their own git refs, for the UI Service Detail (ADR-007/028). Permission service.list. Read-only, no audit. 502 - loader failed.",
		Tags:          []string{"service"},
		DefaultStatus: http.StatusOK,
		Errors:        []int{http.StatusForbidden, http.StatusNotFound, http.StatusInternalServerError, http.StatusBadGateway},
	}
}

// === GET /v1/services/{name}/directives (list-directives) — READ-with-path+query (WITHOUT audit) ===

// directivesCacheControlImmutable — Cache-Control for an IMMUTABLE ref (pinned 40-hex
// commit SHA): the content at that ref is cryptographically immutable → safe to cache
// for a year without revalidation.
const directivesCacheControlImmutable = "public, max-age=31536000, immutable"

// directivesCacheControlRevalidate — Cache-Control for a MUTABLE ref (branch/tag NAME,
// ADR-007: a branch is allowed as a version, a git tag permits force-move). `no-cache` =
// "cache it, but always revalidate before use": the browser sends If-None-Match
// → 304 (cheap), and the server-side invalidateDirectives (Update/Deregister) doesn't get stuck
// behind the year-long immutable cache — the new catalog reaches the UI.
const directivesCacheControlRevalidate = "no-cache"

// reImmutableRef — a ref in immutable form: a full 40-hex commit SHA. Branch/tag names
// (main / v1.2.3) do NOT match here — conservatively: there's no reliable way to tell a
// force-movable tag apart from a branch without git metadata, so any non-SHA ref → revalidate.
var reImmutableRef = regexp.MustCompile(`^[0-9a-fA-F]{40}$`)

// directivesCacheControlFor picks the Cache-Control based on the shape of ref (see the constants above):
// pinned commit SHA → immutable+year; branch/tag name → no-cache (revalidate via ETag/304).
func directivesCacheControlFor(ref string) string {
	if reImmutableRef.MatchString(ref) {
		return directivesCacheControlImmutable
	}
	return directivesCacheControlRevalidate
}

// serviceDirectivesInput — huma-input GET /v1/services/{name}/directives. Name —
// path; Ref/Version — optional query; If-None-Match — conditional-GET (304 on a match
// with ETag=snapshot SHA1).
type serviceDirectivesInput struct {
	Name        string `path:"name" doc:"Service name"`
	Ref         string `query:"ref" doc:"opt. git-ref override (omitted → ref from registry)"`
	Version     string `query:"version" doc:"opt. version (e.g. 8.2.2) - narrow the catalog to the major.minor series"`
	IfNoneMatch string `header:"If-None-Match" doc:"conditional GET: 304 if it matches the ETag (snapshot SHA1)"`
}

// serviceDirectivesOutput — huma-output GET /v1/services/{name}/directives (FULL-TYPED).
// Body — handlers.ServiceDirectivesReply. ETag/Cache-Control — response headers
// (header tags; json:"-"). Status=304 → huma doesn't write the body (huma.go transformAndWrite
// skips the body on StatusNotModified) — conditional-GET without the 31KB payload.
type serviceDirectivesOutput struct {
	Status       int    `json:"-"`
	ETag         string `header:"ETag" json:"-"`
	CacheControl string `header:"Cache-Control" json:"-"`
	Body         handlers.ServiceDirectivesReply
}

// serviceDirectivesOperation — metadata for GET /v1/services/{name}/directives.
// DefaultStatus=200. READ route: audit is NOT attached. Permission service.list. Errors:
// 403 RBAC, 404 not-found, 500 (no lister/registry failure), 502 loader failed.
func serviceDirectivesOperation() huma.Operation {
	return huma.Operation{
		OperationID:   "listServiceDirectives",
		Method:        http.MethodGet,
		Path:          "/{name}/directives",
		Summary:       "catalog of valid redis.conf directives by version",
		Description:   "Catalog of valid service directive names (essence.redis_directives, major.minor series map -> names) for the UI redis_settings editor (ADR-042). Permission service.list. Read-only, no audit. ?version=X.Y.Z narrows to the series. ETag=snapshot SHA1; If-None-Match -> 304. Cache-Control: immutable+year for a pinned commit-SHA ref, otherwise no-cache (branch/tag mutable - revalidation via ETag/304). Service without a catalog -> directives:{} + 200. 502 - loader failed.",
		Tags:          []string{"service"},
		DefaultStatus: http.StatusOK,
		Errors:        []int{http.StatusForbidden, http.StatusNotFound, http.StatusInternalServerError, http.StatusBadGateway},
	}
}

// === GET /v1/services/{name}/telemetry (get-telemetry) — READ-with-path+query (no audit) ===

// serviceTelemetryInput — huma-input GET /v1/services/{name}/telemetry. Name — path;
// Ref — optional query-override; If-None-Match — conditional-GET (304 on match with
// ETag=snapshot SHA1).
type serviceTelemetryInput struct {
	Name        string `path:"name" doc:"Service name"`
	Ref         string `query:"ref" doc:"optional git-ref override (omitted -> ref from registry)"`
	IfNoneMatch string `header:"If-None-Match" doc:"conditional GET: 304 if it matches ETag (snapshot SHA1)"`
}

// serviceTelemetryOutput — huma-output GET /v1/services/{name}/telemetry (FULL-TYPED).
// Body — handlers.ServiceTelemetryReply. ETag/Cache-Control — response headers
// (header tags; json:"-"). Status=304 -> huma does not write a body (conditional-GET without a payload).
type serviceTelemetryOutput struct {
	Status       int    `json:"-"`
	ETag         string `header:"ETag" json:"-"`
	CacheControl string `header:"Cache-Control" json:"-"`
	Body         handlers.ServiceTelemetryReply
}

// serviceTelemetryOperation — metadata for GET /v1/services/{name}/telemetry.
// DefaultStatus=200. READ route: audit is NOT attached. Permission service.list. Errors:
// 403 RBAC, 404 not-found, 500 (no lister / registry failure), 502 loader failed.
func serviceTelemetryOperation() huma.Operation {
	return huma.Operation{
		OperationID:   "getServiceTelemetry",
		Method:        http.MethodGet,
		Path:          "/{name}/telemetry",
		Summary:       "default host-vitals telemetry config of a Service + allowed collectors",
		Description:   "Effective default (per-service, without essence/incarnation) host-vitals config of the service (enabled/interval_sec/collectors) from the manifest `telemetry:` + known_collectors (full allowed set for the UI, ADR-042 backend-driven, ADR-072). Permission service.list. Read-only, no audit. ETag=snapshot SHA1; If-None-Match -> 304. Cache-Control: immutable+year for pinned commit-SHA ref, otherwise no-cache (mutable branch/tag). A service without a telemetry block -> manifest defaults (enabled=true, interval_sec=30, all collectors) + 200. 502 - loader failed. Not to be confused with /v1/incarnations/{name}/telemetry (runtime host-vitals from Redis, NIM-86).",
		Tags:          []string{"service"},
		DefaultStatus: http.StatusOK,
		Errors:        []int{http.StatusForbidden, http.StatusNotFound, http.StatusInternalServerError, http.StatusBadGateway},
	}
}

// etagQuote wraps the snapshot SHA1 in a strong ETag (`"<sha1>"`, RFC 7232).
func etagQuote(sha1 string) string {
	return `"` + sha1 + `"`
}

// etagMatchesSHA1 — did If-None-Match match the current SHA1 (conditional-GET).
// Parses the comma-list of ETags, strips the `W/` prefix and quotes; `*` matches anything.
// An empty SHA1/If-None-Match → false (nothing to compare).
func etagMatchesSHA1(ifNoneMatch, sha1 string) bool {
	if sha1 == "" || ifNoneMatch == "" {
		return false
	}
	for _, tok := range strings.Split(ifNoneMatch, ",") {
		tok = strings.TrimSpace(tok)
		if tok == "*" {
			return true
		}
		tok = strings.TrimPrefix(tok, "W/")
		if strings.Trim(tok, `"`) == sha1 {
			return true
		}
	}
	return false
}
