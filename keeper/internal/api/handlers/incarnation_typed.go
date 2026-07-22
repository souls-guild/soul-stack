package handlers

// FULL-TYPED extractions of the incarnation domain (ADR-054 §Pattern, batch-2g). Each
// *Typed function carries the entire business logic of the corresponding (w,r) handler
// WITHOUT http.ResponseWriter/*http.Request: decode/auth on the huma layer (api package),
// errors as *problemError, success as a typed reply.
//
// Two audit classes (VERIFIED against router.go + handler code; mixing them up = S6 regression):
//
//   - MIDDLEWARE-AUDIT (create / run / unlock / upgrade): the huma-audit middleware
//     (variant B) writes audit from OUTSIDE. *Typed returns a reply CARRYING the audit-payload
//     (field AuditPayload) — the huma register func sets it via SetHumaAuditPayload.
//     The *Typed functions do NOT write audit themselves.
//   - SELF-AUDIT (rerun-last / check-drift / destroy / update-hosts): the handler writes
//     audit ITSELF via h.auditW.Write INSIDE *Typed (the payload is built only after
//     the domain operation — previous_status / drift_summary / old-new snapshot). audit
//     middleware is NOT wired on these routes.
//
// read (get / list / history) — do NOT write audit at all.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sort"
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

// IncarnationSpecStub — a non-empty *IncarnationHandler stub for generating the huma
// OpenAPI fragment (HumaIncarnationSpecYAML): the domain handler is not called during
// dump, but huma.Register requires non-nil. All dependencies are nil.
func IncarnationSpecStub() *IncarnationHandler { return &IncarnationHandler{} }

// --- Create (MIDDLEWARE-AUDIT incarnation.created) --------------------

// IncarnationCreateView — FLAT domain projection of the 202 body of POST /v1/incarnations
// (handler-native). Package api projects it into native IncarnationCreateReply. ApplyID is
// pointer-optional (lifecycle.auto_create:false → incarnation is ready without a run, apply_id omitted).
type IncarnationCreateView struct {
	ApplyID     *string
	Incarnation string
}

// IncarnationCreateRequestInput — NATIVE request form for POST /v1/incarnations (handler-native).
// Replaces IncarnationCreateRequest: the huma-input (package api) binds the body against its
// fields and calls CreateTyped with this flat model. Covens/Input — nil = "not set" (parity with
// the legacy omitempty decode).
type IncarnationCreateRequestInput struct {
	Name    string
	Service string
	Covens  []string
	Input   map[string]any
	// Traits — operator-set trait labels of the incarnation (ADR-060 amend R1): stored in
	// spec.traits → incarnation.traits (source of truth) + projected into souls.traits of
	// member hosts. nil = "not set" (parity with the legacy omitempty decode).
	Traits map[string]any
	// CreateScenario — choice of the starting scenario (multi-create mechanism,
	// Variant A). Empty-choice contract (Phase 2): the service OFFERS create
	// scenarios (≥1 `create: true`) + empty → 422 create_scenario_required; a service
	// WITHOUT create scenarios + empty → bare incarnation (StatusReady without a run).
	// A non-empty name must belong to the create set, else 422; the choice is stored in
	// incarnation.created_scenario (NULL for bare; rerun-last uses it on the create path).
	CreateScenario string
}

// IncarnationCreateReply — typed reply of CreateTyped. Body is the flat domain projection of
// the 202 body; AuditPayload is data for the huma-audit middleware (name/service/covens/apply_id).
type IncarnationCreateReply struct {
	Body         IncarnationCreateView
	AuditPayload apimiddleware.AuditPayload
}

// CreateTyped — extracted domain function POST /v1/incarnations (MIDDLEWARE-AUDIT).
// Parity with (w,r)-Create: name/service/covens validation → resolve service-ref + choice of
// create scenario (Phase 2: required if present / bare if absent) + sync
// input-validation + lifecycle.auto_create → insert row → optional bootstrap start →
// 202 + apply_id. A bare incarnation (service without create scenarios) is created StatusReady
// WITHOUT a run, without apply_id, created_scenario=NULL. Does NOT write audit (middleware
// variant B): returns the payload in the reply.
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
	// bareNoScenario — the service offers no create scenario (no
	// `create: true`): the incarnation is created StatusReady WITHOUT a run (ready for
	// operations). created_scenario is written NULL (see createScenarioCol). Different from
	// autoCreate=false: there a bootstrap scenario EXISTS (chosen) but the run is deferred —
	// created_scenario is non-empty. Neither triggers runner.Start → bare (below).
	bareNoScenario := false
	// createScenario — the actual starting scenario (multi-create mechanism).
	// The default CreateScenarioName (`create`) preserves legacy behavior WITHOUT a loader
	// (stub mode: the set is not resolved, bare cannot be determined → run `create` as
	// before). With a loader present it is overwritten by the operator's choice or the bare
	// marker. Used in all bootstrap-run phases (via createScenario
	// in runner.Start); created_scenario is written via createScenarioCol (NULL when
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

		// Shared resolve of the starting scenario + input validation + pre-flight assert
		// (R2: single scenario.ResolveCreatePlan with MCP callIncarnationCreate). h.loader
		// nil → stub plan (`create`, not bare, auto_create=true). preflighter is h.runner
		// (type-assertion to scenario.AssertPreflighter inside; a ScenarioStarter fake →
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

	// Trait per-incarnation (ADR-060 amend, R1): operator-set traits live in
	// incarnation.spec.traits (top-level API field `traits`, passed into spec
	// above). On the create path we extract them into the incarnation.traits column — it is
	// the source of truth, projected into souls.traits of member hosts by the sync-hook
	// below. An invalid set (key/value format) → 422 BEFORE insert.
	traits, err := incarnation.TraitsFromSpec(spec)
	if err != nil {
		return zero, incProblem(problem.TypeValidationFailed, err.Error())
	}

	// createScenarioCol — value of the created_scenario column: NULL for a bare
	// incarnation (no bootstrap scenario), otherwise a pointer to the chosen name.
	// autoCreate=false does NOT make it NULL — a bootstrap scenario exists (chosen),
	// the run is merely deferred; rerun-last uses it on the create path.
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

	// Sync-hook (ADR-060 amend, R1): incarnation.traits → souls.traits of member
	// hosts. Gated on non-empty traits — otherwise every create without traits
	// would wipe per-soul traits to `{}` (a transitional footgun).
	// On create there are usually 0 members (onboarding happens in scenario create) → no-op;
	// the bind hook in keeper-dispatch catches up the hosts bound during the run. We don't fail
	// create on a projection error (best-effort, logged): the incarnation is already created, sync
	// converges on the next bind/create retry.
	if len(traits) > 0 {
		if serr := incarnation.SyncTraitsToHosts(ctx, h.db, req.Name, traits); serr != nil {
			h.logger.Warn("incarnation.create: sync traits -> souls failed (best-effort)",
				slog.String("name", req.Name), slog.Any("error", serr))
		}
	}

	// bareReady — a live runner stack, but the run is deliberately NOT started: bare
	// incarnation (no create scenario) OR autoCreate=false (run deferred). In that case the
	// incarnation stays StatusReady without a run, apply_id omitted. In stub mode
	// (!runScenario, M0.6c-1 insert-only) apply_id is issued as before — that is NOT bare.
	bareReady := runScenario && (bareNoScenario || !autoCreate)
	// runCreate — whether a bootstrap run starts (needs a live stack, a scenario and
	// autoCreate). Separate from bareReady: stub mode also does not call Start.
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

// IncarnationRunView — FLAT domain projection of the 202 body of POST .../scenarios/{scenario}
// (handler-native). Package api projects it into native IncarnationRunReply.
type IncarnationRunView struct {
	ApplyID     string
	Incarnation string
	Scenario    string
}

// IncarnationRunReply — typed reply of RunTyped. Body is the flat domain projection of the
// 202 body; AuditPayload is for the huma-audit middleware (name/scenario/apply_id).
type IncarnationRunReply struct {
	Body         IncarnationRunView
	AuditPayload apimiddleware.AuditPayload
}

// RunTyped — extracted domain function POST /v1/incarnations/{name}/scenarios/
// {scenario} (MIDDLEWARE-AUDIT). Parity with (w,r)-Run: resolve incarnation → secondary
// error_locked probe → resolve service-ref + sync input-validation → runner.Start →
// 202 + apply_id. name/scenarioName arrive as arguments (path-bind on the huma layer).
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

// IncarnationUnlockView — FLAT domain projection of the 200 body of POST .../unlock (handler-native).
// Package api projects it into native IncarnationUnlockReply. PreviousStatus/Status are the domain's
// RAW string (the native type in api holds the enum form). UnlockedAt is a nanosecond time-wire.
type IncarnationUnlockView struct {
	Name           string
	PreviousStatus string
	Status         string
	UnlockedAt     time.Time
	UnlockedByAID  string
}

// IncarnationUnlockReply — typed reply of UnlockTyped. Body is the flat domain projection of the
// 200 body; AuditPayload is for the huma-audit middleware (name/previous_status/reason).
type IncarnationUnlockReply struct {
	Body         IncarnationUnlockView
	AuditPayload apimiddleware.AuditPayload
}

// UnlockTyped — extracted domain function POST /v1/incarnations/{name}/unlock
// (MIDDLEWARE-AUDIT). Parity with (w,r)-Unlock: clearing error_locked/migration_failed under
// FOR UPDATE → 200 {name, previous_status, status, unlocked_by_aid, unlocked_at}.
func (h *IncarnationHandler) UnlockTyped(ctx context.Context, claims *jwt.Claims, name, reason string) (IncarnationUnlockReply, error) {
	var zero IncarnationUnlockReply

	if !incarnation.ValidName(name) {
		return zero, incProblem(problem.TypeValidationFailed, "path 'name' must match "+incarnation.NamePattern)
	}
	if reason == "" {
		return zero, incProblem(problem.TypeValidationFailed, "field 'reason' is required")
	}
	// JSON-Schema maxLength counts Unicode code points (runes), not bytes —
	// we check in runes, otherwise a Cyrillic reason is rejected with 422 at half the limit.
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

// IncarnationUpgradeView — FLAT domain projection of the 202 body of POST .../upgrade (handler-native).
// Package api projects it into native IncarnationUpgradeReply.
type IncarnationUpgradeView struct {
	// ApplyID — M: ULID of the state migration (back-compat, always present).
	ApplyID string
	// RunApplyID — R: ULID of the auto-started upgrade run (ADR-0068 §5, found branch).
	// nil in legacy (no upgrade scenario found → drift without host orchestration).
	RunApplyID *string
}

// IncarnationUpgradeReply — typed reply of UpgradeTyped. Body is the flat domain projection of
// the 202 body; AuditPayload is for the huma-audit middleware (name/to_version/apply_id).
type IncarnationUpgradeReply struct {
	Body         IncarnationUpgradeView
	AuditPayload apimiddleware.AuditPayload
}

// UpgradeTyped — extracted domain function POST /v1/incarnations/{name}/upgrade
// (MIDDLEWARE-AUDIT). Parity with (w,r)-Upgrade: SelectByName → PrepareUpgrade →
// UpgradeStateSchema (sync under 202) → 202 + apply_id.
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
				"to_version "+toVersion+" matches the current incarnation version - nothing to upgrade")
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

	// found mode (ADR-0068 §5): an upgrade scenario was found → generate R (ULID of the
	// Runner run, SEPARATE from M) and commit the auto-start (upgradeTx reserves
	// applying instead of drift). The runner is required — we check it BEFORE reserving
	// applying, otherwise the incarnation would hang in applying without a run.
	var runApplyID string
	if upIn.UpgradeSlug != "" {
		if h.runner == nil {
			return zero, incProblem(problem.TypeInternalError, "scenario runner is not configured")
		}
		runApplyID = audit.NewULID()
		upIn.RunApplyID = runApplyID
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

	// Found → auto-start of the upgrade scenario by the Runner on the new pin (upIn.TargetRef.Ref
	// = to_version). FromLocked: applying is already reserved by upgradeTx (otherwise lockRun
	// would hit applying); FromUpgrade: load upgrade/<slug>/. Input is empty —
	// the upgrade scenario operates on state, NOT input (ADR-0068 §7). We mirror the Start
	// error as in RerunLastTyped (applying is reserved in the tx; on a start failure the
	// incarnation stays in applying, triage/rerun-last will drive it).
	if upIn.UpgradeSlug != "" {
		if err := h.runner.Start(ctx, scenario.RunSpec{
			ApplyID:         runApplyID,
			IncarnationName: name,
			ServiceRef:      upIn.TargetRef,
			ScenarioName:    upIn.UpgradeSlug,
			FromUpgrade:     true,
			FromLocked:      true,
			StartedByAID:    claims.Subject,
		}); err != nil {
			h.logger.Error("incarnation.upgrade: upgrade-scenario start failed",
				slog.String("name", name), slog.String("run_apply_id", runApplyID),
				slog.String("scenario", upIn.UpgradeSlug), slog.Any("error", err))
			return zero, incProblem(problem.TypeInternalError, "start upgrade scenario "+upIn.UpgradeSlug+" failed")
		}
	} else {
		// Legacy (ADR-0068 §5): a transition without an upgrade scenario → drift without host
		// orchestration, the operator drives it with a regular apply. from = pre-upgrade pin
		// (inc was not reloaded after the tx).
		h.logger.Warn("incarnation.upgrade: no upgrade scenario for transition — legacy drift, manual apply required",
			slog.String("incarnation", name),
			slog.String("from", inc.ServiceVersion),
			slog.String("to", toVersion))
	}

	view := IncarnationUpgradeView{ApplyID: applyID}
	auditPayload := apimiddleware.AuditPayload{
		"name":       name,
		"to_version": toVersion,
		"apply_id":   applyID,
	}
	if runApplyID != "" {
		view.RunApplyID = &runApplyID
		auditPayload["run_apply_id"] = runApplyID
	}
	return IncarnationUpgradeReply{Body: view, AuditPayload: auditPayload}, nil
}

// --- RerunLast (SELF-AUDIT incarnation.rerun_last) --------------------

// IncarnationRerunLastView — FLAT domain projection of the 202 body of POST .../rerun-last
// (handler-native). Package api projects it into native IncarnationRerunLastReply.
type IncarnationRerunLastView struct {
	ApplyID     string
	Incarnation string
	// Scenario — name of the re-run scenario (the last failed one: bootstrap
	// `create`/… OR an operational add_user/…). The UI shows it as a label.
	Scenario string
}

// RerunLastTyped — extracted domain function POST /v1/incarnations/{name}/
// rerun-last (SELF-AUDIT: the handler writes incarnation.rerun_last ITSELF inside —
// the previous_status/scenario payload is known only after UnlockForRerun). Parity with
// (w,r)-RerunLast. source is ScenarioInvocationSource(ctx) (api / mcp). 202 +
// apply_id + scenario.
func (h *IncarnationHandler) RerunLastTyped(ctx context.Context, claims *jwt.Claims, name, reason string) (IncarnationRerunLastView, error) {
	var zero IncarnationRerunLastView

	if !incarnation.ValidName(name) {
		return zero, incProblem(problem.TypeValidationFailed, "path 'name' must match "+incarnation.NamePattern)
	}
	if reason == "" {
		return zero, incProblem(problem.TypeValidationFailed, "field 'reason' is required")
	}
	// JSON-Schema maxLength counts Unicode code points (runes), not bytes —
	// we check in runes, otherwise a Cyrillic reason is rejected with 422 at half the limit.
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
				"incarnation "+name+" rerun-last is not applicable: input of the failed run is unavailable "+
					"(the run failed before dispatch - render/no_hosts/preflight, no recipe recorded; "+
					"the recipe was purged by retention; legacy run without a recipe) - clear the lock via plain unlock "+
					"and launch the desired scenario manually with an explicit input")
		default:
			h.logger.Error("incarnation.rerun-last: unlock failed",
				slog.String("name", name), slog.String("by_aid", claims.Subject), slog.Any("error", err))
			return zero, incProblem(problem.TypeInternalError, "rerun-last unlock failed")
		}
	}

	// rerun-last re-runs the LAST failed scenario (UnlockForRerun returned its
	// name and input under FOR UPDATE): bootstrap `create`/… on the create path OR an
	// operational add_user/… — with the SAME input values (spec.input or recipe.input), not
	// with defaults.
	if err := h.runner.Start(ctx, scenario.RunSpec{
		ApplyID:         applyID,
		IncarnationName: name,
		ServiceRef:      serviceRef,
		ScenarioName:    res.Scenario,
		Input:           res.Input,
		StartedByAID:    claims.Subject,
		FromLocked:      true,
		// The failed run may have been an upgrade scenario (recipe.from_upgrade) — then
		// the re-run must load from upgrade/<slug>/, not scenario/ (ADR-0068).
		FromUpgrade: res.FromUpgrade,
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

// CheckDriftTyped — extracted domain function POST /v1/incarnations/{name}/
// check-drift (SELF-AUDIT: the handler writes incarnation.drift_checked ITSELF — the
// drift_summary payload is built after CheckDrift). Parity with (w,r)-CheckDrift: sync
// drift check → 200 + *scenario.DriftReport.
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
				"drift check unavailable for service "+inc.Service+": converge scenario is absent from the current service snapshot")
		}
		if errors.Is(err, scenario.ErrDriftInputMissing) {
			return nil, incProblem(problem.TypeValidationFailed, "drift input does not resolve: "+err.Error())
		}
		h.logger.Error("incarnation.check-drift: failed",
			slog.String("name", name), slog.String("apply_id", applyID), slog.Any("error", err))
		return nil, incProblem(problem.TypeInternalError, "check-drift failed")
	}

	hasDrift := report.Summary.HostsDrifted > 0 || report.Summary.HostsFailed > 0
	if err := h.drift.MarkDriftStatus(ctx, name, hasDrift); err != nil {
		h.logger.Warn("incarnation.check-drift: drift status not recorded",
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

// --- Destroy (SELF-AUDIT incarnation.destroy_started — written by the service layer) ---

// IncarnationDestroyView — FLAT domain projection of the 202 body of DELETE /v1/incarnations/{name}
// (handler-native). Package api projects it into native IncarnationDestroyReply.
type IncarnationDestroyView struct {
	ApplyID string
}

// DestroyTyped — extracted domain function DELETE /v1/incarnations/{name}
// (SELF-AUDIT: destroy_started is written by [incarnation.Destroy] / destroy_completed by
// [DeleteAfterTeardown]; audit middleware is NOT wired). Parity with (w,r)-Destroy: force is
// allow_destroy (path-bind on the huma layer). 202 + apply_id.
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

// IncarnationSpecHostInput — NATIVE hosts[] element of PATCH .../hosts (handler-native). Replaces
// IncarnationSpecHost: SID + optional Role (empty = not set).
type IncarnationSpecHostInput struct {
	SID  string
	Role string
}

// UpdateHostsTyped — extracted domain function PATCH /v1/incarnations/{name}/hosts
// (SELF-AUDIT: the handler writes incarnation.hosts_updated ITSELF — old/new snapshot payload
// after UpdateHosts). Parity with (w,r)-UpdateHosts: three modes over the declared spec.hosts[] →
// 200 + a full IncarnationGetView. mode/items arrive as arguments (native, bound
// on the huma layer).
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

	// schema-aware masking of spec/state in the update-hosts reply (the same detailed view).
	schema := h.secretSchemaForIncarnation(ctx, res.Incarnation)
	return toIncarnationGetView(res.Incarnation, schema), nil
}

// --- SetTraits (SELF-AUDIT incarnation.traits_changed) ----------------

// SetTraitsTyped — extracted domain function PUT /v1/incarnations/{name}/traits
// (SELF-AUDIT: the handler writes incarnation.traits_changed ITSELF — old/new keys payload
// after UpdateTraits). Mirror of the per-soul bulk replace, but on the source of truth:
// replaces incarnation.traits entirely → persist (one tx FOR UPDATE) → projection into
// souls.traits of member hosts ([incarnation.SyncTraitsToHosts]) → 200 + a full
// IncarnationGetView. traits arrives as an argument (native, bound on the huma layer).
// An invalid set (key/value format, nested) → 422 BEFORE writing. An empty/nil
// map = clear the labels.
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

	// Sync-hook (ADR-060 amend, R1): incarnation.traits → souls.traits of member
	// hosts. Best-effort (logged, does not fail the request): incarnation.traits is already
	// written — the projection converges on the next bind/sync. Full replace, including on
	// an empty map (clear the member hosts' labels).
	if serr := incarnation.SyncTraitsToHosts(ctx, h.db, name, res.Incarnation.Traits); serr != nil {
		h.logger.Warn("incarnation.set-traits: sync traits -> souls failed (best-effort)",
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

	// schema-aware masking of spec/state in the reply (the same detailed view as GET / update-hosts).
	schema := h.secretSchemaForIncarnation(ctx, res.Incarnation)
	return toIncarnationGetView(res.Incarnation, schema), nil
}

// --- Get / List / History (READ, no audit) ---------------------------

// GetTyped — extracted domain function GET /v1/incarnations/{name} (READ).
// inScope is the RBAC scope predicate (ADR-047 S3b-3): out of scope → 404. Extracted from
// (w,r)-Get; the caller (huma layer) performs the scope check via the passed predicate over
// the loaded incarnation, so *http.Request need not be pulled into the domain function.
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
	// seal/declarative (ADR-010 §7.4): materialize the service's secret schema for
	// schema-aware masking of spec/state. Best-effort: nil → fallback to
	// MaskSecrets (vault+regex). single-incarnation detailed view — loading the
	// snapshot is acceptable (unlike List, see observations).
	schema := h.secretSchemaForIncarnation(ctx, inc)
	return toIncarnationGetView(inc, schema), nil
}

// IncarnationListReply — typed envelope of GET /v1/incarnations (handler-native: element is a
// domain IncarnationGetView). Package api projects it into the native envelope incarnationListReply
// via RegisterTypeAlias on sharedapi.PagedResponse[handlers.IncarnationGetView].
type IncarnationListReply = sharedapi.PagedResponse[IncarnationGetView]

// IncarnationListQuery — List parameters bound on the huma layer (typed query).
type IncarnationListQuery struct {
	Offset  int
	Limit   int
	Service string
	Status  string
	Coven   string
	SortBy  string
	SortDir string
	// StateParams — query keys `state.<field>` → their values (predicate filter).
	StateParams map[string][]string
}

// ListTyped — extracted domain function GET /v1/incarnations (READ, typed query).
// scope is resolved from the operator's Purview (ADR-047 S3b-3): fail-closed → empty list.
// CheckPageBounds → 400 (parity ParsePage). Invalid status/coven/state-path/sort →
// 422. resolveScope is the caller's closure (huma layer) over claims + serviceFilter.
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
		// List — bulk view: schema pass-through is NOT applied (per-element snapshot
		// materialization is an unacceptable cost on the read hot path). nil schema →
		// MaskSecrets (vault+regex), BIT-FOR-BIT. Declarative masking is available on the detailed
		// GET/History (see observations: schema-aware List is a separate slice with
		// per-service schema caching).
		replies = append(replies, toIncarnationGetView(inc, nil))
	}
	return IncarnationListReply{Items: replies, Offset: q.Offset, Limit: q.Limit, Total: total}, nil
}

// IncarnationHistoryReply — typed envelope of GET /v1/incarnations/{name}/history (handler-native:
// element is a domain StateHistoryView). Package api projects it into the native envelope
// incarnationHistoryReply via RegisterTypeAlias on sharedapi.PagedResponse[handlers.StateHistoryView].
type IncarnationHistoryReply = sharedapi.PagedResponse[StateHistoryView]

// HistoryTyped — extracted domain function GET /v1/incarnations/{name}/history
// (READ, typed query). existence-probe (404) + scope gate (out of scope → 404, parity
// Get) via the passed inScope predicate. CheckPageBounds → 400; bad apply_id → 400.
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
	// materialize the service's secret schema ONCE per request (history is single-
	// incarnation), reuse it for all entries. Best-effort: nil → MaskSecrets.
	schema := h.secretSchemaForIncarnation(ctx, inc)
	entries := make([]StateHistoryView, 0, len(items))
	for _, e := range items {
		entries = append(entries, toStateHistoryView(e, schema))
	}
	return IncarnationHistoryReply{Items: entries, Offset: offset, Limit: limit, Total: total}, nil
}

// --- Runs (READ) — incarnation runs, per-task view -------------------
//
// GET /v1/incarnations/{name}/runs         — list of runs (rollup of apply_runs).
// GET /v1/incarnations/{name}/runs/{apply} — details of one run (per-host slice,
//                                            the failed task's address = "current job").
//
// scope gate — the same inScope predicate as History/Get (action=history):
// the endpoints are in the incarnation domain, scope is resolved by incarnation. A WHERE on
// incarnation_name in the store layer additionally excludes runs of a different incarnation.

// RunSummaryView — FLAT domain row of the run list. Package api projects
// it into native. Status is the aggregate run status (applying/success/failed/cancelled).
// Incarnation is the run's owner: not returned in the per-incarnation wire (implicit),
// a separate entry field in the global GET /v1/runs.
type RunSummaryView struct {
	ApplyID     string
	Incarnation string
	// Service — service of the owning incarnation (global GET /v1/runs; not returned in the
	// per-incarnation wire). "" if unavailable.
	Service      string
	Scenario     string
	Status       string
	StartedAt    time.Time
	FinishedAt   *time.Time
	StartedByAID *string
}

// RunHostStatusView — FLAT domain row of one host in the run details.
// FailedTaskIdx/FailedPlanIndex/ErrorSummary are filled only on the failed host
// (nil on success/still-running).
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

// RunDetailView — FLAT domain projection of the run details (header + per-host slice).
type RunDetailView struct {
	ApplyID      string
	Scenario     string
	Status       string
	StartedAt    time.Time
	FinishedAt   *time.Time
	StartedByAID *string
	Hosts        []RunHostStatusView
}

// IncarnationRunsReply — typed envelope of GET /v1/incarnations/{name}/runs (handler-native:
// element is a domain RunSummaryView). Package api projects it into the native envelope via
// RegisterTypeAlias on sharedapi.PagedResponse[handlers.RunSummaryView].
type IncarnationRunsReply = sharedapi.PagedResponse[RunSummaryView]

// RunsTyped — domain function GET /v1/incarnations/{name}/runs (READ, typed query).
// existence-probe (404) + scope gate (out of scope → 404, parity History) via inScope.
// CheckPageBounds → 400. Returns a page of runs, newest first.
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

// newRunSummaryView projects the store row [applyrun.RunSummary] into a domain view
// (shared by per-incarnation Runs and the global AllRuns).
func newRunSummaryView(s applyrun.RunSummary) RunSummaryView {
	return RunSummaryView{
		ApplyID:      s.ApplyID,
		Incarnation:  s.Incarnation,
		Service:      s.Service,
		Scenario:     s.Scenario,
		Status:       string(s.Status),
		StartedAt:    s.StartedAt,
		FinishedAt:   s.FinishedAt,
		StartedByAID: s.StartedByAID,
	}
}

// RunDetailTyped — domain function GET /v1/incarnations/{name}/runs/{apply_id}
// (READ). existence-probe (404) + scope gate (out of scope → 404) via inScope; bad
// apply_id → 400; run not found / belongs to a different incarnation → 404.
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

// RunTaskErrorView — error part of a per-host task result (FAILED/TIMED_OUT).
type RunTaskErrorView struct {
	Code    string
	Module  string
	Message string
}

// RunTaskHostView — per-host result of one run task (projection of audit task.executed):
// status + output (register_data) + error. Output/Error are nil if absent.
type RunTaskHostView struct {
	SID    string
	Status string
	Output map[string]any
	Error  *RunTaskErrorView
}

// RunTaskView — plan of one run task (host-invariant name/module/no_log/
// passage) + per-host results. Params are the task's operator input parameters
// (NIM-37 S1b), already masked by the seal-aware mechanism on the write path (persistRunPlan);
// nil for no_log tasks and tasks without params.
type RunTaskView struct {
	PlanIndex int
	Passage   int
	Name      string
	Module    string
	NoLog     bool
	Params    map[string]any
	Hosts     []RunTaskHostView
}

// RunTasksView — flat domain projection of GET .../runs/{apply_id}/tasks: the run
// plan + per-host results joined from audit_log.
type RunTasksView struct {
	Tasks []RunTaskView
}

// RunTasksTyped — domain function GET /v1/incarnations/{name}/runs/{apply_id}/tasks
// (READ, NIM-37): the run task plan (apply_run_plan) + per-host status/output/error
// from audit_log (`task.executed`) joined by plan_index → sid. existence-probe (404) +
// scope gate (out of scope → 404) via inScope; bad apply_id → 400; foreign/non-existent
// run → 404. RBAC — incarnation.history (like RunDetail), NOT audit.read.
//
// A task's hosts[] — ONLY hosts with a result in audit (pending hosts are not included,
// the frontend fills them in). no_log task: output/error.message are suppressed on the write
// path → not returned. The last task.executed on (plan_index, sid) wins (retry).
func (h *IncarnationHandler) RunTasksTyped(ctx context.Context, name, applyID string, inScope func(*incarnation.Incarnation) bool) (RunTasksView, error) {
	var zero RunTasksView

	if !incarnation.ValidName(name) {
		return zero, incProblem(problem.TypeValidationFailed, "path 'name' must match "+incarnation.NamePattern)
	}
	if !audit.IsValidULID(applyID) {
		return zero, incProblem(problem.TypeMalformedRequest, "path 'apply_id' must be a Crockford-base32 ULID (26 chars)")
	}

	if _, err := h.existenceProbeInScope(ctx, name, inScope, "run-tasks"); err != nil {
		return zero, err
	}

	// Scope-guard for the run's ownership by the incarnation: apply_run_plan does not carry
	// incarnation_name — we check via apply_runs (foreign apply_id → 404, parity
	// RunDetail SelectRunDetail).
	ok, err := applyrun.RunExistsForIncarnation(ctx, h.db, applyID, name)
	if err != nil {
		h.logger.Error("incarnation.run-tasks: run-exists probe failed",
			slog.String("name", name), slog.String("apply_id", applyID), slog.Any("error", err))
		return zero, incProblem(problem.TypeInternalError, "get run tasks failed")
	}
	if !ok {
		return zero, incProblem(problem.TypeNotFound, "run "+applyID+" not found")
	}

	plan, err := applyrun.SelectRunPlanByApplyID(ctx, h.db, applyID)
	if err != nil {
		h.logger.Error("incarnation.run-tasks: plan select failed",
			slog.String("name", name), slog.String("apply_id", applyID), slog.Any("error", err))
		return zero, incProblem(problem.TypeInternalError, "get run tasks failed")
	}

	// per-host results from audit (task.executed), grouped plan_index → sid.
	// runTasksAudit=nil (unit without a reader) → plan WITHOUT hosts. The last result on
	// (plan_index, sid) wins (execs are ordered by time, later overwrites).
	byPlanHost := map[int]map[string]RunTaskHostView{}
	if h.runTasksAudit != nil {
		execs, aerr := h.runTasksAudit.SelectTaskExecutions(ctx, applyID)
		if aerr != nil {
			h.logger.Error("incarnation.run-tasks: audit select failed",
				slog.String("name", name), slog.String("apply_id", applyID), slog.Any("error", aerr))
			return zero, incProblem(problem.TypeInternalError, "get run tasks failed")
		}
		for _, e := range execs {
			hostsBySID := byPlanHost[e.PlanIndex]
			if hostsBySID == nil {
				hostsBySID = map[string]RunTaskHostView{}
				byPlanHost[e.PlanIndex] = hostsBySID
			}
			hv := RunTaskHostView{SID: e.SID, Status: e.Status, Output: e.Output}
			if e.Error != nil {
				hv.Error = &RunTaskErrorView{Code: e.Error.Code, Module: e.Error.Module, Message: e.Error.Message}
			}
			hostsBySID[e.SID] = hv
		}
	}

	tasks := make([]RunTaskView, 0, len(plan))
	for _, p := range plan {
		hosts := make([]RunTaskHostView, 0, len(byPlanHost[p.PlanIndex]))
		for _, hv := range byPlanHost[p.PlanIndex] {
			hosts = append(hosts, hv)
		}
		sort.Slice(hosts, func(i, j int) bool { return hosts[i].SID < hosts[j].SID })
		tasks = append(tasks, RunTaskView{
			PlanIndex: p.PlanIndex,
			Passage:   p.Passage,
			Name:      p.Name,
			Module:    p.Module,
			NoLog:     p.NoLog,
			Params:    runPlanParams(p.Params), // S1b: masked params from apply_run_plan (NULL→nil)
			Hosts:     hosts,
		})
	}
	return RunTasksView{Tasks: tasks}, nil
}

// runPlanParams deserializes a task's masked params from the stored jsonb
// (apply_run_plan.params, NIM-37 S1b) into an object for the DTO. The values are ALREADY masked
// on the write path (persistRunPlan) — this is read-only. Empty/NULL (a no_log
// task or a task without params) → nil (omitempty on the wire). Malformed JSON → nil (best-
// effort: one bad row doesn't drop the whole /tasks).
func runPlanParams(raw []byte) map[string]any {
	if len(raw) == 0 {
		return nil
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil
	}
	return m
}

// existenceProbeInScope — shared existence-probe + scope gate for Runs/RunDetail:
// SELECT incarnation, out of scope or absent → a single 404 (parity History). action
// is for the log. Returns the found incarnation (nil → error already returned).
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

// mapCreatePlanError maps a [scenario.ResolveCreatePlan] error to a domain
// *problemError for CreateTyped (R2: shared create-plan resolve with MCP, but its own
// HTTP mapping). Preserves the verbatim 422 details of the original inline code
// (create_scenario_required / create_scenario_invalid / input_invalid /
// validation_failed / assert_failed); the rest (snapshot load/parse, pre-flight eval
// failure) → 500 with a log (the stage-specific text of the original code is collapsed to a single
// generic message — the 500 status is unchanged, the operator sees the generic one).
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

// incProblem — constructor of a domain *problemError for the incarnation *Typed functions
// (instance is empty, the caller huma layer needs no URL). Symmetric with the cadence domain.
func incProblem(typ, detail string) error {
	return &problemError{Details: problem.New(typ, "", detail)}
}
