// Service-handler-ы Operator API (реестр Service-ов, ADR-028-паттерн RBAC-
// storage) — доменный слой над [serviceregistry.Service]. *Typed-функции несут
// бизнес-логику без http.ResponseWriter/*http.Request; HTTP обслуживает huma
// full-typed (api/huma_service.go), MCP зовёт serviceregistry.Service напрямую.
//
// T5d (handler-native): домен service отвязан от legacy-генерата. *Typed принимают NATIVE
// request-типы (огранизованы huma-input-ом в пакете api) и возвращают доменные
// result-ы с ПЛОСКИМИ wire-полями. (w,r)-оболочки сняты.
//
// Бизнес-логика (валидация name/git/ref/refresh, invalidate-хук после commit-а)
// — в [serviceregistry.Service]; handler маппит sentinel-ошибки в RFC 7807.
// RBAC-проверка — в middleware (см. api/router.go), здесь её нет.
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
)

// ServiceRefsLister — поверхность listing-а git-ref-ов для одного Service-а.
// name резолвится в gitURL ВНЕ handler-а (кешер серверной стороны принимает
// решение, нужен ли реальный ls-remote или хватит кешированной записи). При
// nil соответствующий /refs-эндпоинт отвечает 500 «not configured» (паттерн
// ServiceLoader/PushRun: фича опциональна, до wire-up отдаёт 5xx).
type ServiceRefsLister interface {
	ListRefs(ctx context.Context, name, gitURL string) ([]artifact.GitRef, error)
}

// ServiceScenarioLister — поверхность listing-а scenario из материализованного
// снапшота Service-репо для одного `(name, ref)`. Симметрично [ServiceRefsLister]:
// handler принимает минимальную зависимость, реальный git-clone + парсинг
// scenario/*/main.yml — внутри реализации (TTL-кеш + ServiceLoader). При nil
// `GET /v1/services/{name}/scenarios` отвечает 500 «not configured».
type ServiceScenarioLister interface {
	ListScenarios(ctx context.Context, name, gitURL, ref string) ([]artifact.Scenario, error)
}

// ServiceStateSchemaLister — поверхность listing-а state-schema-метаданных
// (`state_schema_version` + опц. декларация структуры state + цепочка
// миграций) из материализованного снапшота Service-репо для `(name, ref)`.
// Симметрично [ServiceScenarioLister]: handler принимает минимальную
// зависимость, реальный git-clone + парсинг service.yml + scan migrations/ —
// внутри реализации (TTL-кеш + ServiceLoader). При nil
// `GET /v1/services/{name}/state-schema` отвечает 500 «not configured».
type ServiceStateSchemaLister interface {
	ListStateSchema(ctx context.Context, name, gitURL, ref string) (*artifact.StateSchemaInfo, error)
}

// ServiceDependenciesLister — поверхность listing-а git-зависимостей
// (`destiny:`/`modules:` из `service.yml`) одного снапшота Service-репо для
// `(name, ref)`. Симметрично [ServiceStateSchemaLister]: handler принимает
// минимальную зависимость, реальный git-clone + парсинг service.yml — внутри
// реализации (TTL-кеш + ServiceLoader). При nil
// `GET /v1/services/{name}/dependencies` отвечает 500 «not configured».
type ServiceDependenciesLister interface {
	ListDependencies(ctx context.Context, name, gitURL, ref string) (*artifact.ServiceDependencies, error)
}

// ServiceDirectivesLister — поверхность чтения ПОЛНОГО каталога директив (все
// серии) сервиса + SHA1 снапшота из материализованного снапшота Service-репо для
// `(name, ref)`. Симметрично [ServiceScenarioLister]: handler принимает
// минимальную зависимость, реальный git-clone + чтение essence/_default.yaml —
// внутри реализации (TTL-кеш + ServiceLoader). Version-сужение делает handler над
// результатом (кеш version-agnostic). При nil
// `GET /v1/services/{name}/directives` отвечает 500 «not configured».
type ServiceDirectivesLister interface {
	ListDirectives(ctx context.Context, name, gitURL, ref string) (*artifact.DirectiveCatalog, error)
}

// ServiceHandler — endpoint-ы реестра Service-ов (register / list / get /
// update / deregister / list-refs / list-scenarios / list-state-schema).
// Делегирует бизнес-логику в [serviceregistry.Service]; для /refs / /scenarios /
// /state-schema — отдельные lister-ы с TTL-кешами на стороне serviceregistry
// (не CRUD-логика реестра).
//
// Все зависимости immutable; safe for concurrent use — состояние между
// запросами не держит.
type ServiceHandler struct {
	svc          *serviceregistry.Service
	refs         ServiceRefsLister
	scenarios    ServiceScenarioLister
	stateSchema  ServiceStateSchemaLister
	dependencies ServiceDependenciesLister
	directives   ServiceDirectivesLister
	logger       *slog.Logger
}

// NewServiceHandler создаёт handler. svc обязателен (паника при nil —
// единственная точка misconfiguration, caller обязан передать non-nil). refs /
// scenarios / stateSchema / dependencies / directives опциональны: при nil
// соответствующий эндпоинт отвечает 500 (фича не сконфигурирована).
func NewServiceHandler(svc *serviceregistry.Service, refs ServiceRefsLister, scenarios ServiceScenarioLister, stateSchema ServiceStateSchemaLister, dependencies ServiceDependenciesLister, directives ServiceDirectivesLister, logger *slog.Logger) *ServiceHandler {
	if svc == nil {
		panic("handlers.NewServiceHandler: serviceregistry.Service is nil")
	}
	if logger == nil {
		logger = slog.New(slog.NewJSONHandler(io.Discard, nil))
	}
	return &ServiceHandler{svc: svc, refs: refs, scenarios: scenarios, stateSchema: stateSchema, dependencies: dependencies, directives: directives, logger: logger}
}

// ServiceSpecStub — непустой *ServiceHandler-заглушка для генерации huma-OpenAPI-
// фрагмента (HumaServiceSpecYAML): при dump доменный handler не вызывается, но
// huma.Register требует non-nil для no-op-проверки на nil. svc/lister-ы nil —
// handler никогда не исполняется в spec-режиме (parity [RoleSpecStub]).
func ServiceSpecStub() *ServiceHandler {
	return &ServiceHandler{logger: slog.New(slog.NewJSONHandler(io.Discard, nil))}
}

// ServiceRegisterInput — NATIVE request-форма POST /v1/services (handler-native
// T5d). name+git+ref обязательны, refresh опц. (`*string`). Заменяет
// ServiceRegisterRequest.
type ServiceRegisterInput struct {
	Name    string
	Git     string
	Ref     string
	Refresh *string
}

// ServiceUpdateInput — NATIVE request-форма PATCH /v1/services/{name} (handler-
// native T5d). name — path-параметр (ключ, не в теле); git+ref обязательны
// (replace-семантика mutable-полей), refresh опц.
type ServiceUpdateInput struct {
	Git     string
	Ref     string
	Refresh *string
}

// ServiceView — ПЛОСКАЯ доменная проекция записи реестра Service-а (POST 201 /
// GET / PATCH 200 / list-element), handler-native T5d. created_by_aid/refresh/
// updated_by_aid — `*string` (nil → ключ опущен в native-проекции); даты усечены
// до секунд (UTC). Пакет api проецирует в native-схему ServiceView.
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

// ServiceListPage — доменный список Service-ов GET /v1/services (handler-native T5d).
type ServiceListPage struct {
	Items []ServiceView
}

// ServiceRegisterReply — результат [ServiceHandler.RegisterTyped] (handler-native
// T5d). Несёт 201-тело (плоская ServiceView) + audit-поля (имя/git/ref + caller AID;
// git-URL не секрет).
type ServiceRegisterReply struct {
	Body         ServiceView
	Name         string
	Git          string
	Ref          string
	CreatedByAID string
}

// AuditPayload собирает audit-payload register-роута (parity легаси SetAuditPayload).
// ЕДИНЫЙ источник для (w,r)-оболочки И huma-варианта B.
func (r ServiceRegisterReply) AuditPayload() middleware.AuditPayload {
	return middleware.AuditPayload{
		"name":           r.Name,
		"git":            r.Git,
		"ref":            r.Ref,
		"created_by_aid": r.CreatedByAID,
	}
}

// RegisterTyped — извлечённая доменная функция POST /v1/services (FULL-TYPED
// разворот ADR-054 §Pattern (б)): бизнес-логика без http.ResponseWriter/*http.
// Request. claims/req приходят аргументами; ошибки — *problemError (через
// mapServiceError), успех — [ServiceRegisterReply] (201-тело + audit-поля).
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

// ListTyped — доменная функция GET /v1/services (handler-native T5d, READ без audit):
// читает реестр (sort name ASC) и собирает [ServiceListPage] (плоские ServiceView)
// без http.ResponseWriter/*http.Request. Ошибка чтения → *problemError (500).
// Wire-форму items строит native-проекция в api.
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

// GetTyped — доменная функция GET /v1/services/{name} (handler-native T5d, READ без
// audit): читает одну запись по имени без http.ResponseWriter/*http.Request. name
// приходит аргументом; ошибки — *problemError (404 not-found / 500), успех — плоская
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

// ServiceUpdateReply — результат [ServiceHandler.UpdateTyped] (handler-native T5d).
// Несёт 200-тело (плоская ServiceView) + audit-поля (имя/git/ref).
type ServiceUpdateReply struct {
	Body ServiceView
	Name string
	Git  string
	Ref  string
}

// AuditPayload собирает audit-payload update-роута (parity легаси SetAuditPayload).
func (r ServiceUpdateReply) AuditPayload() middleware.AuditPayload {
	return middleware.AuditPayload{
		"name": r.Name,
		"git":  r.Git,
		"ref":  r.Ref,
	}
}

// UpdateTyped — извлечённая доменная функция PATCH /v1/services/{name} (FULL-TYPED
// разворот ADR-054 §Pattern (б)): replace mutable-полей git/ref/refresh +
// invalidate-хуки кешей, без http.ResponseWriter/*http.Request. claims/name/req
// приходят аргументами; ошибки — *problemError (через mapServiceError), успех —
// [ServiceUpdateReply] (200-тело + audit-поля).
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

	return ServiceUpdateReply{
		Body: toServiceResponse(entry),
		Name: entry.Name,
		Git:  entry.Git,
		Ref:  entry.Ref,
	}, nil
}

// ServiceNameReply — результат write-операций, чей audit-payload несёт лишь имя
// Service-а (deregister). 204-тело пустое; reply — МЕТАДАННЫЕ для audit.
type ServiceNameReply struct {
	Name string
}

// DeregisterTyped — извлечённая доменная функция DELETE /v1/services/{name}
// (FULL-TYPED разворот ADR-054 §Pattern (б)): удаление по PK + invalidate-хуки
// кешей, без http.ResponseWriter/*http.Request. name приходит аргументом; ошибки —
// *problemError (404 not-found / 500), успех — [ServiceNameReply] (audit-payload).
// 204-тело пустое.
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
	return ServiceNameReply{Name: name}, nil
}

// invalidateRefs — best-effort выкидывание записи кеша refs для name после
// Update/Deregister. Если lister не поддерживает Invalidate (минимальный
// ServiceRefsLister — это интерфейс с одним методом) или сам lister=nil — no-op.
//
// Семантика «refs нового git-источника подтянутся при следующем запросе» важна
// для UX: после смены git-URL Service-а первое открытие Upgrade-modal должно
// показывать tags нового репо, а не закешированные старого.
func (h *ServiceHandler) invalidateRefs(name string) {
	if h.refs == nil {
		return
	}
	if inv, ok := h.refs.(interface{ Invalidate(string) }); ok {
		inv.Invalidate(name)
	}
}

// invalidateScenarios — best-effort инвалидация scenarios-кеша по name после
// Update/Deregister Service-а (парная семантика с [invalidateRefs]). После
// смены git-URL или удаления записи закешированные scenario должны исчезнуть,
// чтобы UI dropdown «Choose scenario» подтянул listing нового источника.
func (h *ServiceHandler) invalidateScenarios(name string) {
	if h.scenarios == nil {
		return
	}
	if inv, ok := h.scenarios.(interface{ Invalidate(string) }); ok {
		inv.Invalidate(name)
	}
}

// invalidateStateSchema — best-effort инвалидация state-schema-кеша по name
// (парная семантика с [invalidateScenarios]). После смены git-URL или
// удаления записи закешированная state-schema должна исчезнуть, чтобы UI
// Schema explorer подтянул listing нового источника.
func (h *ServiceHandler) invalidateStateSchema(name string) {
	if h.stateSchema == nil {
		return
	}
	if inv, ok := h.stateSchema.(interface{ Invalidate(string) }); ok {
		inv.Invalidate(name)
	}
}

// invalidateDependencies — best-effort инвалидация dependencies-кеша по name
// (парная семантика с [invalidateStateSchema]). После смены git-URL или
// удаления записи закешированные зависимости должны исчезнуть, чтобы UI
// Service Detail подтянул listing нового источника.
func (h *ServiceHandler) invalidateDependencies(name string) {
	if h.dependencies == nil {
		return
	}
	if inv, ok := h.dependencies.(interface{ Invalidate(string) }); ok {
		inv.Invalidate(name)
	}
}

// invalidateDirectives — best-effort инвалидация directives-кеши по name (парная
// семантика с [invalidateDependencies]). После смены git-URL или удаления записи
// закешированный каталог должен исчезнуть, чтобы UI redis_settings-редактор
// подтянул каталог нового источника.
func (h *ServiceHandler) invalidateDirectives(name string) {
	if h.directives == nil {
		return
	}
	if inv, ok := h.directives.(interface{ Invalidate(string) }); ok {
		inv.Invalidate(name)
	}
}

// mapServiceError маппит sentinel-ошибки [serviceregistry.Service]
// (register/update — единый набор валидации + UNIQUE/FK-границы) в *problemError
// (FULL-TYPED разворот ADR-054 §Pattern: доставляется huma-обёрткой через
// [AsProblemDetails] либо (w,r)-оболочкой через [writeProblemError]).
// Соответствие sentinel ↔ problem-type:
//   - ErrAlreadyExists      → service-already-exists (409).
//   - ErrNotFound           → not-found (404; update несуществующей записи).
//   - ErrOperatorNotFound   → not-found (404; CallerAID отсутствует в operators).
//   - ErrInvalidName / ErrInvalidGit / ErrInvalidRef / ErrInvalidRefresh →
//     validation-failed (422).
//
// Для unknown-ошибок — internal-error (500) + generic-detail (raw err.Error()
// не пробрасывается клиенту; диагностика — в логах).
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

// GitRefView — ПЛОСКАЯ доменная запись git-ref (element ServiceRefsList.Refs),
// handler-native T5d. IsDefault — bool (native-проекция в api опускает false как
// nil-указатель). Форма из [artifact.GitRef].
type GitRefView struct {
	Name      string
	Type      string
	Commit    string
	IsDefault bool
}

// ServiceRefsList — ПЛОСКОЕ доменное тело GET /v1/services/{name}/refs (handler-
// native T5d): service + refs[]. Пакет api проецирует в native ServiceRefsListReply.
type ServiceRefsList struct {
	Service string
	Refs    []GitRefView
}

// ListRefsTyped — доменная функция GET /v1/services/{name}/refs (handler-native T5d,
// READ без audit): резолв записи + ls-remote git-tag-ов/branch-ей, без http.
// ResponseWriter/*http.Request. name приходит аргументом; ошибки — *problemError
// (500 нет lister-а/сбой чтения, 404 not-found, 502 ls-remote упал), успех —
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

// ServiceScenariosReply — GET /v1/services/{name}/scenarios body. service +
// ref-поля дублируют path-параметр / выбранный ref для удобства клиента (один
// объект — самодостаточный JSON; UI ставит ref-метку рядом с dropdown).
//
// НЕ алиас на ServiceScenariosListReply: его элемент [Scenario] несёт
// типизированное enum-поле Kind (ScenarioKind), а домен [artifact.Scenario].Kind —
// plain string. Тесты сравнивают s.Kind с string-литералом — типизированный enum
// сломал бы это сравнение на компиляции. Wire-форма идентична (json-теги те же).
// См. отчёт S0: «typed enum в response-элементах» — частый случай тиража.
// Экспортирован (а не package-private), чтобы huma-обёртка [registerHumaServiceScenarios]
// в пакете api могла назвать его типом Body (FULL-TYPED ADR-054).
type ServiceScenariosReply struct {
	Service   string              `json:"service"`
	Ref       string              `json:"ref"`
	Scenarios []artifact.Scenario `json:"scenarios"`
}

// ListScenarios — GET /v1/services/{name}/scenarios. Возвращает список
// scenario-метаданных из материализованного снапшота git-репо Service-а (для
// UI dropdown «Choose scenario» в Run-modal — парный /refs для Upgrade-modal).
// Permission — service.list (та же проекция Service-записи, что и /refs).
//
// Query-параметр `ref` опционален: если не задан, берётся [ServiceEntry.Ref]
// (текущая версия из реестра). Сортировка scenarios — alphabetical по имени;
// невалидные/пустые scenario пропускаются (warning лог, partial-success).
// Read-only, без audit.
//
// Контракт:
//   - 200 + {service, ref, scenarios:[…]}.
//   - 404 (not-found) — записи с таким name нет в реестре.
//   - 500 — внутренний сбой (нет lister-а / неожиданная ошибка чтения реестра).
//   - 502 (bad-gateway) — git-clone / parse manifest упал на стороне loader-а.
//
// ListScenariosTyped — извлечённая доменная функция GET /v1/services/{name}/
// scenarios (FULL-TYPED разворот ADR-054 §Pattern, READ-вариант без audit): резолв
// записи + listing scenario из снапшота git-репо + разметка kind/runnable, без
// http.ResponseWriter/*http.Request. name/ref приходят аргументами (ref="" →
// дефолт из реестра); ошибки — *problemError (500/404/502), успех —
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

	// `?ref=<git-ref>` — опциональный override; по умолчанию — ref из реестра.
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
	// Разметка kind + runnable по канону scenario-пакета (единственный источник
	// правды): artifact-loader поля не заполняет — направление импорта
	// artifact←scenario, а не наоборот.
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

// StateSchemaMigration — ПЛОСКИЙ доменный шаг цепочки миграций (handler-native T5d).
// Форма из [artifact.Migration] (from/to int + path).
type StateSchemaMigration struct {
	From int
	To   int
	Path string
}

// ServiceStateSchema — ПЛОСКОЕ доменное тело GET /v1/services/{name}/state-schema
// (handler-native T5d). Schema — `map[string]any` (native-проекция опускает пустую
// карту); Migrations — []StateSchemaMigration. Пакет api проецирует в native схему.
type ServiceStateSchema struct {
	Service            string
	Ref                string
	StateSchemaVersion int
	Schema             map[string]any
	Migrations         []StateSchemaMigration
}

// ListStateSchema — GET /v1/services/{name}/state-schema. Возвращает
// state_schema-метаданные сервиса для UI Schema explorer-а: текущая версия
// (`state_schema_version`), опциональная декларация структуры state (если
// сервис её задекларировал в `service.yml::state_schema`) и плоский список
// миграций `<NNN>_to_<MMM>.yml` (metadata-only, без content).
// Permission — service.list (та же проекция Service-записи, что и /refs /
// /scenarios).
//
// Query-параметр `ref` опционален: если не задан, берётся [ServiceEntry.Ref]
// (текущая версия из реестра). Read-only, без audit. Кеш TTL 60s на стороне
// [ServiceStateSchemaLister].
//
// Контракт:
//   - 200 + {service, ref, state_schema_version, schema?, migrations:[…]}.
//   - 404 (not-found) — записи с таким name нет в реестре.
//   - 500 — внутренний сбой (нет lister-а / неожиданная ошибка чтения реестра).
//   - 502 (bad-gateway) — git-clone / parse manifest / scan migrations упал
//     на стороне loader-а.
//
// ListStateSchemaTyped — извлечённая доменная функция GET /v1/services/{name}/
// state-schema (FULL-TYPED разворот ADR-054 §Pattern, READ-вариант без audit):
// резолв записи + listing state-schema-метаданных, без http.ResponseWriter/*http.
// Request. name/ref приходят аргументами (ref="" → дефолт из реестра); ошибки —
// *problemError (500/404/502), успех — [ServiceStateSchema].
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
		// Defensive: lister обязан вернуть non-nil при err=nil; иначе отдаём
		// 502 — расхождение реализации с контрактом.
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

// ServiceDependency — ПЛОСКАЯ доменная запись destiny[]/modules[] (handler-native
// T5d). Git — string (native-проекция опускает пустую как nil-указатель). Форма из
// [artifact.Dependency].
type ServiceDependency struct {
	Name string
	Ref  string
	Git  string
}

// ServiceDependenciesList — ПЛОСКОЕ доменное тело GET /v1/services/{name}/dependencies
// (handler-native T5d): service/ref + destiny[]/modules[]. Пакет api проецирует в
// native ServiceDependenciesReply.
type ServiceDependenciesList struct {
	Service string
	Ref     string
	Destiny []ServiceDependency
	Modules []ServiceDependency
}

// ListDependencies — GET /v1/services/{name}/dependencies. Возвращает
// git-зависимости сервиса для UI Service Detail: задекларированные в
// `service.yml` destiny-кирпичики и custom-модули, каждый со своим git-ref-ом
// (ADR-007: версия = git tag/branch). Permission — service.list (та же
// проекция Service-записи, что и /refs / /scenarios / /state-schema).
//
// Query-параметр `ref` опционален: если не задан, берётся [ServiceEntry.Ref]
// (текущая версия из реестра). Read-only, без audit. Кеш TTL 60s на стороне
// [ServiceDependenciesLister].
//
// Контракт:
//   - 200 + {service, ref, destiny:[…], modules:[…]}.
//   - 404 (not-found) — записи с таким name нет в реестре.
//   - 500 — внутренний сбой (нет lister-а / неожиданная ошибка чтения реестра).
//   - 502 (bad-gateway) — git-clone / parse manifest упал на стороне loader-а.
//
// ListDependenciesTyped — извлечённая доменная функция GET /v1/services/{name}/
// dependencies (FULL-TYPED разворот ADR-054 §Pattern, READ-вариант без audit):
// резолв записи + listing git-зависимостей, без http.ResponseWriter/*http.Request.
// name/ref приходят аргументами (ref="" → дефолт из реестра); ошибки —
// *problemError (500/404/502), успех — [ServiceDependenciesList].
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
		// Defensive: lister обязан вернуть non-nil при err=nil; иначе отдаём
		// 502 — расхождение реализации с контрактом (паттерн ListStateSchema).
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

// ServiceDirectivesReply — GET /v1/services/{name}/directives body. Самодостаточный
// JSON (как ServiceScenariosReply): service + ref эхо-дубли, sha1 снапшота (== ETag),
// directives = карта `серия(major.minor) → отсортированные имена`. Сервис без каталога
// → directives:{} (не null). Body напрямую (не native-DTO): элементы — примитивные
// строки, huma-схема тривиальна. json-теги фиксируют wire; huma-регистратор читает
// SHA1 для ETag/If-None-Match (см. huma_service.go).
type ServiceDirectivesReply struct {
	Service    string              `json:"service"`
	Ref        string              `json:"ref"`
	SHA1       string              `json:"sha1"`
	Directives map[string][]string `json:"directives"`
}

// ListDirectivesTyped — GET /v1/services/{name}/directives (READ без audit): резолв
// записи + чтение ПОЛНОГО каталога директив из снапшота + version-сужение (опц.).
// name/ref/version приходят аргументами (ref="" → дефолт из реестра; version="" →
// весь каталог). Ошибки — *problemError (500 нет lister-а/сбой реестра, 404 not-found,
// 502 loader упал), успех — [ServiceDirectivesReply] с непустой (возможно пустой)
// картой directives.
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
		// Defensive: lister обязан вернуть non-nil при err=nil (паттерн ListStateSchema).
		h.logger.Error("service.directives: loader returned nil without error",
			slog.String("name", name))
		return zero, &problemError{problem.New(problem.TypeBadGateway, "", "directives loader returned empty result")}
	}

	// Version-сужение — над полным (кешированным) каталогом; пустой каталог →
	// непустой {} (не null) для мягкой деградации фронта.
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

// --- domain → ПЛОСКАЯ-доменная проекция списочных тел (handler-native T5d) ---
//
// Сервис-слой отдаёт доменные [artifact.*]; handler собирает плоские доменные
// view-типы, native wire-форму (omitempty/[]-vs-null/enum) строит native-проекция
// в api. Все возвращают non-nil срез (пустой → `[]`, не null — прежний контракт).

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

// toServiceResponse проецирует [serviceregistry.ServiceEntry] в ПЛОСКУЮ доменную
// [ServiceView] (handler-native T5d). Даты — UTC, обрезаны до секунд: native
// ServiceView несёт time.Time (date-time wire); Truncate(Second) сохраняет прежнюю
// wire-форму (без наносекунд).
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
