package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"

	"github.com/souls-guild/soul-stack/keeper/internal/incarnation"
	"github.com/souls-guild/soul-stack/keeper/internal/jwt"
	"github.com/souls-guild/soul-stack/keeper/internal/scenario"
	"github.com/souls-guild/soul-stack/keeper/internal/soul"
	"github.com/souls-guild/soul-stack/shared/audit"
)

// incarnationCreateArgs — arguments tool-а keeper.incarnation.create
// (schemaIncarnationCreateInput): name + service обязательны, covens / input
// опциональны. covens — declared env-Coven-метки (передаются в incarnation,
// влияют на RBAC-scope create); input — параметры scenario `create`.
type incarnationCreateArgs struct {
	Name    string         `json:"name"`
	Service string         `json:"service"`
	Covens  []string       `json:"covens,omitempty"`
	Input   map[string]any `json:"input,omitempty"`
	// CreateScenario — выбор стартового сценария (механизм нескольких create,
	// Вариант A; паритет REST `create_scenario`). Контракт пустого выбора (Фаза 2):
	// сервис предлагает create-сценарии + пусто → create_scenario_required; сервис
	// без них + пусто → bare-инкарнация (ready без прогона). Непустое имя обязано
	// входить в create-набор, иначе validation-failed.
	CreateScenario string `json:"create_scenario,omitempty"`
	// Traits — operator-set trait-метки инкарнации (ADR-060 amend R1, паритет
	// REST CreateTyped поля `traits`): ключ → scalar|list of scalars. Извлекаются
	// в колонку incarnation.traits (источник истины) и проецируются в souls.traits
	// хостов-членов sync-hook-ом после insert.
	Traits map[string]any `json:"traits,omitempty"`
}

// assertPreflighter — локальная узкая поверхность scenario.Runner для
// pre-flight-гейта `assert:` (ADR-009/ADR-027 amendment 2026-06-23, форма A),
// duck-typing над h.deps.ScenarioRunner. Объявлена ЛОКАЛЬНО (не импортируется из
// handlers): mcp не зависит от REST-handler-слоя, а *scenario.Runner так же
// удовлетворяет ей. ScenarioStarter-фейки без PreflightAssert → type-assertion
// не проходит, гейт no-op (как в REST).
type assertPreflighter interface {
	PreflightAssert(ctx context.Context, spec scenario.RunSpec) error
}

// incarnationCreateOutput — output keeper.incarnation.create
// (schemaApplyIDOutputWithIncarnation): apply_id + echo incarnation. ApplyID —
// `*string` (omitempty): отсутствует при lifecycle.auto_create=false (инкарнация
// создана ready без прогона), как nullable apply_id в REST IncarnationCreateReply.
type incarnationCreateOutput struct {
	ApplyID     *string `json:"_apply_id,omitempty"`
	Incarnation string  `json:"incarnation"`
}

// callIncarnationCreate — mutating async-tool keeper.incarnation.create.
// Паритет REST IncarnationHandler.Create в production-режиме (runner+services
// обязательны): резолв git-координат service → insert row (status=ready) →
// async-запуск scenario `create` → 202 + apply_id.
//
// MCP не имеет stub-режима Create (REST деградирует до insert-only при
// runner==nil — это M0.6c-1-совместимость; MCP-tools тиражируются поверх
// готового runner-стека): ScenarioRunner / ServiceRegistry nil → internal-error
// «scenario runner is not configured» (симметрично REST Run/Upgrade без deps).
//
// RBAC — body-scoped OR-Check (паритет REST IncarnationCreateScopeSelector +
// RequirePermissionMulti): scope = covens ∪ {name} (declared env-теги + имя как
// корневая Coven-метка, ADR-008). Без этого coven-scoped оператор обходил бы
// REST-защиту через MCP (least-privilege: scope `coven=dev` НЕ создаёт
// incarnation с covens=[prod]). bare/`*` — матчит любую (как раньше).
// audit: EventIncarnationCreated {name, service, covens, apply_id}, source=mcp.
func (h *Handler) callIncarnationCreate(ctx context.Context, claims *jwt.Claims, req jsonRPCRequest, args json.RawMessage) jsonRPCResponse {
	const toolName = "keeper.incarnation.create"

	var a incarnationCreateArgs
	if len(args) > 0 {
		if err := strictUnmarshal(args, &a); err != nil {
			return h.toolError(req.ID, toolName, mcpCodeMalformedRequest,
				"invalid arguments: "+err.Error())
		}
	}
	if a.Name == "" {
		return h.toolError(req.ID, toolName, mcpCodeValidationFailed, "field 'name' is required")
	}
	if !incarnation.ValidName(a.Name) {
		return h.toolError(req.ID, toolName, mcpCodeValidationFailed,
			"field 'name' must match "+incarnation.NamePattern)
	}
	if a.Service == "" {
		return h.toolError(req.ID, toolName, mcpCodeValidationFailed, "field 'service' is required")
	}
	// Sanity-check имени сервиса по той же kebab-case-грамматике (паритет REST):
	// защита от мусора в БД (символ `/` сломал бы git-resolve пути).
	if !incarnation.ValidName(a.Service) {
		return h.toolError(req.ID, toolName, mcpCodeValidationFailed,
			"field 'service' must match "+incarnation.NamePattern)
	}
	// covens — declared env-теги (ADR-008 amendment a): формат каждого по
	// CovenPattern (симметрично soul.Create / REST create). Невалидная метка
	// → validation-failed до scope-check/insert.
	for _, label := range a.Covens {
		if !soul.ValidCoven(label) {
			return h.toolError(req.ID, toolName, mcpCodeValidationFailed,
				"coven label "+label+" must match "+soul.CovenPattern)
		}
	}

	// Body-scoped RBAC ДО создания (fail-closed): deny → ни audit, ни insert,
	// ни scenario-start. Контексты — covens ∪ {name}, тем же
	// handlers.IncarnationCovenContexts, что REST (single source of truth).
	if err := h.checkIncarnationScope(claims, "create", a.Name, a.Service, a.Covens); err != nil {
		return h.toolError(req.ID, toolName, mcpCodeForbidden,
			"operator lacks required permission incarnation.create")
	}

	// runner / services обязательны для запуска scenario `create`.
	if h.deps.ScenarioRunner == nil || h.deps.ServiceRegistry == nil {
		return h.toolError(req.ID, toolName, mcpCodeInternalError,
			"scenario runner is not configured")
	}

	serviceRef, ok := h.deps.ServiceRegistry.Resolve(a.Service)
	if !ok {
		return h.toolError(req.ID, toolName, mcpCodeValidationFailed,
			"service "+a.Service+" is not registered (manage via service.* API, ADR-029)")
	}

	// Общий резолв стартового сценария + sync-валидация input + pre-flight-assert
	// (R2: единый scenario.ResolveCreatePlan с REST CreateTyped). nil loader →
	// stub-план (`create`, не bare, auto_create=true; в проде MCP loader всегда есть).
	// preflighter — h.deps.ScenarioRunner (type-assertion на scenario.AssertPreflighter
	// внутри; ScenarioStarter-фейк без метода → no-op).
	//
	// createScenario — стартовый сценарий (механизм нескольких create); bareNoScenario
	// — сервис без `create: true` (ready без прогона, created_scenario=NULL); autoCreate
	// — политика lifecycle.auto_create (default true): false → ready без прогона, но
	// created_scenario непустое.
	plan, perr := scenario.ResolveCreatePlan(ctx, h.deps.ServiceLoader, h.deps.ScenarioRunner, a.Name, serviceRef, a.CreateScenario, a.Input, claims.Subject)
	if perr != nil {
		return h.createPlanToolError(req, toolName, a.Name, a.Service, perr)
	}
	createScenario := plan.CreateScenario
	bareNoScenario := plan.BareNoScenario
	autoCreate := plan.AutoCreate

	// spec.input пишем только при непустом input — иначе scenario-runner
	// увидел бы `"input": null` (присутствующий ключ) вместо «оператор не
	// передал input», что неотличимо для CEL (паритет REST).
	spec := map[string]any{}
	if a.Input != nil {
		spec["input"] = a.Input
	}
	if a.Traits != nil {
		spec["traits"] = a.Traits
	}

	// Trait per-incarnation (ADR-060 amend R1, паритет REST CreateTyped): operator-
	// set traits из spec.traits извлекаются в колонку incarnation.traits (источник
	// истины, проецируемый в souls.traits). Невалидный набор (формат ключа/значения)
	// → validation-failed ДО insert.
	traits, err := incarnation.TraitsFromSpec(spec)
	if err != nil {
		return h.toolError(req.ID, toolName, mcpCodeValidationFailed, err.Error())
	}

	// createScenarioCol — NULL для bare-инкарнации (нет bootstrap-сценария), иначе
	// указатель на выбранное имя (паритет REST). autoCreate=false НЕ делает NULL —
	// bootstrap-сценарий есть, прогон лишь отложен.
	var createScenarioCol *string
	if !bareNoScenario {
		createScenarioCol = &createScenario
	}

	creator := claims.Subject
	inc := &incarnation.Incarnation{
		Name:               a.Name,
		Service:            a.Service,
		ServiceVersion:     serviceRef.Ref,
		StateSchemaVersion: 1,
		Spec:               spec,
		State:              nil,
		Status:             incarnation.StatusReady,
		CreatedByAID:       &creator,
		Covens:             a.Covens,
		Traits:             traits,
		CreatedScenario:    createScenarioCol,
	}
	if err := incarnation.Create(ctx, h.deps.IncarnationDB, inc); err != nil {
		if errors.Is(err, incarnation.ErrIncarnationAlreadyExists) {
			return h.toolError(req.ID, toolName, mcpCodeIncarnationExists,
				"incarnation "+a.Name+" already exists")
		}
		h.deps.Logger.Error("mcp: incarnation.create insert failed",
			slog.String("name", a.Name),
			slog.String("service", a.Service),
			slog.String("by_aid", claims.Subject),
			slog.Any("error", err),
		)
		return h.toolError(req.ID, toolName, mcpCodeInternalError, "insert incarnation failed")
	}

	// Sync-hook (ADR-060 amend R1, паритет REST CreateTyped): incarnation.traits →
	// souls.traits хостов-членов. Гейтится непустыми traits (иначе проекция затёрла
	// бы per-soul traits в `{}`). На create обычно 0 членов (онбординг идёт в
	// scenario create) → no-op; bind-хук добьёт хосты позже. Best-effort (лог, не
	// валим create): инкарнация уже создана, sync до-сойдётся при следующем bind.
	if len(traits) > 0 {
		if serr := incarnation.SyncTraitsToHosts(ctx, h.deps.IncarnationDB, a.Name, traits); serr != nil {
			h.deps.Logger.Warn("mcp: incarnation.create sync traits → souls провален (best-effort)",
				slog.String("name", a.Name), slog.Any("error", serr))
		}
	}

	// apply_id генерируется только при запуске bootstrap-прогона (runCreate). bare
	// (нет create-сценария) ИЛИ auto_create=false → инкарнация остаётся ready без
	// прогона, apply_id пуст (в ответе — omitted). runScenario не нужен (в отличие
	// от REST incarnation_typed.go): nil-runner отбит на входе (строки ~117-120),
	// сюда MCP доходит только с живым ScenarioRunner.
	runCreate := !bareNoScenario && autoCreate
	var applyID string
	if runCreate {
		applyID = audit.NewULID()
		if err := h.deps.ScenarioRunner.Start(ctx, scenario.RunSpec{
			ApplyID:         applyID,
			IncarnationName: a.Name,
			ServiceRef:      serviceRef,
			ScenarioName:    createScenario,
			Input:           a.Input,
			StartedByAID:    claims.Subject,
		}); err != nil {
			// Row уже вставлен (status=ready), прогон не стартовал. Логируем;
			// internal-error, чтобы оператор повторил (паритет REST 500).
			h.deps.Logger.Error("mcp: incarnation.create scenario start failed",
				slog.String("name", a.Name),
				slog.String("apply_id", applyID),
				slog.Any("error", err),
			)
			return h.toolError(req.ID, toolName, mcpCodeInternalError, "start scenario create failed")
		}
	}

	// covens — coalesce nil→[] (паритет REST: audit-поле всегда массив,
	// не null). apply_id в audit — null при auto_create=false.
	auditCovens := a.Covens
	if auditCovens == nil {
		auditCovens = []string{}
	}
	var auditApplyID any
	out := incarnationCreateOutput{Incarnation: a.Name}
	if applyID != "" {
		auditApplyID = applyID
		out.ApplyID = &applyID
	}
	h.writeAudit(audit.EventIncarnationCreated, claims.Subject, map[string]any{
		"name":     a.Name,
		"service":  a.Service,
		"covens":   auditCovens,
		"apply_id": auditApplyID,
	})

	return h.toolResult(req.ID, out)
}

// createPlanToolError маппит ошибку [scenario.ResolveCreatePlan] в jsonRPCResponse
// keeper.incarnation.create (R2: общий резолв create-плана с REST CreateTyped, но
// MCP-маппинг свой). Сохраняет дословные detail-префиксы исходного inline-кода
// (create_scenario_required / create_scenario_invalid / input_invalid /
// validation_failed / assert_failed → все validation-failed; assert НЕ имеет
// отдельного MCP-кода — паритет прежнего поведения, в отличие от REST
// TypeAssertFailed); прочие (load/parse снапшота, eval-сбой) → internal-error с
// логом.
func (h *Handler) createPlanToolError(req jsonRPCRequest, toolName, name, service string, err error) jsonRPCResponse {
	switch {
	case errors.Is(err, scenario.ErrCreateScenarioRequired):
		return h.toolError(req.ID, toolName, mcpCodeValidationFailed, "create_scenario_required: "+err.Error())
	case errors.Is(err, scenario.ErrCreateScenarioNotEligible):
		return h.toolError(req.ID, toolName, mcpCodeValidationFailed, "create_scenario_invalid: "+err.Error())
	case errors.Is(err, scenario.ErrInputInvalid):
		return h.toolError(req.ID, toolName, mcpCodeValidationFailed, "input_invalid: "+err.Error())
	case errors.Is(err, scenario.ErrValidateFailed):
		return h.toolError(req.ID, toolName, mcpCodeValidationFailed, "validation_failed: "+err.Error())
	case errors.Is(err, scenario.ErrAssertFailed):
		return h.toolError(req.ID, toolName, mcpCodeValidationFailed, "assert_failed: "+err.Error())
	}
	h.deps.Logger.Error("mcp: incarnation.create resolve create plan failed",
		slog.String("name", name),
		slog.String("service", service),
		slog.Any("error", err),
	)
	return h.toolError(req.ID, toolName, mcpCodeInternalError, "resolve create plan failed")
}
