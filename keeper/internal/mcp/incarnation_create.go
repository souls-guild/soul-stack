package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"

	"github.com/souls-guild/soul-stack/keeper/internal/api/handlers"
	"github.com/souls-guild/soul-stack/keeper/internal/incarnation"
	"github.com/souls-guild/soul-stack/keeper/internal/jwt"
	"github.com/souls-guild/soul-stack/keeper/internal/scenario"
	"github.com/souls-guild/soul-stack/keeper/internal/soul"
	"github.com/souls-guild/soul-stack/shared/audit"
	"github.com/souls-guild/soul-stack/shared/config"
)

// incarnationCreateArgs — arguments for keeper.incarnation.create
// (schemaIncarnationCreateInput): name + service are required, covens / input
// are optional. covens — declared env-Coven labels (passed into the
// incarnation, affect RBAC-scope create); input — parameters for scenario `create`.
type incarnationCreateArgs struct {
	Name    string         `json:"name"`
	Service string         `json:"service"`
	Covens  []string       `json:"covens,omitempty"`
	Input   map[string]any `json:"input,omitempty"`
	// CreateScenario — chooses the starting scenario (multi-create mechanism,
	// Variant A; parity with REST `create_scenario`). Empty-choice contract
	// (Phase 2): service offers create scenarios + empty → create_scenario_required;
	// service has none + empty → bare incarnation (ready without a run). A
	// non-empty name must be in the create set, otherwise validation-failed.
	CreateScenario string `json:"create_scenario,omitempty"`
	// Traits — operator-set trait labels for the incarnation (ADR-060 amend R1,
	// parity with REST CreateTyped field `traits`): key → scalar|list of scalars.
	// Extracted into column incarnation.traits (source of truth) and projected
	// into member souls' souls.traits by the sync hook after insert.
	Traits map[string]any `json:"traits,omitempty"`
}

// assertPreflighter — a narrow local surface of scenario.Runner for the
// pre-flight `assert:` gate (ADR-009/ADR-027 amendment 2026-06-23, form A),
// duck-typing over h.deps.ScenarioRunner. Declared LOCALLY (not imported from
// handlers): mcp doesn't depend on the REST-handler layer, and *scenario.Runner
// satisfies it too. ScenarioStarter fakes without PreflightAssert fail the
// type-assertion, so the gate is a no-op (same as REST).
type assertPreflighter interface {
	PreflightAssert(ctx context.Context, spec scenario.RunSpec) error
}

// incarnationCreateOutput — output of keeper.incarnation.create
// (schemaApplyIDOutputWithIncarnation): apply_id + echo incarnation. ApplyID
// is `*string` (omitempty): absent when lifecycle.auto_create=false (the
// incarnation is created ready without a run), mirroring the nullable
// apply_id in REST IncarnationCreateReply.
type incarnationCreateOutput struct {
	ApplyID     *string `json:"_apply_id,omitempty"`
	Incarnation string  `json:"incarnation"`
}

// callIncarnationCreate — mutating async tool keeper.incarnation.create.
// Parity with REST IncarnationHandler.Create in production mode (runner+
// services required): resolve service git coordinates → insert row
// (status=ready) → async-launch scenario `create` → 202 + apply_id.
//
// MCP has no Create stub mode (REST degrades to insert-only when
// runner==nil, for M0.6c-1 compatibility; MCP tools are rolled out on top of
// a full runner stack): nil ScenarioRunner / ServiceRegistry → internal-error
// "scenario runner is not configured" (mirrors REST Run/Upgrade without deps).
//
// RBAC — body-scoped OR-Check (parity with REST IncarnationCreateScopeSelector
// + RequirePermissionMulti): scope = the declared covens, plus the
// `incarnation=<name>` dimension (ADR-008 amendment 2026-07-17/NIM-124: name is
// NOT a coven). Without this, a coven-scoped operator could bypass REST
// protection via MCP (least-privilege: scope `coven=dev` must NOT create an
// incarnation with covens=[prod]). bare/`*` matches any
// (as before). audit: EventIncarnationCreated {name, service, covens,
// apply_id}, source=mcp.
func (h *Handler) callIncarnationCreate(ctx context.Context, claims *jwt.Claims, req jsonRPCRequest, args json.RawMessage) jsonRPCResponse {
	const toolName = "keeper.incarnation.create"

	var a incarnationCreateArgs
	if len(args) > 0 {
		if err := strictUnmarshal(args, &a); err != nil {
			return h.toolError(req.ID, toolName, mcpCodeMalformedRequest,
				"invalid arguments: "+err.Error())
		}
	}
	// `name` is optional here (ADR-0079, parity with REST CreateTyped): a create
	// scenario carrying a `name_template` composes it from input components, and
	// only the resolved plan knows whether there is one. A non-empty name is
	// format-checked up front; "name is required" is deferred until after the plan.
	if a.Name != "" && !incarnation.ValidName(a.Name) {
		return h.toolError(req.ID, toolName, mcpCodeValidationFailed,
			"field 'name' must match "+incarnation.NamePattern)
	}
	if a.Service == "" {
		return h.toolError(req.ID, toolName, mcpCodeValidationFailed, "field 'service' is required")
	}
	// Sanity-check the service name against the same kebab-case grammar (parity
	// with REST): guards against garbage in the DB (a `/` would break git-resolve paths).
	if !incarnation.ValidName(a.Service) {
		return h.toolError(req.ID, toolName, mcpCodeValidationFailed,
			"field 'service' must match "+incarnation.NamePattern)
	}
	// covens — declared env tags (ADR-008 amendment a): each must match
	// CovenPattern (mirrors soul.Create / REST create). An invalid label →
	// validation-failed before scope-check/insert.
	for _, label := range a.Covens {
		if !soul.ValidCoven(label) {
			return h.toolError(req.ID, toolName, mcpCodeValidationFailed,
				"coven label "+label+" must match "+soul.CovenPattern)
		}
	}

	// Body-scoped RBAC BEFORE creation (fail-closed): deny → no audit, no
	// insert, no scenario-start. Contexts come from the same
	// handlers.IncarnationCreateContexts as REST (single source of truth), which
	// scopes on `service=`/`coven=` when `name` is absent under a `name_template`
	// (NIM-333) instead of admitting only unrestricted roles.
	if err := h.checkIncarnationCreateScope(claims, a.Name, a.Service, a.Covens); err != nil {
		return h.toolError(req.ID, toolName, mcpCodeForbidden,
			"operator lacks required permission incarnation.create")
	}

	// runner / services are required to run scenario `create`.
	if h.deps.ScenarioRunner == nil || h.deps.ServiceRegistry == nil {
		return h.toolError(req.ID, toolName, mcpCodeInternalError,
			"scenario runner is not configured")
	}

	serviceRef, ok := h.deps.ServiceRegistry.Resolve(a.Service)
	if !ok {
		return h.toolError(req.ID, toolName, mcpCodeValidationFailed,
			"service "+a.Service+" is not registered (manage via service.* API, ADR-029)")
	}

	// Shared resolve of the starting scenario + sync input validation +
	// pre-flight-assert (R2: unified scenario.ResolveCreatePlan with REST
	// CreateTyped). nil loader → stub plan (`create`, not bare, auto_create=true;
	// in production the MCP loader is always present). preflighter is
	// h.deps.ScenarioRunner (type-asserted to scenario.AssertPreflighter inside;
	// a ScenarioStarter fake without the method is a no-op).
	//
	// createScenario — the starting scenario (multi-create mechanism);
	// bareNoScenario — service without `create: true` (ready without a run,
	// created_scenario=NULL); autoCreate — lifecycle.auto_create policy
	// (default true): false → ready without a run, but created_scenario is non-empty.
	plan, perr := scenario.ResolveCreatePlan(ctx, h.deps.ServiceLoader, h.deps.ScenarioRunner, a.Name, serviceRef, a.CreateScenario, a.Input, claims.Subject)
	if perr != nil {
		return h.createPlanToolError(req, toolName, a.Name, a.Service, perr)
	}
	createScenario := plan.CreateScenario
	bareNoScenario := plan.BareNoScenario
	autoCreate := plan.AutoCreate
	// name — the EFFECTIVE incarnation name (ADR-0079): the operator's `name`, or
	// the one the create scenario composed from `name_template`. Everything past
	// this point uses it, never a.Name.
	name := plan.EffectiveName(a.Name)
	if name == "" {
		return h.toolError(req.ID, toolName, mcpCodeValidationFailed, "field 'name' is required")
	}

	// Gate (b), mirroring REST CreateTyped (NIM-333/NIM-338): the effective name and
	// EVERY declared coven are measured against the caller's scope, all-or-nothing.
	// Every create, named or composed, and AFTER the "name is required" check above
	// for the same reason REST orders it that way.
	//
	// Gate (a) is an OR over the declared covens, so a request naming one coven the
	// caller holds and one it does not was created carrying BOTH — and an
	// incarnation's covens become labels on its member hosts (ADR-0080), so that
	// placed hosts in a coven the caller cannot reach.
	if err := handlers.ScreenIncarnationCreateScope(h.deps.RBAC, claims.Subject,
		name, a.Service, a.Covens); err != nil {
		return h.toolError(req.ID, toolName, mcpCodeForbidden,
			"incarnation.create denied: "+name+
				" or a declared coven is outside your scope (the request is refused whole, not trimmed)")
	}

	// Write spec.input only when input is non-empty — otherwise scenario-runner
	// would see `"input": null` (key present) instead of "operator didn't pass
	// input", which CEL can't distinguish (parity with REST).
	spec := map[string]any{}
	if a.Input != nil {
		spec["input"] = a.Input
	}
	if a.Traits != nil {
		spec["traits"] = a.Traits
	}

	// Trait per-incarnation (ADR-060 amend R1, parity with REST CreateTyped):
	// operator-set traits from spec.traits are extracted into column
	// incarnation.traits (source of truth, projected into souls.traits). An
	// invalid set (key/value format) → validation-failed BEFORE insert.
	traits, err := incarnation.TraitsFromSpec(spec)
	if err != nil {
		return h.toolError(req.ID, toolName, mcpCodeValidationFailed, err.Error())
	}

	// createScenarioCol — NULL for a bare incarnation (no bootstrap scenario),
	// otherwise a pointer to the chosen name (parity with REST). autoCreate=false
	// does NOT make it NULL — the bootstrap scenario exists, the run is merely deferred.
	var createScenarioCol *string
	if !bareNoScenario {
		createScenarioCol = &createScenario
	}

	creator := claims.Subject
	inc := &incarnation.Incarnation{
		Name:               name,
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
				"incarnation "+name+" already exists")
		}
		h.deps.Logger.Error("mcp: incarnation.create insert failed",
			slog.String("name", name),
			slog.String("service", a.Service),
			slog.String("by_aid", claims.Subject),
			slog.Any("error", err),
		)
		return h.toolError(req.ID, toolName, mcpCodeInternalError, "insert incarnation failed")
	}

	// No projection onto member hosts (ADR-080, parity with REST CreateTyped):
	// the labels stay on the incarnation and reach hosts by inheritance at read
	// time, covering hosts that join later.

	// apply_id is generated only when the bootstrap run starts (runCreate). bare
	// (no create scenario) OR auto_create=false → the incarnation stays ready
	// without a run, apply_id is empty (omitted in the response). runScenario
	// isn't needed here (unlike REST incarnation_typed.go): a nil runner is
	// rejected at entry (lines ~117-120), so MCP only reaches this point with a
	// live ScenarioRunner.
	runCreate := !bareNoScenario && autoCreate
	var applyID string
	if runCreate {
		applyID = audit.NewULID()
		if err := h.deps.ScenarioRunner.Start(ctx, scenario.RunSpec{
			ApplyID:         applyID,
			IncarnationName: name,
			ServiceRef:      serviceRef,
			ScenarioName:    createScenario,
			Input:           a.Input,
			StartedByAID:    claims.Subject,
		}); err != nil {
			// Row already inserted (status=ready), the run didn't start. Log it;
			// internal-error so the operator retries (parity with REST 500).
			h.deps.Logger.Error("mcp: incarnation.create scenario start failed",
				slog.String("name", name),
				slog.String("apply_id", applyID),
				slog.Any("error", err),
			)
			return h.toolError(req.ID, toolName, mcpCodeInternalError, "start scenario create failed")
		}
	}

	// covens — coalesce nil→[] (parity with REST: the audit field is always an
	// array, never null). apply_id in audit is null when auto_create=false.
	auditCovens := a.Covens
	if auditCovens == nil {
		auditCovens = []string{}
	}
	var auditApplyID any
	out := incarnationCreateOutput{Incarnation: name}
	if applyID != "" {
		auditApplyID = applyID
		out.ApplyID = &applyID
	}
	h.writeAudit(audit.EventIncarnationCreated, claims.Subject, map[string]any{
		"name":     name,
		"service":  a.Service,
		"covens":   auditCovens,
		"apply_id": auditApplyID,
	})

	return h.toolResult(req.ID, out)
}

// createPlanToolError maps a [scenario.ResolveCreatePlan] error to the
// jsonRPCResponse for keeper.incarnation.create (R2: shared create-plan
// resolve with REST CreateTyped, but its own MCP mapping). Preserves the
// verbatim detail prefixes of the original inline code (create_scenario_required
// / create_scenario_invalid / input_invalid / validation_failed / assert_failed
// → all map to validation-failed; assert has NO separate MCP code — parity
// with prior behavior, unlike REST TypeAssertFailed); everything else
// (snapshot load/parse, eval failure) → internal-error, logged.
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
	// Name composition (ADR-0079, parity with REST mapCreatePlanError): operator
	// error → validation-failed, same detail prefixes.
	case errors.Is(err, scenario.ErrNameNotComposable):
		return h.toolError(req.ID, toolName, mcpCodeValidationFailed, "name_not_composable: "+err.Error())
	case errors.Is(err, scenario.ErrComposedNameInvalid):
		return h.toolError(req.ID, toolName, mcpCodeValidationFailed, "composed_name_invalid: "+err.Error())
	case errors.Is(err, config.ErrNameTemplateRender):
		return h.toolError(req.ID, toolName, mcpCodeValidationFailed, "name_template_failed: "+err.Error())
	}
	h.deps.Logger.Error("mcp: incarnation.create resolve create plan failed",
		slog.String("name", name),
		slog.String("service", service),
		slog.Any("error", err),
	)
	return h.toolError(req.ID, toolName, mcpCodeInternalError, "resolve create plan failed")
}
