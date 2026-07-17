// Service handlers for the Operator API (Service registry, ADR-028 RBAC-storage
// pattern) — a domain layer over [serviceregistry.Service]. *Typed functions carry
// business logic without http.ResponseWriter/*http.Request; HTTP is served by huma
// full-typed (api/huma_service.go), MCP calls serviceregistry.Service directly.
//
// T5d (handler-native): the service domain is decoupled from the legacy codegen. *Typed
// functions accept NATIVE request types (organized via huma-input in the api package) and
// return domain result types with FLAT wire fields. The (w,r) wrappers are gone.
//
// Business logic (name/git/ref/refresh validation, invalidate hook after commit)
// lives in [serviceregistry.Service]; the handler maps sentinel errors to RFC 7807.
// RBAC check is in middleware (see api/router.go), not here.
package handlers

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"time"

	"github.com/souls-guild/soul-stack/keeper/internal/api/middleware"
	"github.com/souls-guild/soul-stack/keeper/internal/api/problem"
	"github.com/souls-guild/soul-stack/keeper/internal/artifact"
	"github.com/souls-guild/soul-stack/keeper/internal/jwt"
	"github.com/souls-guild/soul-stack/keeper/internal/scenario"
	"github.com/souls-guild/soul-stack/keeper/internal/serviceregistry"
	"github.com/souls-guild/soul-stack/shared/config"
)

// ServiceRefsLister — the listing surface for git refs of a single Service.
// name is resolved to gitURL OUTSIDE the handler (the server-side cacher decides
// whether a real ls-remote is needed or the cached entry is enough). When
// nil, the corresponding /refs endpoint responds 500 "not configured" (the
// ServiceLoader/PushRun pattern: the feature is optional, returns 5xx until wired up).
type ServiceRefsLister interface {
	ListRefs(ctx context.Context, name, gitURL string) ([]artifact.GitRef, error)
}

// ServiceScenarioLister — the listing surface for scenarios from a materialized
// snapshot of the Service repo for a single `(name, ref)`. Symmetric to [ServiceRefsLister]:
// the handler takes a minimal dependency, the real git-clone + parsing of
// scenario/*/main.yml lives inside the implementation (TTL cache + ServiceLoader). When nil,
// `GET /v1/services/{name}/scenarios` responds 500 "not configured".
type ServiceScenarioLister interface {
	ListScenarios(ctx context.Context, name, gitURL, ref string) ([]artifact.Scenario, error)
}

// ServiceStateSchemaLister — the listing surface for state-schema metadata
// (`state_schema_version` + an optional state structure declaration + the migration
// chain) from a materialized snapshot of the Service repo for `(name, ref)`.
// Symmetric to [ServiceScenarioLister]: the handler takes a minimal
// dependency, the real git-clone + parsing of service.yml + scanning migrations/
// lives inside the implementation (TTL cache + ServiceLoader). When nil,
// `GET /v1/services/{name}/state-schema` responds 500 "not configured".
type ServiceStateSchemaLister interface {
	ListStateSchema(ctx context.Context, name, gitURL, ref string) (*artifact.StateSchemaInfo, error)
}

// ServiceDependenciesLister — the listing surface for git dependencies
// (`destiny:`/`modules:` from `service.yml`) of a single Service repo snapshot for
// `(name, ref)`. Symmetric to [ServiceStateSchemaLister]: the handler takes a
// minimal dependency, the real git-clone + parsing of service.yml lives inside
// the implementation (TTL cache + ServiceLoader). When nil,
// `GET /v1/services/{name}/dependencies` responds 500 "not configured".
type ServiceDependenciesLister interface {
	ListDependencies(ctx context.Context, name, gitURL, ref string) (*artifact.ServiceDependencies, error)
}

// ServiceDirectivesLister — the surface for reading the FULL directive catalog (all
// series) of a service + snapshot SHA1 from a materialized snapshot of the Service repo for
// `(name, ref)`. Symmetric to [ServiceScenarioLister]: the handler takes a
// minimal dependency, the real git-clone + reading essence/_default.yaml lives
// inside the implementation (TTL cache + ServiceLoader). Version narrowing is done by the handler
// over the result (the cache is version-agnostic). When nil,
// `GET /v1/services/{name}/directives` responds 500 "not configured".
type ServiceDirectivesLister interface {
	ListDirectives(ctx context.Context, name, gitURL, ref string) (*artifact.DirectiveCatalog, error)
}

// ServiceTelemetryLister — поверхность чтения дефолтного (per-service, без essence)
// host-vitals telemetry-конфига сервиса + SHA1 снапшота (для ETag) из
// материализованного снапшота Service-репо для `(name, ref)`. Симметрично
// [ServiceDirectivesLister]: handler принимает минимальную зависимость, реальный
// git-clone + чтение манифеста (`telemetry:`) + резолв эффективных дефолтов —
// внутри реализации (TTL-кеш + ServiceLoader). При nil
// `GET /v1/services/{name}/telemetry` отвечает 500 «not configured».
type ServiceTelemetryLister interface {
	ListServiceTelemetry(ctx context.Context, name, gitURL, ref string) (*serviceregistry.TelemetryCatalog, error)
}

// ServiceHandler — the Service registry endpoints (register / list / get /
// update / deregister / list-refs / list-scenarios / list-state-schema).
// Delegates business logic to [serviceregistry.Service]; /refs / /scenarios /
// /state-schema each get a separate lister with TTL caches on the serviceregistry
// side (not registry CRUD logic).
//
// All dependencies are immutable; safe for concurrent use — holds no state between
// requests.
type ServiceHandler struct {
	svc          *serviceregistry.Service
	refs         ServiceRefsLister
	scenarios    ServiceScenarioLister
	stateSchema  ServiceStateSchemaLister
	dependencies ServiceDependenciesLister
	directives   ServiceDirectivesLister
	telemetry    ServiceTelemetryLister
	logger       *slog.Logger
}

// NewServiceHandler creates the handler. svc is required (panics on nil —
// the single misconfiguration point, the caller must pass non-nil). refs /
// scenarios / stateSchema / dependencies / directives / telemetry are optional: when
// nil, the corresponding endpoint responds 500 (feature not configured).
func NewServiceHandler(svc *serviceregistry.Service, refs ServiceRefsLister, scenarios ServiceScenarioLister, stateSchema ServiceStateSchemaLister, dependencies ServiceDependenciesLister, directives ServiceDirectivesLister, telemetry ServiceTelemetryLister, logger *slog.Logger) *ServiceHandler {
	if svc == nil {
		panic("handlers.NewServiceHandler: serviceregistry.Service is nil")
	}
	if logger == nil {
		logger = slog.New(slog.NewJSONHandler(io.Discard, nil))
	}
	return &ServiceHandler{svc: svc, refs: refs, scenarios: scenarios, stateSchema: stateSchema, dependencies: dependencies, directives: directives, telemetry: telemetry, logger: logger}
}

// ServiceSpecStub — a non-empty *ServiceHandler stub for generating the huma OpenAPI
// fragment (HumaServiceSpecYAML): the domain handler is not invoked during dump, but
// huma.Register requires non-nil for its no-op nil-check. svc/listers are nil —
// the handler never executes in spec mode (parity with [RoleSpecStub]).
func ServiceSpecStub() *ServiceHandler {
	return &ServiceHandler{logger: slog.New(slog.NewJSONHandler(io.Discard, nil))}
}

// ServiceRegisterInput — the NATIVE request shape for POST /v1/services (handler-native
// T5d). name+git+ref are required, refresh is optional (`*string`). Replaces
// ServiceRegisterRequest.
type ServiceRegisterInput struct {
	Name    string
	Git     string
	Ref     string
	Refresh *string
}

// ServiceUpdateInput — the NATIVE request shape for PATCH /v1/services/{name} (handler-
// native T5d). name is a path parameter (a key, not in the body); git+ref are required
// (replace semantics for mutable fields), refresh is optional.
type ServiceUpdateInput struct {
	Git     string
	Ref     string
	Refresh *string
}

// ServiceView — a FLAT domain projection of a Service registry entry (POST 201 /
// GET / PATCH 200 / list-element), handler-native T5d. created_by_aid/refresh/
// updated_by_aid are `*string` (nil → key omitted in the native projection); dates are
// truncated to seconds (UTC). Package api projects this into the native ServiceView schema.
type ServiceView struct {
	Name         string
	Git          string
	Ref          string
	Refresh      *string
	CreatedByAID *string
	UpdatedByAID *string
	CreatedAt    time.Time
	UpdatedAt    time.Time
}

// ServiceListPage — the domain list of Services for GET /v1/services (handler-native T5d).
type ServiceListPage struct {
	Items []ServiceView
}

// ServiceRegisterReply — the result of [ServiceHandler.RegisterTyped] (handler-native
// T5d). Carries the 201 body (flat ServiceView) + audit fields (name/git/ref + caller AID;
// the git URL is not a secret).
type ServiceRegisterReply struct {
	Body         ServiceView
	Name         string
	Git          string
	Ref          string
	CreatedByAID string
}

// AuditPayload assembles the audit payload for the register route (parity with the legacy SetAuditPayload).
// The SINGLE source for both the (w,r) wrapper AND the huma variant B.
func (r ServiceRegisterReply) AuditPayload() middleware.AuditPayload {
	return middleware.AuditPayload{
		"name":           r.Name,
		"git":            r.Git,
		"ref":            r.Ref,
		"created_by_aid": r.CreatedByAID,
	}
}

// RegisterTyped — the extracted domain function for POST /v1/services (the FULL-TYPED
// unfolding of ADR-054 §Pattern (b)): business logic without http.ResponseWriter/*http.
// Request. claims/req come in as arguments; errors are *problemError (via
// mapServiceError), success is [ServiceRegisterReply] (201 body + audit fields).
func (h *ServiceHandler) RegisterTyped(ctx context.Context, claims *jwt.Claims, req ServiceRegisterInput) (ServiceRegisterReply, error) {
	var zero ServiceRegisterReply
	callerAID := claims.Subject
	entry, err := h.svc.CreateService(ctx, serviceregistry.CreateServiceInput{
		Name:      req.Name,
		Git:       req.Git,
		Ref:       req.Ref,
		Refresh:   req.Refresh,
		CallerAID: &callerAID,
	})
	if err != nil {
		return zero, h.mapServiceError("service.register", req.Name, callerAID, err)
	}

	return ServiceRegisterReply{
		Body:         toServiceResponse(entry),
		Name:         entry.Name,
		Git:          entry.Git,
		Ref:          entry.Ref,
		CreatedByAID: callerAID,
	}, nil
}

// ListTyped — the domain function for GET /v1/services (handler-native T5d, READ without audit):
// reads the registry (sort name ASC) and assembles [ServiceListPage] (flat ServiceView)
// without http.ResponseWriter/*http.Request. A read error → *problemError (500).
// The wire form of items is built by the native projection in api.
func (h *ServiceHandler) ListTyped(ctx context.Context) (ServiceListPage, error) {
	entries, err := h.svc.ListServices(ctx)
	if err != nil {
		h.logger.Error("service.list: service failed", slog.Any("error", err))
		return ServiceListPage{}, &problemError{problem.New(problem.TypeInternalError, "", "list services failed")}
	}

	items := make([]ServiceView, 0, len(entries))
	for _, e := range entries {
		items = append(items, toServiceResponse(e))
	}
	return ServiceListPage{Items: items}, nil
}

// GetTyped — the domain function for GET /v1/services/{name} (handler-native T5d, READ
// without audit): reads a single entry by name without http.ResponseWriter/*http.Request. name
// comes in as an argument; errors are *problemError (404 not-found / 500), success is the flat
// [ServiceView].
func (h *ServiceHandler) GetTyped(ctx context.Context, name string) (ServiceView, error) {
	entry, err := h.svc.GetService(ctx, name)
	switch {
	case err == nil:
		return toServiceResponse(entry), nil
	case errors.Is(err, serviceregistry.ErrNotFound):
		return ServiceView{}, &problemError{problem.New(problem.TypeNotFound, "", "service "+name+" not found")}
	default:
		h.logger.Error("service.get: service failed",
			slog.String("name", name),
			slog.Any("error", err),
		)
		return ServiceView{}, &problemError{problem.New(problem.TypeInternalError, "", "get service failed")}
	}
}

// ServiceUpdateReply — the result of [ServiceHandler.UpdateTyped] (handler-native T5d).
// Carries the 200 body (flat ServiceView) + audit fields (name/git/ref).
type ServiceUpdateReply struct {
	Body ServiceView
	Name string
	Git  string
	Ref  string
}

// AuditPayload assembles the audit payload for the update route (parity with the legacy SetAuditPayload).
func (r ServiceUpdateReply) AuditPayload() middleware.AuditPayload {
	return middleware.AuditPayload{
		"name": r.Name,
		"git":  r.Git,
		"ref":  r.Ref,
	}
}

// UpdateTyped — the extracted domain function for PATCH /v1/services/{name} (the FULL-TYPED
// unfolding of ADR-054 §Pattern (b)): replaces mutable fields git/ref/refresh +
// cache invalidate hooks, without http.ResponseWriter/*http.Request. claims/name/req
// come in as arguments; errors are *problemError (via mapServiceError), success is
// [ServiceUpdateReply] (200 body + audit fields).
func (h *ServiceHandler) UpdateTyped(ctx context.Context, claims *jwt.Claims, name string, req ServiceUpdateInput) (ServiceUpdateReply, error) {
	var zero ServiceUpdateReply
	callerAID := claims.Subject
	entry, err := h.svc.UpdateService(ctx, serviceregistry.UpdateServiceInput{
		Name:      name,
		Git:       req.Git,
		Ref:       req.Ref,
		Refresh:   req.Refresh,
		CallerAID: &callerAID,
	})
	if err != nil {
		return zero, h.mapServiceError("service.update", name, callerAID, err)
	}
	h.invalidateRefs(entry.Name)
	h.invalidateScenarios(entry.Name)
	h.invalidateStateSchema(entry.Name)
	h.invalidateDependencies(entry.Name)
	h.invalidateDirectives(entry.Name)
	h.invalidateTelemetry(entry.Name)

	return ServiceUpdateReply{
		Body: toServiceResponse(entry),
		Name: entry.Name,
		Git:  entry.Git,
		Ref:  entry.Ref,
	}, nil
}

// ServiceNameReply — the result of write operations whose audit payload carries only the
// Service name (deregister). The 204 body is empty; reply is METADATA for audit.
type ServiceNameReply struct {
	Name string
}

// DeregisterTyped — the extracted domain function for DELETE /v1/services/{name}
// (the FULL-TYPED unfolding of ADR-054 §Pattern (b)): deletion by PK + cache invalidate
// hooks, without http.ResponseWriter/*http.Request. name comes in as an argument; errors are
// *problemError (404 not-found / 500), success is [ServiceNameReply] (audit payload).
// The 204 body is empty.
func (h *ServiceHandler) DeregisterTyped(ctx context.Context, name string) (ServiceNameReply, error) {
	var zero ServiceNameReply
	err := h.svc.DeleteService(ctx, name)
	switch {
	case err == nil:
		// fall through to reply.
	case errors.Is(err, serviceregistry.ErrNotFound):
		return zero, &problemError{problem.New(problem.TypeNotFound, "", "service "+name+" not found")}
	default:
		h.logger.Error("service.deregister: service failed",
			slog.String("name", name),
			slog.Any("error", err),
		)
		return zero, &problemError{problem.New(problem.TypeInternalError, "", "deregister service failed")}
	}

	h.invalidateRefs(name)
	h.invalidateScenarios(name)
	h.invalidateStateSchema(name)
	h.invalidateDependencies(name)
	h.invalidateDirectives(name)
	h.invalidateTelemetry(name)
	return ServiceNameReply{Name: name}, nil
}

// invalidateRefs — best-effort eviction of the refs cache entry for name after
// Update/Deregister. If the lister doesn't support Invalidate (the minimal
// ServiceRefsLister is a single-method interface) or the lister itself is nil — no-op.
//
// The "refs of the new git source will be picked up on the next request" semantics matters
// for UX: after a Service's git URL changes, the first time the Upgrade modal opens it should
// show tags from the new repo, not cached ones from the old one.
func (h *ServiceHandler) invalidateRefs(name string) {
	if h.refs == nil {
		return
	}
	if inv, ok := h.refs.(interface{ Invalidate(string) }); ok {
		inv.Invalidate(name)
	}
}

// invalidateScenarios — best-effort invalidation of the scenarios cache by name after
// Update/Deregister of a Service (paired semantics with [invalidateRefs]). After
// a git URL change or entry deletion, cached scenarios must disappear
// so the UI "Choose scenario" dropdown picks up the listing from the new source.
func (h *ServiceHandler) invalidateScenarios(name string) {
	if h.scenarios == nil {
		return
	}
	if inv, ok := h.scenarios.(interface{ Invalidate(string) }); ok {
		inv.Invalidate(name)
	}
}

// invalidateStateSchema — best-effort invalidation of the state-schema cache by name
// (paired semantics with [invalidateScenarios]). After a git URL change or
// entry deletion, the cached state-schema must disappear so the UI
// Schema explorer picks up the listing from the new source.
func (h *ServiceHandler) invalidateStateSchema(name string) {
	if h.stateSchema == nil {
		return
	}
	if inv, ok := h.stateSchema.(interface{ Invalidate(string) }); ok {
		inv.Invalidate(name)
	}
}

// invalidateDependencies — best-effort invalidation of the dependencies cache by name
// (paired semantics with [invalidateStateSchema]). After a git URL change or
// entry deletion, cached dependencies must disappear so the UI
// Service Detail picks up the listing from the new source.
func (h *ServiceHandler) invalidateDependencies(name string) {
	if h.dependencies == nil {
		return
	}
	if inv, ok := h.dependencies.(interface{ Invalidate(string) }); ok {
		inv.Invalidate(name)
	}
}

// invalidateDirectives — best-effort invalidation of the directives cache by name (paired
// semantics with [invalidateDependencies]). After a git URL change or entry deletion,
// the cached catalog must disappear so the UI redis_settings editor
// picks up the catalog from the new source.
func (h *ServiceHandler) invalidateDirectives(name string) {
	if h.directives == nil {
		return
	}
	if inv, ok := h.directives.(interface{ Invalidate(string) }); ok {
		inv.Invalidate(name)
	}
}

// invalidateTelemetry — best-effort инвалидация telemetry-кеша по name (парная
// семантика с [invalidateDirectives]). После смены git-URL или удаления записи
// закешированный telemetry-конфиг должен исчезнуть, чтобы UI подтянул дефолты
// нового источника.
func (h *ServiceHandler) invalidateTelemetry(name string) {
	if h.telemetry == nil {
		return
	}
	if inv, ok := h.telemetry.(interface{ Invalidate(string) }); ok {
		inv.Invalidate(name)
	}
}

// mapServiceError maps [serviceregistry.Service] sentinel errors
// (register/update share a single validation set + UNIQUE/FK boundaries) to *problemError
// (the FULL-TYPED unfolding of ADR-054 §Pattern: delivered by the huma wrapper via
// [AsProblemDetails] or by the (w,r) wrapper via [writeProblemError]).
// sentinel ↔ problem-type mapping:
//   - ErrAlreadyExists      → service-already-exists (409).
//   - ErrNotFound           → not-found (404; update of a nonexistent entry).
//   - ErrOperatorNotFound   → not-found (404; CallerAID missing from operators).
//   - ErrInvalidName / ErrInvalidGit / ErrInvalidRef / ErrInvalidRefresh →
//     validation-failed (422).
//
// For unknown errors — internal-error (500) + a generic detail (the raw err.Error()
// is not surfaced to the client; diagnostics go to the logs).
func (h *ServiceHandler) mapServiceError(op, name, callerAID string, err error) error {
	switch {
	case errors.Is(err, serviceregistry.ErrAlreadyExists):
		return &problemError{problem.New(problem.TypeServiceExists, "", "service "+name+" already exists")}
	case errors.Is(err, serviceregistry.ErrNotFound):
		return &problemError{problem.New(problem.TypeNotFound, "", "service "+name+" not found")}
	case errors.Is(err, serviceregistry.ErrOperatorNotFound):
		return &problemError{problem.New(problem.TypeNotFound, "", "caller AID "+callerAID+" not found in operators registry")}
	case errors.Is(err, serviceregistry.ErrInvalidName),
		errors.Is(err, serviceregistry.ErrInvalidGit),
		errors.Is(err, serviceregistry.ErrInvalidRef),
		errors.Is(err, serviceregistry.ErrInvalidRefresh):
		return &problemError{problem.New(problem.TypeValidationFailed, "", err.Error())}
	default:
		h.logger.Error(op+": service failed",
			slog.String("name", name),
			slog.String("by_aid", callerAID),
			slog.Any("error", err),
		)
		return &problemError{problem.New(problem.TypeInternalError, "", op+" failed")}
	}
}

// GitRefView — a FLAT domain git-ref entry (element of ServiceRefsList.Refs),
// handler-native T5d. IsDefault is bool (the native projection in api omits false as
// a nil pointer). Shaped after [artifact.GitRef].
type GitRefView struct {
	Name      string
	Type      string
	Commit    string
	IsDefault bool
}

// ServiceRefsList — the FLAT domain body for GET /v1/services/{name}/refs (handler-
// native T5d): service + refs[]. Package api projects this into the native ServiceRefsListReply.
type ServiceRefsList struct {
	Service string
	Refs    []GitRefView
}

// ListRefsTyped — the domain function for GET /v1/services/{name}/refs (handler-native T5d,
// READ without audit): resolves the entry + ls-remote of git tags/branches, without http.
// ResponseWriter/*http.Request. name comes in as an argument; errors are *problemError
// (500 no lister/read failure, 404 not-found, 502 ls-remote failed), success is
// [ServiceRefsList].
func (h *ServiceHandler) ListRefsTyped(ctx context.Context, name string) (ServiceRefsList, error) {
	var zero ServiceRefsList
	if h.refs == nil {
		return zero, &problemError{problem.New(problem.TypeInternalError, "", "service refs lister not configured")}
	}

	entry, err := h.svc.GetService(ctx, name)
	switch {
	case err == nil:
	case errors.Is(err, serviceregistry.ErrNotFound):
		return zero, &problemError{problem.New(problem.TypeNotFound, "", "service "+name+" not found")}
	default:
		h.logger.Error("service.refs: get service failed",
			slog.String("name", name),
			slog.Any("error", err),
		)
		return zero, &problemError{problem.New(problem.TypeInternalError, "", "get service failed")}
	}

	refs, err := h.refs.ListRefs(ctx, entry.Name, entry.Git)
	if err != nil {
		h.logger.Warn("service.refs: ls-remote failed",
			slog.String("name", name),
			slog.String("git", entry.Git),
			slog.Any("error", err),
		)
		return zero, &problemError{problem.New(problem.TypeBadGateway, "", "ls-remote failed for service "+name+": "+err.Error())}
	}
	return ServiceRefsList{
		Service: entry.Name,
		Refs:    toGitRefViews(refs),
	}, nil
}

// ServiceScenariosReply — the GET /v1/services/{name}/scenarios body. The service +
// ref fields duplicate the path parameter / selected ref for client convenience (one
// object is self-contained JSON; the UI puts the ref label next to the dropdown).
//
// NOT an alias for ServiceScenariosListReply: its element [Scenario] carries a
// typed enum field Kind (ScenarioKind), while the domain [artifact.Scenario].Kind is a
// plain string. Tests compare s.Kind against a string literal — a typed enum
// would break that comparison at compile time. The wire form is identical (same json tags).
// See the S0 report: "typed enum in response elements" — a recurring pattern.
// Exported (rather than package-private) so the huma wrapper [registerHumaServiceScenarios]
// in package api can name it as the Body type (FULL-TYPED ADR-054).
type ServiceScenariosReply struct {
	Service   string              `json:"service"`
	Ref       string              `json:"ref"`
	Scenarios []artifact.Scenario `json:"scenarios"`
}

// ListScenarios — GET /v1/services/{name}/scenarios. Returns the list of
// scenario metadata from a materialized snapshot of the Service's git repo (for
// the "Choose scenario" UI dropdown in the Run modal — paired with /refs for the Upgrade modal).
// Permission — service.list (the same Service-entry projection as /refs).
//
// The `ref` query parameter is optional: if not set, [ServiceEntry.Ref] is used
// (the current version from the registry). Scenarios are sorted alphabetically by name;
// invalid/empty scenarios are skipped (warning log, partial success).
// Read-only, no audit.
//
// Contract:
//   - 200 + {service, ref, scenarios:[…]}.
//   - 404 (not-found) — no entry with that name in the registry.
//   - 500 — internal failure (no lister / unexpected registry read error).
//   - 502 (bad-gateway) — git-clone / manifest parse failed on the loader side.
//
// ListScenariosTyped — the extracted domain function for GET /v1/services/{name}/
// scenarios (the FULL-TYPED unfolding of ADR-054 §Pattern, READ variant without audit): resolves
// the entry + lists scenarios from the git repo snapshot + tags kind/runnable, without
// http.ResponseWriter/*http.Request. name/ref come in as arguments (ref="" →
// default from the registry); errors are *problemError (500/404/502), success is
// [ServiceScenariosReply].
func (h *ServiceHandler) ListScenariosTyped(ctx context.Context, name, ref string) (ServiceScenariosReply, error) {
	var zero ServiceScenariosReply
	if h.scenarios == nil {
		return zero, &problemError{problem.New(problem.TypeInternalError, "", "service scenarios lister not configured")}
	}

	entry, err := h.svc.GetService(ctx, name)
	switch {
	case err == nil:
	case errors.Is(err, serviceregistry.ErrNotFound):
		return zero, &problemError{problem.New(problem.TypeNotFound, "", "service "+name+" not found")}
	default:
		h.logger.Error("service.scenarios: get service failed",
			slog.String("name", name),
			slog.Any("error", err),
		)
		return zero, &problemError{problem.New(problem.TypeInternalError, "", "get service failed")}
	}

	// `?ref=<git-ref>` — an optional override; defaults to the ref from the registry.
	if ref == "" {
		ref = entry.Ref
	}

	scenarios, err := h.scenarios.ListScenarios(ctx, entry.Name, entry.Git, ref)
	if err != nil {
		h.logger.Warn("service.scenarios: loader failed",
			slog.String("name", name),
			slog.String("git", entry.Git),
			slog.String("ref", ref),
			slog.Any("error", err),
		)
		return zero, &problemError{problem.New(problem.TypeBadGateway, "", "scenarios loader failed for service "+name+": "+err.Error())}
	}
	if scenarios == nil {
		scenarios = []artifact.Scenario{}
	}
	// kind + runnable tagging follows the canonical scenario package (the single source
	// of truth): the artifact loader doesn't fill these fields — the import direction is
	// artifact←scenario, not the other way around.
	for i := range scenarios {
		if scenario.IsLifecycleScenario(scenarios[i].Name) {
			scenarios[i].Kind = artifact.ScenarioKindLifecycle
		} else {
			scenarios[i].Kind = artifact.ScenarioKindOperational
		}
		scenarios[i].Runnable = scenario.IsRunnableScenario(scenarios[i].Name)
	}
	return ServiceScenariosReply{
		Service:   entry.Name,
		Ref:       ref,
		Scenarios: scenarios,
	}, nil
}

// StateSchemaMigration — a FLAT domain step of the migration chain (handler-native T5d).
// Shaped after [artifact.Migration] (from/to int + path).
type StateSchemaMigration struct {
	From int
	To   int
	Path string
}

// ServiceStateSchema — the FLAT domain body for GET /v1/services/{name}/state-schema
// (handler-native T5d). Schema is `map[string]any` (the native projection omits an empty
// map); Migrations is []StateSchemaMigration. Package api projects this into the native schema.
type ServiceStateSchema struct {
	Service            string
	Ref                string
	StateSchemaVersion int
	Schema             map[string]any
	Migrations         []StateSchemaMigration
}

// ListStateSchema — GET /v1/services/{name}/state-schema. Returns the
// service's state_schema metadata for the UI Schema explorer: the current version
// (`state_schema_version`), an optional state structure declaration (if
// the service declared one in `service.yml::state_schema`), and a flat list of
// migrations `<NNN>_to_<MMM>.yml` (metadata-only, no content).
// Permission — service.list (the same Service-entry projection as /refs /
// /scenarios).
//
// The `ref` query parameter is optional: if not set, [ServiceEntry.Ref] is used
// (the current version from the registry). Read-only, no audit. TTL cache 60s on the
// [ServiceStateSchemaLister] side.
//
// Contract:
//   - 200 + {service, ref, state_schema_version, schema?, migrations:[…]}.
//   - 404 (not-found) — no entry with that name in the registry.
//   - 500 — internal failure (no lister / unexpected registry read error).
//   - 502 (bad-gateway) — git-clone / manifest parse / migration scan failed
//     on the loader side.
//
// ListStateSchemaTyped — the extracted domain function for GET /v1/services/{name}/
// state-schema (the FULL-TYPED unfolding of ADR-054 §Pattern, READ variant without audit):
// resolves the entry + lists state-schema metadata, without http.ResponseWriter/*http.
// Request. name/ref come in as arguments (ref="" → default from the registry); errors are
// *problemError (500/404/502), success is [ServiceStateSchema].
func (h *ServiceHandler) ListStateSchemaTyped(ctx context.Context, name, ref string) (ServiceStateSchema, error) {
	var zero ServiceStateSchema
	if h.stateSchema == nil {
		return zero, &problemError{problem.New(problem.TypeInternalError, "", "service state-schema lister not configured")}
	}

	entry, err := h.svc.GetService(ctx, name)
	switch {
	case err == nil:
	case errors.Is(err, serviceregistry.ErrNotFound):
		return zero, &problemError{problem.New(problem.TypeNotFound, "", "service "+name+" not found")}
	default:
		h.logger.Error("service.state-schema: get service failed",
			slog.String("name", name),
			slog.Any("error", err),
		)
		return zero, &problemError{problem.New(problem.TypeInternalError, "", "get service failed")}
	}

	if ref == "" {
		ref = entry.Ref
	}

	info, err := h.stateSchema.ListStateSchema(ctx, entry.Name, entry.Git, ref)
	if err != nil {
		h.logger.Warn("service.state-schema: loader failed",
			slog.String("name", name),
			slog.String("git", entry.Git),
			slog.String("ref", ref),
			slog.Any("error", err),
		)
		return zero, &problemError{problem.New(problem.TypeBadGateway, "", "state-schema loader failed for service "+name+": "+err.Error())}
	}
	if info == nil {
		// Defensive: the lister must return non-nil when err=nil; otherwise we return
		// 502 — the implementation diverges from the contract.
		h.logger.Error("service.state-schema: loader returned nil info without error",
			slog.String("name", name))
		return zero, &problemError{problem.New(problem.TypeBadGateway, "", "state-schema loader returned empty result")}
	}

	return ServiceStateSchema{
		Service:            entry.Name,
		Ref:                ref,
		StateSchemaVersion: info.Version,
		Schema:             info.Schema,
		Migrations:         toMigrations(info.Migrations),
	}, nil
}

// ServiceDependency — a FLAT domain entry for destiny[]/modules[] (handler-native
// T5d). Git is a string (the native projection omits an empty one as a nil pointer). Shaped after
// [artifact.Dependency].
type ServiceDependency struct {
	Name string
	Ref  string
	Git  string
}

// ServiceDependenciesList — the FLAT domain body for GET /v1/services/{name}/dependencies
// (handler-native T5d): service/ref + destiny[]/modules[]. Package api projects this into
// the native ServiceDependenciesReply.
type ServiceDependenciesList struct {
	Service string
	Ref     string
	Destiny []ServiceDependency
	Modules []ServiceDependency
}

// ListDependencies — GET /v1/services/{name}/dependencies. Returns the
// service's git dependencies for the UI Service Detail: destiny bricks and custom
// modules declared in `service.yml`, each with its own git ref
// (ADR-007: version = git tag/branch). Permission — service.list (the same
// Service-entry projection as /refs / /scenarios / /state-schema).
//
// The `ref` query parameter is optional: if not set, [ServiceEntry.Ref] is used
// (the current version from the registry). Read-only, no audit. TTL cache 60s on the
// [ServiceDependenciesLister] side.
//
// Contract:
//   - 200 + {service, ref, destiny:[…], modules:[…]}.
//   - 404 (not-found) — no entry with that name in the registry.
//   - 500 — internal failure (no lister / unexpected registry read error).
//   - 502 (bad-gateway) — git-clone / manifest parse failed on the loader side.
//
// ListDependenciesTyped — the extracted domain function for GET /v1/services/{name}/
// dependencies (the FULL-TYPED unfolding of ADR-054 §Pattern, READ variant without audit):
// resolves the entry + lists git dependencies, without http.ResponseWriter/*http.Request.
// name/ref come in as arguments (ref="" → default from the registry); errors are
// *problemError (500/404/502), success is [ServiceDependenciesList].
func (h *ServiceHandler) ListDependenciesTyped(ctx context.Context, name, ref string) (ServiceDependenciesList, error) {
	var zero ServiceDependenciesList
	if h.dependencies == nil {
		return zero, &problemError{problem.New(problem.TypeInternalError, "", "service dependencies lister not configured")}
	}

	entry, err := h.svc.GetService(ctx, name)
	switch {
	case err == nil:
	case errors.Is(err, serviceregistry.ErrNotFound):
		return zero, &problemError{problem.New(problem.TypeNotFound, "", "service "+name+" not found")}
	default:
		h.logger.Error("service.dependencies: get service failed",
			slog.String("name", name),
			slog.Any("error", err),
		)
		return zero, &problemError{problem.New(problem.TypeInternalError, "", "get service failed")}
	}

	if ref == "" {
		ref = entry.Ref
	}

	deps, err := h.dependencies.ListDependencies(ctx, entry.Name, entry.Git, ref)
	if err != nil {
		h.logger.Warn("service.dependencies: loader failed",
			slog.String("name", name),
			slog.String("git", entry.Git),
			slog.String("ref", ref),
			slog.Any("error", err),
		)
		return zero, &problemError{problem.New(problem.TypeBadGateway, "", "dependencies loader failed for service "+name+": "+err.Error())}
	}
	if deps == nil {
		// Defensive: the lister must return non-nil when err=nil; otherwise we return
		// 502 — the implementation diverges from the contract (same pattern as ListStateSchema).
		h.logger.Error("service.dependencies: loader returned nil without error",
			slog.String("name", name))
		return zero, &problemError{problem.New(problem.TypeBadGateway, "", "dependencies loader returned empty result")}
	}

	return ServiceDependenciesList{
		Service: entry.Name,
		Ref:     ref,
		Destiny: toDependencyViews(deps.Destiny),
		Modules: toDependencyViews(deps.Modules),
	}, nil
}

// ServiceDirectivesReply — the GET /v1/services/{name}/directives body. Self-contained
// JSON (like ServiceScenariosReply): service + ref are echoed duplicates, sha1 is the snapshot hash (== ETag),
// directives is a map of `series(major.minor) → sorted names`. A service without a catalog
// → directives:{} (not null). Body directly (not a native DTO): elements are primitive
// strings, the huma schema is trivial. json tags fix the wire form; the huma registrar reads
// SHA1 for ETag/If-None-Match (see huma_service.go).
type ServiceDirectivesReply struct {
	Service    string              `json:"service"`
	Ref        string              `json:"ref"`
	SHA1       string              `json:"sha1"`
	Directives map[string][]string `json:"directives"`
}

// ListDirectivesTyped — GET /v1/services/{name}/directives (READ without audit): resolves
// the entry + reads the FULL directive catalog from the snapshot + version narrowing (optional).
// name/ref/version come in as arguments (ref="" → default from the registry; version="" →
// the entire catalog). Errors are *problemError (500 no lister/registry failure, 404 not-found,
// 502 loader failed), success is [ServiceDirectivesReply] with a non-nil (possibly empty)
// directives map.
func (h *ServiceHandler) ListDirectivesTyped(ctx context.Context, name, ref, version string) (ServiceDirectivesReply, error) {
	var zero ServiceDirectivesReply
	if h.directives == nil {
		return zero, &problemError{problem.New(problem.TypeInternalError, "", "service directives lister not configured")}
	}

	entry, err := h.svc.GetService(ctx, name)
	switch {
	case err == nil:
	case errors.Is(err, serviceregistry.ErrNotFound):
		return zero, &problemError{problem.New(problem.TypeNotFound, "", "service "+name+" not found")}
	default:
		h.logger.Error("service.directives: get service failed",
			slog.String("name", name),
			slog.Any("error", err),
		)
		return zero, &problemError{problem.New(problem.TypeInternalError, "", "get service failed")}
	}

	if ref == "" {
		ref = entry.Ref
	}

	catalog, err := h.directives.ListDirectives(ctx, entry.Name, entry.Git, ref)
	if err != nil {
		h.logger.Warn("service.directives: loader failed",
			slog.String("name", name),
			slog.String("git", entry.Git),
			slog.String("ref", ref),
			slog.Any("error", err),
		)
		return zero, &problemError{problem.New(problem.TypeBadGateway, "", "directives loader failed for service "+name+": "+err.Error())}
	}
	if catalog == nil {
		// Defensive: the lister must return non-nil when err=nil (same pattern as ListStateSchema).
		h.logger.Error("service.directives: loader returned nil without error",
			slog.String("name", name))
		return zero, &problemError{problem.New(problem.TypeBadGateway, "", "directives loader returned empty result")}
	}

	// Version narrowing happens over the full (cached) catalog; an empty catalog →
	// a non-nil {} (not null) for graceful frontend degradation.
	dirs := artifact.FilterDirectivesByVersion(catalog.Directives, version)
	if dirs == nil {
		dirs = map[string][]string{}
	}
	return ServiceDirectivesReply{
		Service:    entry.Name,
		Ref:        ref,
		SHA1:       catalog.SHA1,
		Directives: dirs,
	}, nil
}

// ServiceTelemetryReply — GET /v1/services/{name}/telemetry body. Самодостаточный
// JSON (как ServiceDirectivesReply): service + ref эхо-дубли, sha1 снапшота (== ETag),
// эффективный дефолтный (per-service, без essence/инкарнации) host-vitals-конфиг
// (enabled/interval_sec/collectors) + known_collectors — полный допустимый набор
// коллекторов для UI (ADR-042 backend-driven, ADR-072). Collectors пусто → `[]`
// (не null); KnownCollectors всегда полный набор. json-теги фиксируют wire; huma-
// регистратор читает SHA1 для ETag/If-None-Match (см. huma_service.go).
type ServiceTelemetryReply struct {
	Service         string   `json:"service"`
	Ref             string   `json:"ref"`
	SHA1            string   `json:"sha1"`
	Enabled         bool     `json:"enabled"`
	IntervalSec     int32    `json:"interval_sec"`
	Collectors      []string `json:"collectors"`
	KnownCollectors []string `json:"known_collectors"`
}

// ListServiceTelemetryTyped — GET /v1/services/{name}/telemetry (READ без audit):
// резолв записи + чтение дефолтного telemetry-конфига из снапшота манифеста + полный
// набор допустимых коллекторов (config.KnownCollectors) для UI. name/ref приходят
// аргументами (ref="" → дефолт из реестра). Ошибки — *problemError (500 нет lister-а/
// сбой реестра, 404 not-found, 502 loader упал), успех — [ServiceTelemetryReply].
func (h *ServiceHandler) ListServiceTelemetryTyped(ctx context.Context, name, ref string) (ServiceTelemetryReply, error) {
	var zero ServiceTelemetryReply
	if h.telemetry == nil {
		return zero, &problemError{problem.New(problem.TypeInternalError, "", "service telemetry lister not configured")}
	}

	entry, err := h.svc.GetService(ctx, name)
	switch {
	case err == nil:
	case errors.Is(err, serviceregistry.ErrNotFound):
		return zero, &problemError{problem.New(problem.TypeNotFound, "", "service "+name+" not found")}
	default:
		h.logger.Error("service.telemetry: get service failed",
			slog.String("name", name),
			slog.Any("error", err),
		)
		return zero, &problemError{problem.New(problem.TypeInternalError, "", "get service failed")}
	}

	if ref == "" {
		ref = entry.Ref
	}

	catalog, err := h.telemetry.ListServiceTelemetry(ctx, entry.Name, entry.Git, ref)
	if err != nil {
		h.logger.Warn("service.telemetry: loader failed",
			slog.String("name", name),
			slog.String("git", entry.Git),
			slog.String("ref", ref),
			slog.Any("error", err),
		)
		return zero, &problemError{problem.New(problem.TypeBadGateway, "", "telemetry loader failed for service "+name+": "+err.Error())}
	}
	if catalog == nil || catalog.Telemetry == nil {
		// Defensive: lister обязан вернуть non-nil при err=nil (паттерн ListStateSchema).
		h.logger.Error("service.telemetry: loader returned nil without error",
			slog.String("name", name))
		return zero, &problemError{problem.New(problem.TypeBadGateway, "", "telemetry loader returned empty result")}
	}

	// Collectors пусто → `[]` (не null) для мягкой деградации фронта; known_collectors —
	// всегда полный допустимый набор (копия — не отдаём наружу пакетную переменную).
	collectors := catalog.Telemetry.GetCollectors()
	if collectors == nil {
		collectors = []string{}
	}
	known := make([]string, len(config.KnownCollectors))
	copy(known, config.KnownCollectors)

	return ServiceTelemetryReply{
		Service:         entry.Name,
		Ref:             ref,
		SHA1:            catalog.SHA1,
		Enabled:         catalog.Telemetry.GetEnabled(),
		IntervalSec:     catalog.Telemetry.GetIntervalSec(),
		Collectors:      collectors,
		KnownCollectors: known,
	}, nil
}

// --- domain → FLAT-domain projection of list bodies (handler-native T5d) ---
//
// The service layer returns domain [artifact.*] types; the handler assembles flat domain
// view types, the native projection in api builds the native wire form (omitempty/[]-vs-null/enum).
// All return a non-nil slice (empty → `[]`, not null — same contract as before).

func toGitRefViews(in []artifact.GitRef) []GitRefView {
	out := make([]GitRefView, 0, len(in))
	for _, r := range in {
		out = append(out, GitRefView{Name: r.Name, Type: r.Type, Commit: r.Commit, IsDefault: r.IsDefault})
	}
	return out
}

func toMigrations(in []artifact.Migration) []StateSchemaMigration {
	out := make([]StateSchemaMigration, 0, len(in))
	for _, m := range in {
		out = append(out, StateSchemaMigration{From: m.From, To: m.To, Path: m.Path})
	}
	return out
}

func toDependencyViews(in []artifact.Dependency) []ServiceDependency {
	out := make([]ServiceDependency, 0, len(in))
	for _, d := range in {
		out = append(out, ServiceDependency{Name: d.Name, Ref: d.Ref, Git: d.Git})
	}
	return out
}

// toServiceResponse projects [serviceregistry.ServiceEntry] into the FLAT domain
// [ServiceView] (handler-native T5d). Dates are UTC, truncated to seconds: the native
// ServiceView carries time.Time (date-time wire); Truncate(Second) preserves the previous
// wire form (no nanoseconds).
func toServiceResponse(e *serviceregistry.ServiceEntry) ServiceView {
	return ServiceView{
		Name:         e.Name,
		Git:          e.Git,
		Ref:          e.Ref,
		Refresh:      e.Refresh,
		CreatedByAID: e.CreatedByAID,
		UpdatedByAID: e.UpdatedByAID,
		CreatedAt:    e.CreatedAt.UTC().Truncate(time.Second),
		UpdatedAt:    e.UpdatedAt.UTC().Truncate(time.Second),
	}
}
