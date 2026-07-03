package handlers

// FULL-TYPED извлечения incarnation-домена (ADR-054 §Pattern, батч-2g). Каждая
// *Typed-функция несёт всю бизнес-логику соответствующего (w,r)-handler-а БЕЗ
// http.ResponseWriter/*http.Request: декод/auth — на huma-слое (api-пакет), ошибки —
// *problemError, успех — typed reply.
//
// Два класса audit (СВЕРЕНЫ по router.go + handler-коду, перепутать = S6-регрессия):
//
//   - MIDDLEWARE-AUDIT (create / run / unlock / upgrade): audit пишет huma-audit-
//     middleware (вариант B) СНАРУЖИ. *Typed возвращает reply, НЕСУЩИЙ audit-payload
//     (поле AuditPayload) — huma-register-func кладёт его через SetHumaAuditPayload.
//     Сами *Typed audit НЕ пишут.
//   - SELF-AUDIT (rerun-last / check-drift / destroy / update-hosts): audit пишет
//     САМ handler через h.auditW.Write ВНУТРИ *Typed (payload собирается только после
//     доменной операции — previous_status / drift_summary / old-new snapshot). audit-
//     middleware на этих роутах НЕ навешан.
//
// read (get / list / history) — audit НЕ пишут вообще.

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"
	"unicode/utf8"

	apimiddleware "github.com/souls-guild/soul-stack/keeper/internal/api/middleware"
	"github.com/souls-guild/soul-stack/keeper/internal/api/problem"
	"github.com/souls-guild/soul-stack/keeper/internal/applyrun"
	"github.com/souls-guild/soul-stack/keeper/internal/artifact"
	"github.com/souls-guild/soul-stack/keeper/internal/incarnation"
	"github.com/souls-guild/soul-stack/keeper/internal/jwt"
	"github.com/souls-guild/soul-stack/keeper/internal/scenario"
	"github.com/souls-guild/soul-stack/keeper/internal/soul"
	sharedapi "github.com/souls-guild/soul-stack/shared/api"
	"github.com/souls-guild/soul-stack/shared/audit"
)

// IncarnationSpecStub — непустой *IncarnationHandler-заглушка для генерации huma-
// OpenAPI-фрагмента (HumaIncarnationSpecYAML): при dump доменный handler не
// вызывается, но huma.Register требует non-nil. Все зависимости nil.
func IncarnationSpecStub() *IncarnationHandler { return &IncarnationHandler{} }

// --- Create (MIDDLEWARE-AUDIT incarnation.created) --------------------

// IncarnationCreateView — ПЛОСКАЯ доменная проекция 202-тела POST /v1/incarnations (handler-
// native). Пакет api проецирует её в native IncarnationCreateReply. ApplyID — pointer-optional
// (lifecycle.auto_create:false → инкарнация в ready без прогона, apply_id опущен).
type IncarnationCreateView struct {
	ApplyID     *string
	Incarnation string
}

// IncarnationCreateRequestInput — NATIVE request-форма POST /v1/incarnations (handler-native).
// Заменяет IncarnationCreateRequest: huma-input (пакет api) биндит тело по своим полям и
// зовёт CreateTyped с этой плоской моделью. Covens/Input — nil = «не задано» (parity legacy
// omitempty-декода).
type IncarnationCreateRequestInput struct {
	Name    string
	Service string
	Covens  []string
	Input   map[string]any
	// Traits — operator-set trait-метки инкарнации (ADR-060 amend R1): кладутся в
	// spec.traits → incarnation.traits (источник истины) + проекция в souls.traits
	// хостов-членов. nil = «не задано» (parity legacy omitempty-декода).
	Traits map[string]any
	// CreateScenario — выбор стартового сценария (механизм нескольких create,
	// Вариант A). Контракт пустого выбора (Фаза 2): сервис ПРЕДЛАГАЕТ create-
	// сценарии (≥1 `create: true`) + пусто → 422 create_scenario_required; сервис
	// БЕЗ create-сценариев + пусто → bare-инкарнация (StatusReady без прогона).
	// Непустое имя обязано входить в create-набор, иначе 422; выбор сохраняется в
	// incarnation.created_scenario (NULL для bare; rerun-last использует его на create-пути).
	CreateScenario string
}

// IncarnationCreateReply — typed reply CreateTyped. Body — плоская доменная проекция 202-тела;
// AuditPayload — данные для huma-audit-middleware (name/service/covens/apply_id).
type IncarnationCreateReply struct {
	Body         IncarnationCreateView
	AuditPayload apimiddleware.AuditPayload
}

// CreateTyped — извлечённая доменная функция POST /v1/incarnations (MIDDLEWARE-AUDIT).
// Parity (w,r)-Create: валидация name/service/covens → resolve service-ref + выбор
// create-сценария (Фаза 2: required при наличии / bare при отсутствии) + sync
// input-validation + lifecycle.auto_create → insert row → опц. запуск bootstrap →
// 202 + apply_id. Bare-инкарнация (сервис без create-сценариев) создаётся StatusReady
// БЕЗ прогона, без apply_id, created_scenario=NULL. audit НЕ пишет (middleware-
// вариант B): возвращает payload в reply.
func (h *IncarnationHandler) CreateTyped(ctx context.Context, claims *jwt.Claims, req IncarnationCreateRequestInput) (IncarnationCreateReply, error) {
	var zero IncarnationCreateReply

	covens := req.Covens
	input := req.Input
	if req.Name == "" {
		return zero, incProblem(problem.TypeValidationFailed, "field 'name' is required")
	}
	if !incarnation.ValidName(req.Name) {
		return zero, incProblem(problem.TypeValidationFailed, "field 'name' must match "+incarnation.NamePattern)
	}
	if req.Service == "" {
		return zero, incProblem(problem.TypeValidationFailed, "field 'service' is required")
	}
	if !incarnation.ValidName(req.Service) {
		return zero, incProblem(problem.TypeValidationFailed, "field 'service' must match "+incarnation.NamePattern)
	}
	for _, label := range covens {
		if !soul.ValidCoven(label) {
			return zero, incProblem(problem.TypeValidationFailed, "coven label "+label+" must match "+soul.CovenPattern)
		}
	}

	serviceVersion := "stub"
	var serviceRef artifact.ServiceRef
	runScenario := h.runner != nil && h.services != nil
	autoCreate := true
	// bareNoScenario — сервис не предлагает ни одного create-сценария (нет
	// `create: true`): инкарнация создаётся StatusReady БЕЗ прогона (готова к
	// day-2). created_scenario пишется NULL (см. createScenarioCol). Отлично от
	// autoCreate=false: там bootstrap-сценарий ЕСТЬ (chosen), но прогон отложен —
	// created_scenario непустое. Оба не запускают runner.Start → bare (ниже).
	bareNoScenario := false
	// createScenario — фактический стартовый сценарий (механизм нескольких create).
	// Дефолт CreateScenarioName (`create`) удерживает legacy-поведение БЕЗ loader-а
	// (stub-режим: набор не резолвится, bare не определить → запускаем `create` как
	// раньше). При наличии loader-а перезаписывается выбором оператора либо bare-
	// признаком. Используется во всех фазах bootstrap-прогона (через createScenario
	// в runner.Start); created_scenario пишется через createScenarioCol (NULL при
	// bareNoScenario).
	createScenario := scenario.CreateScenarioName
	if runScenario {
		ref, ok := h.services.Resolve(req.Service)
		if !ok {
			return zero, incProblem(problem.TypeValidationFailed,
				"service "+req.Service+" is not registered (manage via service.* API, ADR-029)")
		}
		serviceRef = ref
		serviceVersion = ref.Ref

		// Общий резолв стартового сценария + input-валидация + pre-flight-assert
		// (R2: единый scenario.ResolveCreatePlan с MCP callIncarnationCreate). h.loader
		// nil → stub-план (`create`, не bare, auto_create=true). preflighter — h.runner
		// (type-assertion на scenario.AssertPreflighter внутри; ScenarioStarter-фейк →
		// no-op).
		plan, perr := scenario.ResolveCreatePlan(ctx, h.loader, h.runner, req.Name, serviceRef, req.CreateScenario, input, claims.Subject)
		if perr != nil {
			return zero, h.mapCreatePlanError(req.Name, req.Service, perr)
		}
		createScenario = plan.CreateScenario
		bareNoScenario = plan.BareNoScenario
		autoCreate = plan.AutoCreate
	}

	spec := map[string]any{}
	if input != nil {
		spec["input"] = input
	}
	if req.Traits != nil {
		spec["traits"] = req.Traits
	}

	// Trait per-incarnation (ADR-060 amend, R1): operator-set traits живут в
	// incarnation.spec.traits (top-level API-поле `traits`, прокинуто в spec
	// выше). На create-пути извлекаем их в колонку incarnation.traits — она
	// источник истины, проецируемый в souls.traits хостов-членов sync-hook-ом
	// ниже. Невалидный набор (формат ключа/значения) → 422 ДО insert.
	traits, err := incarnation.TraitsFromSpec(spec)
	if err != nil {
		return zero, incProblem(problem.TypeValidationFailed, err.Error())
	}

	// createScenarioCol — значение колонки created_scenario: NULL для bare-
	// инкарнации (нет bootstrap-сценария), иначе указатель на выбранное имя.
	// autoCreate=false НЕ делает её NULL — bootstrap-сценарий есть (chosen),
	// просто прогон отложен; rerun-last использует его на create-пути.
	var createScenarioCol *string
	if !bareNoScenario {
		createScenarioCol = &createScenario
	}

	creator := claims.Subject
	inc := &incarnation.Incarnation{
		Name:               req.Name,
		Service:            req.Service,
		ServiceVersion:     serviceVersion,
		StateSchemaVersion: 1,
		Spec:               spec,
		State:              nil,
		Status:             incarnation.StatusReady,
		CreatedByAID:       &creator,
		Covens:             covens,
		Traits:             traits,
		CreatedScenario:    createScenarioCol,
	}
	if err := incarnation.Create(ctx, h.db, inc); err != nil {
		if errors.Is(err, incarnation.ErrIncarnationAlreadyExists) {
			return zero, incProblem(problem.TypeIncarnationExists, "incarnation "+req.Name+" already exists")
		}
		h.logger.Error("incarnation.create: insert failed",
			slog.String("name", req.Name), slog.String("service", req.Service),
			slog.String("by_aid", claims.Subject), slog.Any("error", err))
		return zero, incProblem(problem.TypeInternalError, "insert incarnation failed")
	}

	// Sync-hook (ADR-060 amend, R1): incarnation.traits → souls.traits хостов-
	// членов. Гейтится непустыми traits — иначе на каждом create без traits
	// проекция затирала бы per-soul traits в `{}` (footgun переходного периода).
	// На create обычно 0 членов (онбординг идёт в scenario create) → no-op;
	// bind-хук в keeper-dispatch добьёт хосты, привязавшиеся в прогоне. Не валим
	// create на сбое проекции (best-effort, лог): инкарнация уже создана, sync
	// до-сойдётся при следующем bind/create-повторе.
	if len(traits) > 0 {
		if serr := incarnation.SyncTraitsToHosts(ctx, h.db, req.Name, traits); serr != nil {
			h.logger.Warn("incarnation.create: sync traits → souls провален (best-effort)",
				slog.String("name", req.Name), slog.Any("error", serr))
		}
	}

	// bareReady — живой runner-стек, но прогон сознательно НЕ стартует: bare-
	// инкарнация (нет create-сценария) ИЛИ autoCreate=false (прогон отложен). Тогда
	// инкарнация остаётся StatusReady без прогона, apply_id опущен. В stub-режиме
	// (!runScenario, M0.6c-1 insert-only) apply_id выдаётся как прежде — это НЕ bare.
	bareReady := runScenario && (bareNoScenario || !autoCreate)
	// runCreate — стартует ли bootstrap-прогон (нужен живой стек, сценарий и
	// autoCreate). Отдельно от bareReady: stub-режим тоже не запускает Start.
	runCreate := runScenario && !bareNoScenario && autoCreate

	var applyID string
	if !bareReady {
		applyID = audit.NewULID()
	}

	if runCreate {
		if err := h.runner.Start(ctx, scenario.RunSpec{
			ApplyID:         applyID,
			IncarnationName: req.Name,
			ServiceRef:      serviceRef,
			ScenarioName:    createScenario,
			Input:           input,
			StartedByAID:    claims.Subject,
		}); err != nil {
			h.logger.Error("incarnation.create: scenario start failed",
				slog.String("name", req.Name), slog.String("apply_id", applyID), slog.Any("error", err))
			return zero, incProblem(problem.TypeInternalError, "start scenario create failed")
		}
	}

	var auditApplyID any
	body := IncarnationCreateView{Incarnation: req.Name}
	if applyID != "" {
		auditApplyID = applyID
		body.ApplyID = &applyID
	}
	return IncarnationCreateReply{
		Body: body,
		AuditPayload: apimiddleware.AuditPayload{
			"name":     req.Name,
			"service":  req.Service,
			"covens":   coalesceCoven(covens),
			"apply_id": auditApplyID,
		},
	}, nil
}

// --- Run (MIDDLEWARE-AUDIT incarnation.scenario_started) --------------

// IncarnationRunView — ПЛОСКАЯ доменная проекция 202-тела POST .../scenarios/{scenario}
// (handler-native). Пакет api проецирует её в native IncarnationRunReply.
type IncarnationRunView struct {
	ApplyID     string
	Incarnation string
	Scenario    string
}

// IncarnationRunReply — typed reply RunTyped. Body — плоская доменная проекция 202-тела;
// AuditPayload — для huma-audit-middleware (name/scenario/apply_id).
type IncarnationRunReply struct {
	Body         IncarnationRunView
	AuditPayload apimiddleware.AuditPayload
}

// RunTyped — извлечённая доменная функция POST /v1/incarnations/{name}/scenarios/
// {scenario} (MIDDLEWARE-AUDIT). Parity (w,r)-Run: резолв incarnation → secondary
// error_locked-probe → resolve service-ref + sync input-validation → runner.Start →
// 202 + apply_id. name/scenarioName приходят аргументами (path-bind на huma-слое).
func (h *IncarnationHandler) RunTyped(ctx context.Context, claims *jwt.Claims, name, scenarioName string, input map[string]any) (IncarnationRunReply, error) {
	var zero IncarnationRunReply

	if !incarnation.ValidName(name) {
		return zero, incProblem(problem.TypeValidationFailed, "path 'name' must match "+incarnation.NamePattern)
	}
	if !scenario.ValidScenarioName(scenarioName) {
		return zero, incProblem(problem.TypeValidationFailed, "path 'scenario' must match "+scenario.ScenarioNamePattern)
	}

	if h.runner == nil || h.services == nil {
		return zero, incProblem(problem.TypeInternalError, "scenario runner is not configured")
	}

	inc, err := incarnation.SelectByName(ctx, h.db, name)
	if err != nil {
		if errors.Is(err, incarnation.ErrIncarnationNotFound) {
			return zero, incProblem(problem.TypeNotFound, "incarnation "+name+" not found")
		}
		h.logger.Error("incarnation.run: select failed", slog.String("name", name), slog.Any("error", err))
		return zero, incProblem(problem.TypeInternalError, "select incarnation failed")
	}

	if inc.Status == incarnation.StatusErrorLocked {
		return zero, incProblem(problem.TypeIncarnationLocked,
			"incarnation "+name+" is error_locked — unlock required before next run")
	}

	serviceRef, ok := h.services.Resolve(inc.Service)
	if !ok {
		return zero, incProblem(problem.TypeValidationFailed,
			"service "+inc.Service+" is not registered (manage via service.* API, ADR-029)")
	}

	if h.loader != nil {
		if err := scenario.ValidateInput(ctx, h.loader, serviceRef, scenarioName, input); err != nil {
			if errors.Is(err, scenario.ErrInputInvalid) {
				return zero, incProblem(problem.TypeValidationFailed, "input_invalid: "+err.Error())
			}
			if errors.Is(err, scenario.ErrValidateFailed) {
				return zero, incProblem(problem.TypeValidationFailed, "validation_failed: "+err.Error())
			}
			h.logger.Error("incarnation.run: input validation failed",
				slog.String("name", name), slog.String("scenario", scenarioName), slog.Any("error", err))
			return zero, incProblem(problem.TypeInternalError,
				"validate scenario "+scenarioName+" input failed")
		}
	}

	applyID := audit.NewULID()
	if err := h.runner.Start(ctx, scenario.RunSpec{
		ApplyID:         applyID,
		IncarnationName: name,
		ServiceRef:      serviceRef,
		ScenarioName:    scenarioName,
		Input:           input,
		StartedByAID:    claims.Subject,
	}); err != nil {
		h.logger.Error("incarnation.run: scenario start failed",
			slog.String("name", name), slog.String("scenario", scenarioName),
			slog.String("apply_id", applyID), slog.Any("error", err))
		return zero, incProblem(problem.TypeInternalError, "start scenario "+scenarioName+" failed")
	}

	return IncarnationRunReply{
		Body: IncarnationRunView{ApplyID: applyID, Incarnation: name, Scenario: scenarioName},
		AuditPayload: apimiddleware.AuditPayload{
			"name":     name,
			"scenario": scenarioName,
			"apply_id": applyID,
		},
	}, nil
}

// --- Unlock (MIDDLEWARE-AUDIT incarnation.unlocked) -------------------

// IncarnationUnlockView — ПЛОСКАЯ доменная проекция 200-тела POST .../unlock (handler-native).
// Пакет api проецирует её в native IncarnationUnlockReply. PreviousStatus/Status — RAW string
// домена (native-тип в api держит enum-форму). UnlockedAt — наносекундный time-wire.
type IncarnationUnlockView struct {
	Name           string
	PreviousStatus string
	Status         string
	UnlockedAt     time.Time
	UnlockedByAID  string
}

// IncarnationUnlockReply — typed reply UnlockTyped. Body — плоская доменная проекция 200-тела;
// AuditPayload — для huma-audit-middleware (name/previous_status/reason).
type IncarnationUnlockReply struct {
	Body         IncarnationUnlockView
	AuditPayload apimiddleware.AuditPayload
}

// UnlockTyped — извлечённая доменная функция POST /v1/incarnations/{name}/unlock
// (MIDDLEWARE-AUDIT). Parity (w,r)-Unlock: снятие error_locked/migration_failed под
// FOR UPDATE → 200 {name, previous_status, status, unlocked_by_aid, unlocked_at}.
func (h *IncarnationHandler) UnlockTyped(ctx context.Context, claims *jwt.Claims, name, reason string) (IncarnationUnlockReply, error) {
	var zero IncarnationUnlockReply

	if !incarnation.ValidName(name) {
		return zero, incProblem(problem.TypeValidationFailed, "path 'name' must match "+incarnation.NamePattern)
	}
	if reason == "" {
		return zero, incProblem(problem.TypeValidationFailed, "field 'reason' is required")
	}
	// JSON-Schema maxLength считает Unicode code points (руны), не байты —
	// сверяем рунами, иначе кириллический reason отбивается 422 вдвое раньше лимита.
	if utf8.RuneCountInString(reason) > incarnation.ReasonMaxLen {
		return zero, incProblem(problem.TypeValidationFailed,
			fmt.Sprintf("field 'reason' must be at most %d characters", incarnation.ReasonMaxLen))
	}

	historyID := audit.NewULID()
	res, err := incarnation.Unlock(ctx, h.db, name, reason, claims.Subject, historyID)
	if err != nil {
		switch {
		case errors.Is(err, incarnation.ErrIncarnationNotFound):
			return zero, incProblem(problem.TypeNotFound, "incarnation "+name+" not found")
		case errors.Is(err, incarnation.ErrIncarnationNotLocked):
			return zero, incProblem(problem.TypeIncarnationLocked,
				"incarnation "+name+" is not error_locked — nothing to unlock")
		default:
			h.logger.Error("incarnation.unlock: failed",
				slog.String("name", name), slog.String("by_aid", claims.Subject), slog.Any("error", err))
			return zero, incProblem(problem.TypeInternalError, "unlock incarnation failed")
		}
	}

	return IncarnationUnlockReply{
		Body: IncarnationUnlockView{
			Name:           name,
			PreviousStatus: string(res.PreviousStatus),
			Status:         string(incarnation.StatusReady),
			UnlockedByAID:  claims.Subject,
			UnlockedAt:     time.Now().UTC(),
		},
		AuditPayload: apimiddleware.AuditPayload{
			"name":            name,
			"previous_status": string(res.PreviousStatus),
			"reason":          reason,
		},
	}, nil
}

// --- Upgrade (MIDDLEWARE-AUDIT incarnation.upgrade_started) -----------

// IncarnationUpgradeView — ПЛОСКАЯ доменная проекция 202-тела POST .../upgrade (handler-native).
// Пакет api проецирует её в native IncarnationUpgradeReply.
type IncarnationUpgradeView struct {
	ApplyID string
}

// IncarnationUpgradeReply — typed reply UpgradeTyped. Body — плоская доменная проекция 202-тела;
// AuditPayload — для huma-audit-middleware (name/to_version/apply_id).
type IncarnationUpgradeReply struct {
	Body         IncarnationUpgradeView
	AuditPayload apimiddleware.AuditPayload
}

// UpgradeTyped — извлечённая доменная функция POST /v1/incarnations/{name}/upgrade
// (MIDDLEWARE-AUDIT). Parity (w,r)-Upgrade: SelectByName → PrepareUpgrade →
// UpgradeStateSchema (sync под 202) → 202 + apply_id.
func (h *IncarnationHandler) UpgradeTyped(ctx context.Context, claims *jwt.Claims, name, toVersion string) (IncarnationUpgradeReply, error) {
	var zero IncarnationUpgradeReply

	if !incarnation.ValidName(name) {
		return zero, incProblem(problem.TypeValidationFailed, "path 'name' must match "+incarnation.NamePattern)
	}
	if h.loader == nil || h.services == nil {
		return zero, incProblem(problem.TypeInternalError, "service loader is not configured")
	}
	if toVersion == "" {
		return zero, incProblem(problem.TypeValidationFailed, "field 'to_version' is required")
	}

	inc, err := incarnation.SelectByName(ctx, h.db, name)
	if err != nil {
		if errors.Is(err, incarnation.ErrIncarnationNotFound) {
			return zero, incProblem(problem.TypeNotFound, "incarnation "+name+" not found")
		}
		h.logger.Error("incarnation.upgrade: select failed", slog.String("name", name), slog.Any("error", err))
		return zero, incProblem(problem.TypeInternalError, "select incarnation failed")
	}

	applyID := audit.NewULID()
	changedBy := claims.Subject
	upIn, err := incarnation.PrepareUpgrade(ctx, h.services, h.loader, inc, toVersion, applyID, &changedBy)
	if err != nil {
		switch {
		case errors.Is(err, incarnation.ErrUpgradeNoop):
			return zero, incProblem(problem.TypeValidationFailed,
				"to_version "+toVersion+" совпадает с текущей версией incarnation — апгрейдить нечего")
		case errors.Is(err, incarnation.ErrDowngradeViaRef):
			return zero, incProblem(problem.TypeIncarnationLocked,
				"to_version "+toVersion+" downgrades state_schema_version — forward-only (ADR-019)")
		case errors.Is(err, artifact.ErrMigrationChainBroken):
			return zero, incProblem(problem.TypeValidationFailed,
				"migration chain to "+toVersion+" is broken: "+err.Error())
		default:
			h.logger.Error("incarnation.upgrade: prepare failed",
				slog.String("name", name), slog.String("service", inc.Service),
				slog.String("to_version", toVersion), slog.Any("error", err))
			return zero, incProblem(problem.TypeInternalError, "prepare incarnation upgrade failed")
		}
	}

	if _, err := incarnation.UpgradeStateSchema(ctx, h.db, upIn); err != nil {
		switch {
		case errors.Is(err, incarnation.ErrIncarnationNotFound):
			return zero, incProblem(problem.TypeNotFound, "incarnation "+name+" not found")
		case errors.Is(err, incarnation.ErrIncarnationBusy):
			return zero, incProblem(problem.TypeIncarnationLocked,
				"incarnation "+name+" is applying — upgrade rejected until run completes")
		case errors.Is(err, incarnation.ErrIncarnationLocked):
			return zero, incProblem(problem.TypeIncarnationLocked,
				"incarnation "+name+" is locked — unlock required before upgrade")
		case errors.Is(err, incarnation.ErrDowngradeUnsupported):
			return zero, incProblem(problem.TypeIncarnationLocked,
				"to_version "+toVersion+" downgrades state_schema_version — forward-only (ADR-019)")
		case errors.Is(err, incarnation.ErrSchemaVersionMismatch):
			return zero, incProblem(problem.TypeIncarnationLocked,
				"incarnation "+name+" schema changed concurrently — retry upgrade")
		default:
			h.logger.Error("incarnation.upgrade: failed",
				slog.String("name", name), slog.String("to_version", toVersion),
				slog.String("apply_id", applyID), slog.Any("error", err))
			return zero, incProblem(problem.TypeInternalError, "upgrade incarnation failed")
		}
	}

	return IncarnationUpgradeReply{
		Body: IncarnationUpgradeView{ApplyID: applyID},
		AuditPayload: apimiddleware.AuditPayload{
			"name":       name,
			"to_version": toVersion,
			"apply_id":   applyID,
		},
	}, nil
}

// --- RerunLast (SELF-AUDIT incarnation.rerun_last) --------------------

// IncarnationRerunLastView — ПЛОСКАЯ доменная проекция 202-тела POST .../rerun-last
// (handler-native). Пакет api проецирует её в native IncarnationRerunLastReply.
type IncarnationRerunLastView struct {
	ApplyID     string
	Incarnation string
	// Scenario — имя перезапущенного сценария (последний упавший: bootstrap
	// `create`/… ИЛИ day-2 add_user/…). UI показывает его лейблом.
	Scenario string
}

// RerunLastTyped — извлечённая доменная функция POST /v1/incarnations/{name}/
// rerun-last (SELF-AUDIT: incarnation.rerun_last пишет САМ handler внутри —
// payload previous_status/scenario известны только после UnlockForRerun). Parity
// (w,r)-RerunLast. source — ScenarioInvocationSource(ctx) (api / mcp). 202 +
// apply_id + scenario.
func (h *IncarnationHandler) RerunLastTyped(ctx context.Context, claims *jwt.Claims, name, reason string) (IncarnationRerunLastView, error) {
	var zero IncarnationRerunLastView

	if !incarnation.ValidName(name) {
		return zero, incProblem(problem.TypeValidationFailed, "path 'name' must match "+incarnation.NamePattern)
	}
	if reason == "" {
		return zero, incProblem(problem.TypeValidationFailed, "field 'reason' is required")
	}
	// JSON-Schema maxLength считает Unicode code points (руны), не байты —
	// сверяем рунами, иначе кириллический reason отбивается 422 вдвое раньше лимита.
	if utf8.RuneCountInString(reason) > incarnation.ReasonMaxLen {
		return zero, incProblem(problem.TypeValidationFailed,
			fmt.Sprintf("field 'reason' must be at most %d characters", incarnation.ReasonMaxLen))
	}
	if h.runner == nil || h.services == nil {
		return zero, incProblem(problem.TypeInternalError, "scenario runner is not configured")
	}

	inc, err := incarnation.SelectByName(ctx, h.db, name)
	if err != nil {
		if errors.Is(err, incarnation.ErrIncarnationNotFound) {
			return zero, incProblem(problem.TypeNotFound, "incarnation "+name+" not found")
		}
		h.logger.Error("incarnation.rerun-last: select failed", slog.String("name", name), slog.Any("error", err))
		return zero, incProblem(problem.TypeInternalError, "select incarnation failed")
	}

	serviceRef, ok := h.services.Resolve(inc.Service)
	if !ok {
		return zero, incProblem(problem.TypeValidationFailed,
			"service "+inc.Service+" is not registered (manage via service.* API, ADR-029)")
	}
	serviceRef.Ref = inc.ServiceVersion

	applyID := audit.NewULID()
	res, err := incarnation.UnlockForRerun(ctx, h.db, name, reason, claims.Subject, applyID, applyID)
	if err != nil {
		switch {
		case errors.Is(err, incarnation.ErrIncarnationNotFound):
			return zero, incProblem(problem.TypeNotFound, "incarnation "+name+" not found")
		case errors.Is(err, incarnation.ErrIncarnationNotErrorLocked):
			return zero, incProblem(problem.TypeIncarnationLocked,
				"incarnation "+name+" is not error_locked — rerun-last requires error_locked")
		case errors.Is(err, incarnation.ErrRerunInputUnavailable):
			return zero, incProblem(problem.TypeRerunInputUnavailable,
				"incarnation "+name+" rerun-last неприменим: input упавшего прогона недоступен "+
					"(прогон упал до dispatch — render/no_hosts/preflight, рецепт не записан; "+
					"рецепт вычищен ретеншном; legacy-прогон без рецепта) — сними блок обычным unlock "+
					"и запусти нужный сценарий вручную с явным input")
		default:
			h.logger.Error("incarnation.rerun-last: unlock failed",
				slog.String("name", name), slog.String("by_aid", claims.Subject), slog.Any("error", err))
			return zero, incProblem(problem.TypeInternalError, "rerun-last unlock failed")
		}
	}

	// rerun-last перезапускает ПОСЛЕДНИЙ упавший сценарий (UnlockForRerun вернул его
	// имя и input под FOR UPDATE): bootstrap `create`/… на create-пути ИЛИ day-2
	// add_user/… — с ТЕМИ ЖЕ входными значениями (spec.input или recipe.input), а не
	// с дефолтами.
	if err := h.runner.Start(ctx, scenario.RunSpec{
		ApplyID:         applyID,
		IncarnationName: name,
		ServiceRef:      serviceRef,
		ScenarioName:    res.Scenario,
		Input:           res.Input,
		StartedByAID:    claims.Subject,
		FromLocked:      true,
	}); err != nil {
		h.logger.Error("incarnation.rerun-last: scenario start failed",
			slog.String("name", name), slog.String("apply_id", applyID),
			slog.String("scenario", res.Scenario), slog.Any("error", err))
		return zero, incProblem(problem.TypeInternalError, "start scenario "+res.Scenario+" failed")
	}

	if h.auditW != nil {
		_ = h.auditW.Write(ctx, &audit.Event{
			EventType:     audit.EventIncarnationRerunLast,
			Source:        apimiddleware.ScenarioInvocationSource(ctx),
			ArchonAID:     claims.Subject,
			CorrelationID: applyID,
			Payload: map[string]any{
				"name":            name,
				"reason":          reason,
				"scenario":        res.Scenario,
				"previous_status": string(res.PreviousStatus),
				"apply_id":        applyID,
			},
		})
	}

	return IncarnationRerunLastView{ApplyID: applyID, Incarnation: name, Scenario: res.Scenario}, nil
}

// --- CheckDrift (SELF-AUDIT incarnation.drift_checked) ----------------

// CheckDriftTyped — извлечённая доменная функция POST /v1/incarnations/{name}/
// check-drift (SELF-AUDIT: incarnation.drift_checked пишет САМ handler — payload
// drift_summary собирается после CheckDrift). Parity (w,r)-CheckDrift: sync
// drift-проверка → 200 + *scenario.DriftReport.
func (h *IncarnationHandler) CheckDriftTyped(ctx context.Context, claims *jwt.Claims, name string, inputOverride map[string]any) (*scenario.DriftReport, error) {
	if !incarnation.ValidName(name) {
		return nil, incProblem(problem.TypeValidationFailed, "path 'name' must match "+incarnation.NamePattern)
	}
	if h.drift == nil || h.services == nil {
		return nil, incProblem(problem.TypeInternalError, "drift checker is not configured")
	}

	inc, err := incarnation.SelectByName(ctx, h.db, name)
	if err != nil {
		if errors.Is(err, incarnation.ErrIncarnationNotFound) {
			return nil, incProblem(problem.TypeNotFound, "incarnation "+name+" not found")
		}
		h.logger.Error("incarnation.check-drift: select failed", slog.String("name", name), slog.Any("error", err))
		return nil, incProblem(problem.TypeInternalError, "select incarnation failed")
	}

	serviceRef, ok := h.services.Resolve(inc.Service)
	if !ok {
		return nil, incProblem(problem.TypeValidationFailed,
			"service "+inc.Service+" is not registered (manage via service.* API, ADR-029)")
	}

	applyID := audit.NewULID()
	report, err := h.drift.CheckDrift(ctx, scenario.CheckDriftSpec{
		ApplyID:         applyID,
		IncarnationName: name,
		ServiceRef:      serviceRef,
		InputOverride:   inputOverride,
		StartedByAID:    claims.Subject,
	})
	if err != nil {
		if errors.Is(err, scenario.ErrConvergeMissing) {
			return nil, incProblem(problem.TypeValidationFailed,
				"drift-проверка недоступна для service "+inc.Service+": сценарий converge отсутствует в текущем service-snapshot-е")
		}
		if errors.Is(err, scenario.ErrDriftInputMissing) {
			return nil, incProblem(problem.TypeValidationFailed, "drift-input не резолвится: "+err.Error())
		}
		h.logger.Error("incarnation.check-drift: failed",
			slog.String("name", name), slog.String("apply_id", applyID), slog.Any("error", err))
		return nil, incProblem(problem.TypeInternalError, "check-drift failed")
	}

	hasDrift := report.Summary.HostsDrifted > 0 || report.Summary.HostsFailed > 0
	if err := h.drift.MarkDriftStatus(ctx, name, hasDrift); err != nil {
		h.logger.Warn("incarnation.check-drift: статус drift не зафиксирован",
			slog.String("name", name), slog.Any("error", err))
	}

	if h.auditW != nil {
		_ = h.auditW.Write(ctx, &audit.Event{
			EventType:     audit.EventIncarnationDriftChecked,
			Source:        audit.SourceAPI,
			ArchonAID:     claims.Subject,
			CorrelationID: applyID,
			Payload: map[string]any{
				"name":     name,
				"scenario": scenario.ConvergeScenarioName,
				"apply_id": applyID,
				"drift_summary": map[string]any{
					"hosts_drifted":     report.Summary.HostsDrifted,
					"hosts_clean":       report.Summary.HostsClean,
					"hosts_unsupported": report.Summary.HostsUnsupported,
					"hosts_failed":      report.Summary.HostsFailed,
				},
			},
		})
	}

	return report, nil
}

// --- Destroy (SELF-AUDIT incarnation.destroy_started — пишет service-слой) ---

// IncarnationDestroyView — ПЛОСКАЯ доменная проекция 202-тела DELETE /v1/incarnations/{name}
// (handler-native). Пакет api проецирует её в native IncarnationDestroyReply.
type IncarnationDestroyView struct {
	ApplyID string
}

// DestroyTyped — извлечённая доменная функция DELETE /v1/incarnations/{name}
// (SELF-AUDIT: destroy_started пишет сам [incarnation.Destroy] / destroy_completed —
// [DeleteAfterTeardown]; audit-middleware НЕ навешан). Parity (w,r)-Destroy: force —
// allow_destroy (path-bind на huma-слое). 202 + apply_id.
func (h *IncarnationHandler) DestroyTyped(ctx context.Context, claims *jwt.Claims, name string, force bool) (IncarnationDestroyView, error) {
	var zero IncarnationDestroyView

	if !incarnation.ValidName(name) {
		return zero, incProblem(problem.TypeValidationFailed, "path 'name' must match "+incarnation.NamePattern)
	}
	if h.destroyer == nil || h.services == nil || h.loader == nil {
		return zero, incProblem(problem.TypeInternalError, "destroy is not configured")
	}

	inc, err := incarnation.SelectByName(ctx, h.db, name)
	if err != nil {
		if errors.Is(err, incarnation.ErrIncarnationNotFound) {
			return zero, incProblem(problem.TypeNotFound, "incarnation "+name+" not found")
		}
		h.logger.Error("incarnation.destroy: select failed", slog.String("name", name), slog.Any("error", err))
		return zero, incProblem(problem.TypeInternalError, "select incarnation failed")
	}

	art, err := incarnation.PrepareDestroy(ctx, h.services, h.loader, inc, true)
	if err != nil {
		switch {
		case errors.Is(err, incarnation.ErrServiceNotRegistered):
			return zero, incProblem(problem.TypeInternalError, "service "+inc.Service+" is not registered")
		default:
			h.logger.Error("incarnation.destroy: prepare failed",
				slog.String("name", name), slog.String("service", inc.Service), slog.Any("error", err))
			return zero, incProblem(problem.TypeInternalError, "prepare incarnation destroy failed")
		}
	}

	autoDestroy := true
	if art != nil && art.Manifest != nil {
		autoDestroy = art.Manifest.Lifecycle.AutoDestroyEnabled()
	}
	effectiveForce := force || !autoDestroy

	if !effectiveForce {
		hasScenario, herr := incarnation.HasDestroyScenario(h.loader, art)
		if herr != nil {
			h.logger.Error("incarnation.destroy: scenario probe failed",
				slog.String("name", name), slog.String("service", inc.Service), slog.Any("error", herr))
			return zero, incProblem(problem.TypeInternalError, "prepare incarnation destroy failed")
		}
		if !hasScenario {
			return zero, incProblem(problem.TypeValidationFailed,
				"service "+inc.Service+" has no `destroy` scenario — pass allow_destroy=true to force destroy without teardown")
		}
	}

	applyID := audit.NewULID()

	if _, err := incarnation.Destroy(ctx, h.db, h.auditW, name, effectiveForce,
		audit.SourceAPI, claims.Subject, applyID, h.logger); err != nil {
		switch {
		case errors.Is(err, incarnation.ErrIncarnationNotFound):
			return zero, incProblem(problem.TypeNotFound, "incarnation "+name+" not found")
		case errors.Is(err, incarnation.ErrIncarnationNotDestroyable):
			return zero, incProblem(problem.TypeIncarnationLocked,
				"incarnation "+name+" status does not allow destroy (applying / destroying)")
		default:
			h.logger.Error("incarnation.destroy: transition failed",
				slog.String("name", name), slog.String("by_aid", claims.Subject),
				slog.String("apply_id", applyID), slog.Any("error", err))
			return zero, incProblem(problem.TypeInternalError, "destroy incarnation failed")
		}
	}

	if effectiveForce {
		dctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		if _, err := incarnation.DeleteAfterTeardown(dctx, h.db, h.auditW, name, effectiveForce, h.logger); err != nil {
			h.logger.Error("incarnation.destroy: force delete failed",
				slog.String("name", name), slog.String("apply_id", applyID), slog.Any("error", err))
			return zero, incProblem(problem.TypeInternalError, "force-destroy delete failed")
		}
		return IncarnationDestroyView{ApplyID: applyID}, nil
	}

	serviceRef, ok := h.services.Resolve(inc.Service)
	if !ok {
		h.logger.Error("incarnation.destroy: service deregistered between prepare and teardown",
			slog.String("name", name), slog.String("service", inc.Service))
		return zero, incProblem(problem.TypeInternalError, "service "+inc.Service+" is not registered")
	}
	serviceRef.Ref = inc.ServiceVersion
	if err := h.destroyer.StartDestroy(ctx, scenario.RunSpec{
		ApplyID:         applyID,
		IncarnationName: name,
		ServiceRef:      serviceRef,
		StartedByAID:    claims.Subject,
	}); err != nil {
		h.logger.Error("incarnation.destroy: teardown start failed",
			slog.String("name", name), slog.String("apply_id", applyID), slog.Any("error", err))
		return zero, incProblem(problem.TypeInternalError, "start destroy teardown failed")
	}

	return IncarnationDestroyView{ApplyID: applyID}, nil
}

// --- UpdateHosts (SELF-AUDIT incarnation.hosts_updated) ---------------

// IncarnationSpecHostInput — NATIVE элемент hosts[] PATCH .../hosts (handler-native). Заменяет
// IncarnationSpecHost: SID + опц. Role (пусто = не задано).
type IncarnationSpecHostInput struct {
	SID  string
	Role string
}

// UpdateHostsTyped — извлечённая доменная функция PATCH /v1/incarnations/{name}/hosts
// (SELF-AUDIT: incarnation.hosts_updated пишет САМ handler — payload old/new snapshot
// после UpdateHosts). Parity (w,r)-UpdateHosts: три mode над declared spec.hosts[] →
// 200 + полный IncarnationGetView. mode/items приходят аргументами (native, биндятся
// на huma-слое).
func (h *IncarnationHandler) UpdateHostsTyped(ctx context.Context, claims *jwt.Claims, name, mode string, items []IncarnationSpecHostInput) (IncarnationGetView, error) {
	var zero IncarnationGetView

	if !incarnation.ValidName(name) {
		return zero, incProblem(problem.TypeValidationFailed, "path 'name' must match "+incarnation.NamePattern)
	}

	hMode := incarnation.UpdateHostsMode(mode)
	if !incarnation.ValidHostsMode(hMode) {
		return zero, incProblem(problem.TypeValidationFailed, "field 'mode' must be one of replace/append/remove")
	}

	hosts := make([]incarnation.SpecHost, 0, len(items))
	for i, item := range items {
		role := item.Role
		if !soul.ValidSID(item.SID) {
			return zero, incProblem(problem.TypeValidationFailed,
				fmt.Sprintf("hosts[%d].sid must match %s", i, soul.SIDPattern))
		}
		if !validHostRole(role) {
			return zero, incProblem(problem.TypeValidationFailed,
				fmt.Sprintf("hosts[%d].role must be lowercase kebab-case (1..63 chars) or empty", i))
		}
		hosts = append(hosts, incarnation.SpecHost{SID: item.SID, Role: role})
	}

	changedBy := claims.Subject
	res, err := incarnation.UpdateHosts(ctx, h.db, incarnation.UpdateHostsInput{
		Name:         name,
		Hosts:        hosts,
		Mode:         hMode,
		ChangedByAID: &changedBy,
	})
	if err != nil {
		var unk *incarnation.ErrUnknownSouls
		switch {
		case errors.Is(err, incarnation.ErrIncarnationNotFound):
			return zero, incProblem(problem.TypeNotFound, "incarnation "+name+" not found")
		case errors.Is(err, incarnation.ErrIncarnationNotEditable):
			return zero, incProblem(problem.TypeIncarnationLocked,
				"incarnation "+name+" status does not allow spec edits (destroying / destroy_failed)")
		case errors.As(err, &unk):
			return zero, incProblem(problem.TypeValidationFailed,
				"unknown SID(s) in souls registry: "+strings.Join(unk.Missing, ", "))
		default:
			h.logger.Error("incarnation.update-hosts: failed",
				slog.String("name", name), slog.String("mode", string(hMode)), slog.Any("error", err))
			return zero, incProblem(problem.TypeInternalError, "update incarnation hosts failed")
		}
	}

	if h.auditW != nil {
		_ = h.auditW.Write(ctx, &audit.Event{
			EventType: audit.EventIncarnationHostsUpdated,
			Source:    audit.SourceAPI,
			ArchonAID: claims.Subject,
			Payload: map[string]any{
				"name":      name,
				"mode":      string(hMode),
				"old_hosts": specHostsToPayload(res.OldHosts),
				"new_hosts": specHostsToPayload(res.NewHosts),
			},
		})
	}

	// schema-aware маскинг spec/state в reply update-hosts (тот же детальный вид).
	schema := h.secretSchemaForIncarnation(ctx, res.Incarnation)
	return toIncarnationGetView(res.Incarnation, schema), nil
}

// --- SetTraits (SELF-AUDIT incarnation.traits_changed) ----------------

// SetTraitsTyped — извлечённая доменная функция PUT /v1/incarnations/{name}/traits
// (SELF-AUDIT: incarnation.traits_changed пишет САМ handler — payload old/new keys
// после UpdateTraits). Зеркало per-soul bulk replace, но на источнике истины:
// целиком заменяет incarnation.traits → персист (одна tx FOR UPDATE) → проекция в
// souls.traits хостов-членов ([incarnation.SyncTraitsToHosts]) → 200 + полный
// IncarnationGetView. traits приходит аргументом (native, биндится на huma-слое).
// Невалидный набор (формат ключа/значения, nested) → 422 ДО записи. Пустой/nil
// map = очистить метки.
func (h *IncarnationHandler) SetTraitsTyped(ctx context.Context, claims *jwt.Claims, name string, traits map[string]any) (IncarnationGetView, error) {
	var zero IncarnationGetView

	if !incarnation.ValidName(name) {
		return zero, incProblem(problem.TypeValidationFailed, "path 'name' must match "+incarnation.NamePattern)
	}
	if err := soul.ValidateTraitDelta(traits); err != nil {
		return zero, incProblem(problem.TypeValidationFailed, err.Error())
	}

	res, err := incarnation.UpdateTraits(ctx, h.db, name, traits)
	if err != nil {
		if errors.Is(err, incarnation.ErrIncarnationNotFound) {
			return zero, incProblem(problem.TypeNotFound, "incarnation "+name+" not found")
		}
		h.logger.Error("incarnation.set-traits: failed",
			slog.String("name", name), slog.Any("error", err))
		return zero, incProblem(problem.TypeInternalError, "update incarnation traits failed")
	}

	// Sync-hook (ADR-060 amend, R1): incarnation.traits → souls.traits хостов-
	// членов. Best-effort (лог, не валим запрос): incarnation.traits уже записан —
	// проекция до-сойдётся при следующем bind/sync. Replace целиком, в т.ч. на
	// пустом map (очистить метки хостов-членов).
	if serr := incarnation.SyncTraitsToHosts(ctx, h.db, name, res.Incarnation.Traits); serr != nil {
		h.logger.Warn("incarnation.set-traits: sync traits → souls провален (best-effort)",
			slog.String("name", name), slog.Any("error", serr))
	}

	if h.auditW != nil {
		_ = h.auditW.Write(ctx, &audit.Event{
			EventType: audit.EventIncarnationTraitsChanged,
			Source:    apimiddleware.ScenarioInvocationSource(ctx),
			ArchonAID: claims.Subject,
			Payload: map[string]any{
				"name":     name,
				"old_keys": res.OldKeys,
				"new_keys": res.NewKeys,
			},
		})
	}

	// schema-aware маскинг spec/state в reply (тот же детальный вид, что GET / update-hosts).
	schema := h.secretSchemaForIncarnation(ctx, res.Incarnation)
	return toIncarnationGetView(res.Incarnation, schema), nil
}

// --- Get / List / History (READ, без audit) ---------------------------

// GetTyped — извлечённая доменная функция GET /v1/incarnations/{name} (READ).
// inScope — RBAC scope-предикат (ADR-047 S3b-3): вне scope → 404. Извлечён из
// (w,r)-Get; scope-чек выполняет caller (huma-слой) через переданный предикат над
// загруженной incarnation, чтобы не тащить *http.Request в доменную функцию.
func (h *IncarnationHandler) GetTyped(ctx context.Context, name string, inScope func(*incarnation.Incarnation) bool) (IncarnationGetView, error) {
	var zero IncarnationGetView

	if !incarnation.ValidName(name) {
		return zero, incProblem(problem.TypeValidationFailed, "path 'name' must match "+incarnation.NamePattern)
	}
	inc, err := incarnation.SelectByName(ctx, h.db, name)
	if err != nil {
		if errors.Is(err, incarnation.ErrIncarnationNotFound) {
			return zero, incProblem(problem.TypeNotFound, "incarnation "+name+" not found")
		}
		h.logger.Error("incarnation.get: select failed", slog.String("name", name), slog.Any("error", err))
		return zero, incProblem(problem.TypeInternalError, "select incarnation failed")
	}
	if inScope == nil || !inScope(inc) {
		return zero, incProblem(problem.TypeNotFound, "incarnation "+name+" not found")
	}
	// seal/декларатив (ADR-010 §7.4): материализуем secret-схему сервиса для
	// schema-aware маскинга spec/state. Best-effort: nil → деградация к
	// MaskSecrets (vault+regex). single-incarnation детальный вид — загрузка
	// снапшота приемлема (в отличие от List, см. observations).
	schema := h.secretSchemaForIncarnation(ctx, inc)
	return toIncarnationGetView(inc, schema), nil
}

// IncarnationListReply — typed envelope GET /v1/incarnations (handler-native: element —
// доменный IncarnationGetView). Пакет api проецирует его в native-envelope incarnationListReply
// через RegisterTypeAlias на sharedapi.PagedResponse[handlers.IncarnationGetView].
type IncarnationListReply = sharedapi.PagedResponse[IncarnationGetView]

// IncarnationListQuery — параметры List, биндённые на huma-слое (typed-query).
type IncarnationListQuery struct {
	Offset  int
	Limit   int
	Service string
	Status  string
	Coven   string
	SortBy  string
	SortDir string
	// StateParams — query-ключи `state.<field>` → их значения (предикат-фильтр).
	StateParams map[string][]string
}

// ListTyped — извлечённая доменная функция GET /v1/incarnations (READ, typed-query).
// scope резолвится из Purview оператора (ADR-047 S3b-3): fail-closed → пустой список.
// CheckPageBounds → 400 (parity ParsePage). Невалидный status/coven/state-path/sort →
// 422. resolveScope — замыкание caller-а (huma-слой) над claims + serviceFilter.
func (h *IncarnationHandler) ListTyped(ctx context.Context, q IncarnationListQuery, resolveScope func(serviceFilter string) (incarnation.ListScope, bool)) (IncarnationListReply, error) {
	var zero IncarnationListReply

	if err := sharedapi.CheckPageBounds(q.Offset, q.Limit); err != nil {
		return zero, incProblem(problem.TypeMalformedRequest, err.Error())
	}

	var filter incarnation.ListFilter
	filter.Service = q.Service
	if q.Status != "" {
		st := incarnation.Status(q.Status)
		if !incarnation.ValidStatus(st) {
			return zero, incProblem(problem.TypeValidationFailed,
				"invalid 'status' filter: must be one of ready/applying/error_locked/migration_failed")
		}
		filter.Status = st
	}
	if q.Coven != "" {
		if !soul.ValidCoven(q.Coven) {
			return zero, incProblem(problem.TypeValidationFailed, "query 'coven' must match "+soul.CovenPattern)
		}
		filter.Coven = q.Coven
	}

	preds, perr := parseStatePredicatesFromMap(q.StateParams)
	if perr != nil {
		return zero, incProblem(problem.TypeValidationFailed, perr.Error())
	}
	filter.StatePredicates = preds

	filter.SortBy = q.SortBy
	filter.SortDir = incarnation.SortDir(q.SortDir)

	scope, ok := resolveScope(filter.Service)
	if !ok {
		return IncarnationListReply{
			Items:  []IncarnationGetView{},
			Offset: q.Offset,
			Limit:  q.Limit,
			Total:  0,
		}, nil
	}

	items, total, err := incarnation.SelectAll(ctx, h.db, filter, scope, q.Offset, q.Limit)
	if err != nil {
		switch {
		case errors.Is(err, incarnation.ErrInvalidStatePath),
			errors.Is(err, incarnation.ErrInvalidStateOp),
			errors.Is(err, incarnation.ErrInvalidStateValue),
			errors.Is(err, incarnation.ErrInvalidSortField),
			errors.Is(err, incarnation.ErrInvalidSortDir):
			return zero, incProblem(problem.TypeValidationFailed, err.Error())
		}
		h.logger.Error("incarnation.list: select failed", slog.Any("filter", filter), slog.Any("error", err))
		return zero, incProblem(problem.TypeInternalError, "list incarnations failed")
	}

	replies := make([]IncarnationGetView, 0, len(items))
	for _, inc := range items {
		// List — bulk-вид: schema-прокидка НЕ применяется (материализация снапшота
		// per-элемент — недопустимая стоимость на read-hot-path). nil-схема →
		// MaskSecrets (vault+regex), БИТ-В-БИТ. Декларатив доступен на детальном
		// GET/History (см. observations: schema-aware List — отдельный слайс с
		// кешированием schema per-service).
		replies = append(replies, toIncarnationGetView(inc, nil))
	}
	return IncarnationListReply{Items: replies, Offset: q.Offset, Limit: q.Limit, Total: total}, nil
}

// IncarnationHistoryReply — typed envelope GET /v1/incarnations/{name}/history (handler-native:
// element — доменный StateHistoryView). Пакет api проецирует его в native-envelope
// incarnationHistoryReply через RegisterTypeAlias на sharedapi.PagedResponse[handlers.StateHistoryView].
type IncarnationHistoryReply = sharedapi.PagedResponse[StateHistoryView]

// HistoryTyped — извлечённая доменная функция GET /v1/incarnations/{name}/history
// (READ, typed-query). existence-probe (404) + scope-гейт (вне scope → 404, parity
// Get) через переданный inScope-предикат. CheckPageBounds → 400; bad apply_id → 400.
func (h *IncarnationHandler) HistoryTyped(ctx context.Context, name, applyID string, offset, limit int, inScope func(*incarnation.Incarnation) bool) (IncarnationHistoryReply, error) {
	var zero IncarnationHistoryReply

	if !incarnation.ValidName(name) {
		return zero, incProblem(problem.TypeValidationFailed, "path 'name' must match "+incarnation.NamePattern)
	}
	if err := sharedapi.CheckPageBounds(offset, limit); err != nil {
		return zero, incProblem(problem.TypeMalformedRequest, err.Error())
	}

	var filter incarnation.HistoryFilter
	if applyID != "" {
		if !audit.IsValidULID(applyID) {
			return zero, incProblem(problem.TypeMalformedRequest,
				"query 'apply_id' must be a Crockford-base32 ULID (26 chars)")
		}
		filter.ApplyID = applyID
	}

	inc, err := incarnation.SelectByName(ctx, h.db, name)
	if err != nil {
		if errors.Is(err, incarnation.ErrIncarnationNotFound) {
			return zero, incProblem(problem.TypeNotFound, "incarnation "+name+" not found")
		}
		h.logger.Error("incarnation.history: existence-probe failed", slog.String("name", name), slog.Any("error", err))
		return zero, incProblem(problem.TypeInternalError, "history probe failed")
	}

	if inScope == nil || !inScope(inc) {
		return zero, incProblem(problem.TypeNotFound, "incarnation "+name+" not found")
	}

	items, total, err := incarnation.HistorySelectByName(ctx, h.db, name, filter, offset, limit)
	if err != nil {
		h.logger.Error("incarnation.history: select failed",
			slog.String("name", name), slog.String("apply_id", filter.ApplyID), slog.Any("error", err))
		return zero, incProblem(problem.TypeInternalError, "list history failed")
	}
	// secret-схему сервиса материализуем ОДИН раз на запрос (history — single
	// incarnation), переиспользуем для всех записей. Best-effort: nil → MaskSecrets.
	schema := h.secretSchemaForIncarnation(ctx, inc)
	entries := make([]StateHistoryView, 0, len(items))
	for _, e := range items {
		entries = append(entries, toStateHistoryView(e, schema))
	}
	return IncarnationHistoryReply{Items: entries, Offset: offset, Limit: limit, Total: total}, nil
}

// --- Runs (READ) — прогоны инкарнации per-task view -------------------
//
// GET /v1/incarnations/{name}/runs         — список прогонов (свёртка apply_runs).
// GET /v1/incarnations/{name}/runs/{apply} — детали одного прогона (срез по хостам,
//                                            адрес упавшей задачи = «текущая джоба»).
//
// scope-гейт — тот же inScope-предикат, что у History/Get (action=history):
// эндпоинты в incarnation-домене, scope резолвится по incarnation. WHERE по
// incarnation_name в store-слое дополнительно отсекает прогоны чужой инкарнации.

// RunSummaryView — ПЛОСКАЯ доменная строка списка прогонов. Пакет api проецирует
// её в native. Status — агрегатный статус прогона (applying/success/failed/cancelled).
// Incarnation — владелец прогона: в per-incarnation wire не отдаётся (имплицитен),
// в глобальном GET /v1/runs — отдельное поле entry.
type RunSummaryView struct {
	ApplyID      string
	Incarnation  string
	Scenario     string
	Status       string
	StartedAt    time.Time
	FinishedAt   *time.Time
	StartedByAID *string
}

// RunHostStatusView — ПЛОСКАЯ доменная строка одного хоста в деталях прогона.
// FailedTaskIdx/FailedPlanIndex/ErrorSummary заполнены только на упавшем хосте
// (nil на success/ещё-running).
type RunHostStatusView struct {
	SID             string
	Status          string
	Passage         int
	FailedTaskIdx   *int
	FailedPlanIndex *int
	ErrorSummary    *string
	Attempt         int32
	CancelRequested bool
}

// RunDetailView — ПЛОСКАЯ доменная проекция деталей прогона (шапка + срез по хостам).
type RunDetailView struct {
	ApplyID      string
	Scenario     string
	Status       string
	StartedAt    time.Time
	FinishedAt   *time.Time
	StartedByAID *string
	Hosts        []RunHostStatusView
}

// IncarnationRunsReply — typed envelope GET /v1/incarnations/{name}/runs (handler-native:
// element — доменный RunSummaryView). Пакет api проецирует его в native-envelope через
// RegisterTypeAlias на sharedapi.PagedResponse[handlers.RunSummaryView].
type IncarnationRunsReply = sharedapi.PagedResponse[RunSummaryView]

// RunsTyped — доменная функция GET /v1/incarnations/{name}/runs (READ, typed-query).
// existence-probe (404) + scope-гейт (вне scope → 404, parity History) через inScope.
// CheckPageBounds → 400. Отдаёт страницу прогонов, новейшие сверху.
func (h *IncarnationHandler) RunsTyped(ctx context.Context, name string, offset, limit int, inScope func(*incarnation.Incarnation) bool) (IncarnationRunsReply, error) {
	var zero IncarnationRunsReply

	if !incarnation.ValidName(name) {
		return zero, incProblem(problem.TypeValidationFailed, "path 'name' must match "+incarnation.NamePattern)
	}
	if err := sharedapi.CheckPageBounds(offset, limit); err != nil {
		return zero, incProblem(problem.TypeMalformedRequest, err.Error())
	}

	if _, err := h.existenceProbeInScope(ctx, name, inScope, "runs"); err != nil {
		return zero, err
	}

	summaries, total, err := applyrun.ListRunsByIncarnation(ctx, h.db, name, offset, limit)
	if err != nil {
		h.logger.Error("incarnation.runs: list failed", slog.String("name", name), slog.Any("error", err))
		return zero, incProblem(problem.TypeInternalError, "list runs failed")
	}
	items := make([]RunSummaryView, 0, len(summaries))
	for _, s := range summaries {
		items = append(items, newRunSummaryView(s))
	}
	return IncarnationRunsReply{Items: items, Offset: offset, Limit: limit, Total: total}, nil
}

// newRunSummaryView проецирует store-строку [applyrun.RunSummary] в доменный view
// (общая для per-incarnation Runs и глобального AllRuns).
func newRunSummaryView(s applyrun.RunSummary) RunSummaryView {
	return RunSummaryView{
		ApplyID:      s.ApplyID,
		Incarnation:  s.Incarnation,
		Scenario:     s.Scenario,
		Status:       string(s.Status),
		StartedAt:    s.StartedAt,
		FinishedAt:   s.FinishedAt,
		StartedByAID: s.StartedByAID,
	}
}

// RunDetailTyped — доменная функция GET /v1/incarnations/{name}/runs/{apply_id}
// (READ). existence-probe (404) + scope-гейт (вне scope → 404) через inScope; bad
// apply_id → 400; прогон не найден / принадлежит другой инкарнации → 404.
func (h *IncarnationHandler) RunDetailTyped(ctx context.Context, name, applyID string, inScope func(*incarnation.Incarnation) bool) (RunDetailView, error) {
	var zero RunDetailView

	if !incarnation.ValidName(name) {
		return zero, incProblem(problem.TypeValidationFailed, "path 'name' must match "+incarnation.NamePattern)
	}
	if !audit.IsValidULID(applyID) {
		return zero, incProblem(problem.TypeMalformedRequest, "path 'apply_id' must be a Crockford-base32 ULID (26 chars)")
	}

	if _, err := h.existenceProbeInScope(ctx, name, inScope, "runs"); err != nil {
		return zero, err
	}

	d, err := applyrun.SelectRunDetail(ctx, h.db, applyID, name)
	if err != nil {
		if errors.Is(err, applyrun.ErrApplyRunNotFound) {
			return zero, incProblem(problem.TypeNotFound, "run "+applyID+" not found")
		}
		h.logger.Error("incarnation.run-detail: select failed",
			slog.String("name", name), slog.String("apply_id", applyID), slog.Any("error", err))
		return zero, incProblem(problem.TypeInternalError, "get run detail failed")
	}

	hosts := make([]RunHostStatusView, 0, len(d.Hosts))
	for _, hs := range d.Hosts {
		hosts = append(hosts, RunHostStatusView{
			SID:             hs.SID,
			Status:          string(hs.Status),
			Passage:         hs.Passage,
			FailedTaskIdx:   hs.FailedTaskIdx,
			FailedPlanIndex: hs.FailedPlanIndex,
			ErrorSummary:    hs.ErrorSummary,
			Attempt:         hs.Attempt,
			CancelRequested: hs.CancelRequested,
		})
	}
	return RunDetailView{
		ApplyID:      d.ApplyID,
		Scenario:     d.Scenario,
		Status:       string(d.Status),
		StartedAt:    d.StartedAt,
		FinishedAt:   d.FinishedAt,
		StartedByAID: d.StartedByAID,
		Hosts:        hosts,
	}, nil
}

// existenceProbeInScope — общий existence-probe + scope-гейт для Runs/RunDetail:
// SELECT incarnation, вне scope или отсутствие → единый 404 (parity History). action
// — для лога. Возвращает найденную инкарнацию (nil → ошибка уже отдана).
func (h *IncarnationHandler) existenceProbeInScope(ctx context.Context, name string, inScope func(*incarnation.Incarnation) bool, action string) (*incarnation.Incarnation, error) {
	inc, err := incarnation.SelectByName(ctx, h.db, name)
	if err != nil {
		if errors.Is(err, incarnation.ErrIncarnationNotFound) {
			return nil, incProblem(problem.TypeNotFound, "incarnation "+name+" not found")
		}
		h.logger.Error("incarnation."+action+": existence-probe failed", slog.String("name", name), slog.Any("error", err))
		return nil, incProblem(problem.TypeInternalError, action+" probe failed")
	}
	if inScope == nil || !inScope(inc) {
		return nil, incProblem(problem.TypeNotFound, "incarnation "+name+" not found")
	}
	return inc, nil
}

// mapCreatePlanError маппит ошибку [scenario.ResolveCreatePlan] в доменный
// *problemError для CreateTyped (R2: общий резолв create-плана с MCP, но
// HTTP-маппинг свой). Сохраняет дословные 422-детали исходного inline-кода
// (create_scenario_required / create_scenario_invalid / input_invalid /
// validation_failed / assert_failed); прочие (load/parse снапшота, eval-сбой
// pre-flight) → 500 с логом (стадия-specific текст исходного кода сведён к одному
// generic-сообщению — HTTP-код 500 не меняется, оператор видит generic).
func (h *IncarnationHandler) mapCreatePlanError(name, service string, err error) error {
	switch {
	case errors.Is(err, scenario.ErrCreateScenarioRequired):
		return incProblem(problem.TypeValidationFailed, "create_scenario_required: "+err.Error())
	case errors.Is(err, scenario.ErrCreateScenarioNotEligible):
		return incProblem(problem.TypeValidationFailed, "create_scenario_invalid: "+err.Error())
	case errors.Is(err, scenario.ErrInputInvalid):
		return incProblem(problem.TypeValidationFailed, "input_invalid: "+err.Error())
	case errors.Is(err, scenario.ErrValidateFailed):
		return incProblem(problem.TypeValidationFailed, "validation_failed: "+err.Error())
	case errors.Is(err, scenario.ErrAssertFailed):
		return incProblem(problem.TypeAssertFailed, err.Error())
	}
	h.logger.Error("incarnation.create: resolve create plan failed",
		slog.String("name", name), slog.String("service", service), slog.Any("error", err))
	return incProblem(problem.TypeInternalError, "resolve create plan failed")
}

// incProblem — конструктор доменного *problemError для incarnation *Typed-функций
// (instance пуст, caller-huma-слой не нуждается в URL). Симметрично cadence-домену.
func incProblem(typ, detail string) error {
	return &problemError{Details: problem.New(typ, "", detail)}
}
