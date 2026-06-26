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
	// Вариант A; паритет REST `create_scenario`). Пусто → default `create`;
	// непустое имя обязано входить в create-набор сервиса, иначе validation-failed.
	CreateScenario string `json:"create_scenario,omitempty"`
}

// incarnationCreateOutput — output keeper.incarnation.create
// (schemaApplyIDOutputWithIncarnation): apply_id + echo incarnation.
// Симметричен REST createIncarnationResponse. ApplyID — `*string` (omitempty):
// отсутствует при lifecycle.auto_create=false (инкарнация создана ready без
// прогона), как nullable apply_id в REST IncarnationCreateReply.
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

	// Sync-валидация input против scenario `create` `input:`-схемы — ДО insert-а
	// (паритет REST Create: закрытие дыры с required-полями, проверявшимися лишь
	// async). nil loader → деградация без проверки (как в REST). Невалидный input
	// → validation-failed; сбой загрузки снапшота → internal-error.
	//
	// autoCreate — политика lifecycle.auto_create манифеста (default true). false
	// → инкарнация создаётся ready без прогона (apply_id в ответе отсутствует),
	// оператор запускает `create` вручную (паритет REST Create).
	// createScenario — фактический стартовый сценарий (механизм нескольких create).
	// Дефолт `create` (back-compat); при наличии loader-а валидируется выбор оператора
	// на членство в create-наборе сервиса. Сохраняется в incarnation.created_scenario;
	// rerun-create перезапускает именно его.
	createScenario := scenario.CreateScenarioName
	autoCreate := true
	if h.deps.ServiceLoader != nil {
		chosen, cerr := scenario.ValidateCreateScenarioChoice(ctx, h.deps.ServiceLoader, serviceRef, a.CreateScenario)
		if cerr != nil {
			if errors.Is(cerr, scenario.ErrCreateScenarioNotEligible) {
				return h.toolError(req.ID, toolName, mcpCodeValidationFailed,
					"create_scenario_invalid: "+cerr.Error())
			}
			h.deps.Logger.Error("mcp: incarnation.create resolve create scenario failed",
				slog.String("name", a.Name),
				slog.String("service", a.Service),
				slog.Any("error", cerr),
			)
			return h.toolError(req.ID, toolName, mcpCodeInternalError,
				"resolve create scenario failed")
		}
		createScenario = chosen

		if err := scenario.ValidateInput(ctx, h.deps.ServiceLoader, serviceRef, createScenario, a.Input); err != nil {
			if errors.Is(err, scenario.ErrInputInvalid) {
				return h.toolError(req.ID, toolName, mcpCodeValidationFailed,
					"input_invalid: "+err.Error())
			}
			h.deps.Logger.Error("mcp: incarnation.create input validation failed",
				slog.String("name", a.Name),
				slog.String("service", a.Service),
				slog.Any("error", err),
			)
			return h.toolError(req.ID, toolName, mcpCodeInternalError,
				"validate scenario create input failed")
		}

		// lifecycle.auto_create из снапшота (тот же ref, loader кеширует слот).
		art, err := h.deps.ServiceLoader.Load(ctx, serviceRef)
		if err != nil {
			h.deps.Logger.Error("mcp: incarnation.create load snapshot for lifecycle failed",
				slog.String("name", a.Name),
				slog.String("service", a.Service),
				slog.Any("error", err),
			)
			return h.toolError(req.ID, toolName, mcpCodeInternalError,
				"load service snapshot failed")
		}
		if art != nil && art.Manifest != nil {
			autoCreate = art.Manifest.Lifecycle.AutoCreateEnabled()
		}
	}

	// spec.input пишем только при непустом input — иначе scenario-runner
	// увидел бы `"input": null` (присутствующий ключ) вместо «оператор не
	// передал input», что неотличимо для CEL (паритет REST).
	spec := map[string]any{}
	if a.Input != nil {
		spec["input"] = a.Input
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
		CreatedScenario:    createScenario,
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

	// apply_id генерируется только при запуске scenario `create`
	// (auto_create=true). auto_create=false → инкарнация остаётся ready без
	// прогона, apply_id пуст (в ответе — omitted).
	var applyID string
	if autoCreate {
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
