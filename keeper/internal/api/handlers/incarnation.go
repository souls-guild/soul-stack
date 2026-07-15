package handlers

// T5d-2c-full (handler-native): the incarnation domain is decoupled from the legacy generator.
// Per-route business logic lives in *Typed functions (incarnation_typed.go) returning FLAT domain
// view structs (incarnation_view.go); package api binds huma input / projects the native reply.
// The former (w,r) layer (thin strict wrappers) and legacy-generator converters are gone.
//
// This file carries: handler dependencies (interfaces + constructor), shared RBAC-scope helpers
// (ADR-047 S3b-3) — GetInScopeFor/ResolveListScopeFor factories + state-CEL dimension resolution,
// host-role/state-predicate validation, and route RBAC scope selectors (router.go + MCP parity).

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"regexp"
	"strings"
	"sync"

	"github.com/go-chi/chi/v5"

	"github.com/souls-guild/soul-stack/keeper/internal/api/middleware"
	"github.com/souls-guild/soul-stack/keeper/internal/artifact"
	"github.com/souls-guild/soul-stack/keeper/internal/auditpg"
	"github.com/souls-guild/soul-stack/keeper/internal/incarnation"
	"github.com/souls-guild/soul-stack/keeper/internal/jwt"
	"github.com/souls-guild/soul-stack/keeper/internal/rbac"
	"github.com/souls-guild/soul-stack/keeper/internal/scenario"
	"github.com/souls-guild/soul-stack/keeper/internal/statemigrate"
	"github.com/souls-guild/soul-stack/keeper/internal/statepredicate"
	"github.com/souls-guild/soul-stack/shared/audit"
)

// IncarnationDB — narrow surface over pgxpool.Pool for incarnation CRUD
// operations. Combines [incarnation.ExecQueryRower] (Create / Get / List /
// History) and [incarnation.TxBeginner] (Unlock — atomic FOR UPDATE → mutate →
// commit). A real `*pgxpool.Pool` satisfies it automatically; unit tests pass a
// fake.
type IncarnationDB interface {
	incarnation.ExecQueryRower
	incarnation.TxBeginner
}

// ScenarioStarter — narrow surface of scenario.Runner needed by the Create
// handler: async start of a scenario run. An interface (not *scenario.Runner)
// so handler unit tests don't stand up the whole runner stack.
type ScenarioStarter interface {
	Start(ctx context.Context, spec scenario.RunSpec) error
}

// AssertPreflighter — narrow surface of scenario.Runner for the `assert:`
// pre-flight gate (ADR-009/ADR-027 amendment 2026-06-23, form A): synchronous
// evaluation of a scenario's assert predicates at run CREATION (request path,
// BEFORE the incarnation commit). Implemented by *scenario.Runner (PreflightAssert
// method); the Create handler picks it up optionally via type assertion on the
// runner, so test ScenarioStarter fakes without this method keep working
// (pre-flight is then skipped — a no-op, as in M0.6c-1 stub mode without a runner).
//
// Returns scenario.ErrAssertFailed on a predicate failure (handler → 422
// assert_failed); other errors are an internal pre-flight failure (handler → 500).
type AssertPreflighter interface {
	PreflightAssert(ctx context.Context, spec scenario.RunSpec) error
}

// DestroyStarter — narrow surface of scenario.Runner needed by the Destroy
// handler: async start of the `destroy` teardown scenario run in TerminalDestroy
// mode (S-D2b). A separate interface from [ScenarioStarter]: Destroy uses
// StartDestroy (pins ScenarioName=destroy + TerminalMode), not Start. A real
// *scenario.Runner satisfies both.
type DestroyStarter interface {
	StartDestroy(ctx context.Context, spec scenario.RunSpec) error
}

// DriftChecker — narrow surface of scenario.Runner for the check-drift handler
// (ADR-031, Slice B). CheckDrift is sync (not async, unlike Start/StartDestroy):
// the handler blocks until the DriftReport is assembled so it can return it to the
// operator in the 200 response. MarkDriftStatus is a post-check informational mark
// on incarnation.status (drift/ready). A real *scenario.Runner satisfies it.
type DriftChecker interface {
	CheckDrift(ctx context.Context, spec scenario.CheckDriftSpec) (*scenario.DriftReport, error)
	MarkDriftStatus(ctx context.Context, name string, hasDrift bool) error
}

// ServiceResolver resolves the git coordinates of a service repo by service name
// (`incarnation.service` → service registry in the DB, ADR-029). Used by the
// Create handler when starting the `create` scenario.
type ServiceResolver interface {
	Resolve(service string) (artifact.ServiceRef, bool)
}

// ServiceSnapshotLoader — narrow surface of [artifact.ServiceLoader] needed by the
// Upgrade and Destroy handlers: materialize a snapshot of the target service-ref
// (to read `state_schema_version` from its `service.yml`), assemble the
// state_schema migration chain current→target (Upgrade), and read snapshot files
// (Destroy pre-check for a `destroy` scenario, [incarnation.PrepareDestroy]). An
// interface (not *artifact.ServiceLoader) so handler unit tests don't stand up the
// git stack. ReadFile aligns the contract with [incarnation.DestroyScenarioReader]
// (Load + ReadFile) — *artifact.ServiceLoader satisfies both.
type ServiceSnapshotLoader interface {
	Load(ctx context.Context, ref artifact.ServiceRef) (*artifact.ServiceArtifact, error)
	LoadMigrationChain(art *artifact.ServiceArtifact, from, to int) (statemigrate.Chain, error)
	ReadFile(art *artifact.ServiceArtifact, file string) ([]byte, error)
	// ListUpgrades — scan upgrade/<slug>/ of the target snapshot (ADR-0068): needed
	// by the Upgrade handler so incarnation.PrepareUpgrade resolves found/legacy.
	ListUpgrades(art *artifact.ServiceArtifact) ([]artifact.Scenario, error)
}

// IncarnationHandler — handlers for incarnation endpoints:
// Create / Get / List / History.
//
// runner / services are optional: with nil, Create degrades to M0.6c-1 stub mode
// (insert row with status=ready, no scenario start). Production wire-up
// (`keeper run`) passes both — Create inserts the row and starts the `create`
// scenario, which itself moves the incarnation applying → ready / error_locked.
//
// loader is optional: needed only by the Upgrade handler (materialize the target
// service-ref snapshot + assemble the migration chain). With nil, Upgrade returns
// 500 (endpoint not configured), symmetric to Run without a runner.
//
// The pool for UpgradeStateSchema is `db` itself: IncarnationDB embeds
// [incarnation.TxBeginner], so no separate dependency is required.
//
// destroyer / auditW are for the Destroy handler (S-D4): destroyer starts the
// teardown run (StartDestroy), auditW is passed to [incarnation.Destroy] /
// [incarnation.DeleteAfterTeardown] to record destroy_started / destroy_completed
// (the service layer writes audit, not the permission middleware — see router.go).
// Both allow nil: without destroyer+services+loader, Destroy returns 500 (endpoint
// not configured, symmetric to Run/Upgrade); auditW=nil → the destroy trail is not
// written (acceptable in unit tests).
//
// Dependencies are immutable after wire-up (refs — late-binding via [SetServiceRefs]
// before the HTTP server starts); safe for concurrent use.
type IncarnationHandler struct {
	db        IncarnationDB
	runner    ScenarioStarter
	destroyer DestroyStarter
	drift     DriftChecker
	services  ServiceResolver
	loader    ServiceSnapshotLoader
	auditW    audit.Writer
	scoper    PurviewResolver
	logger    *slog.Logger

	// refs — ls-remote of the service registry's tags/branches, needed ONLY by the
	// cheap UpgradePathsTyped mode (ADR-0068 §6, enumerating upgrade targets). The
	// same [ServiceRefsLister] as ServiceHandler — no duplicate ls-remote. Injected
	// late-binding via [SetServiceRefs] (the constructor has 143 call sites, not
	// worth widening; pattern [OperatorHandler.SetProvisioningGate]); nil → the cheap
	// mode returns 500 (not configured). On-demand ?to= does not use refs.
	refs ServiceRefsLister

	// runTasksAudit — read-side task.executed for RunTasksTyped (per-host run task
	// totals, NIM-37). Injected late-binding via [SetRunTasksAuditReader] (same
	// motive as refs). nil → /tasks returns the plan without per-host results (unit
	// without an audit reader).
	runTasksAudit RunTaskAuditReader

	// vault — read surface of Vault KV for the secret reveal endpoint (NIM-74).
	// Injected late-binding via [SetVaultReader] (same motive as refs). nil →
	// RevealSecretTyped returns 404 (secret not revealable — endpoint not configured).
	vault VaultKVReader
}

// VaultKVReader — narrow read surface of Vault KV for the reveal endpoint (NIM-74):
// resolve a plaintext secret by logical path. keeper/internal/vault.Client
// satisfies it as-is; the narrowing gives a hermetic unit run with a fixture reader.
type VaultKVReader interface {
	ReadKV(ctx context.Context, path string) (map[string]any, error)
}

// RunTaskAuditReader — narrow read surface of audit_log for RunTasksTyped:
// per-host run task totals (`task.executed`) joined by plan_index (NIM-37).
// *auditpg.Reader satisfies it.
type RunTaskAuditReader interface {
	SelectTaskExecutions(ctx context.Context, applyID string) ([]auditpg.TaskExecution, error)
}

// NewIncarnationHandler builds the handler. runner / destroyer / drift / services /
// loader / auditW allow nil: without runner+services Create degrades to the stub,
// without loader Upgrade returns 500, without destroyer+services+loader Destroy
// returns 500, without drift+services CheckDrift returns 500, without auditW the
// destroy/drift trail is not written.
//
// scoper — read surface of the operator's scope boundary ([PurviewResolver],
// production wire-up passes rbac.Holder) for scoped List/Get visibility
// (ADR-047 S3b-3, coven∪{name} + state-CEL Purview dimensions). nil is allowed
// only in tests that don't use List/Get scope: List with a nil scoper is
// fail-closed (empty list — the safe default, NOT all incarnations), Get with a
// nil scoper is fail-closed (404 — don't leak another's incarnation).
func NewIncarnationHandler(db IncarnationDB, runner ScenarioStarter, destroyer DestroyStarter, drift DriftChecker, services ServiceResolver, loader ServiceSnapshotLoader, auditW audit.Writer, scoper PurviewResolver, logger *slog.Logger) *IncarnationHandler {
	if logger == nil {
		logger = slog.New(slog.NewJSONHandler(io.Discard, nil))
	}
	return &IncarnationHandler{db: db, runner: runner, destroyer: destroyer, drift: drift, services: services, loader: loader, auditW: auditW, scoper: scoper, logger: logger}
}

// SetServiceRefs late-binds the refs lister (ls-remote of the service registry's
// tags) for the cheap [UpgradePathsTyped] mode (ADR-0068 §6). A separate setter
// rather than a 10th positional constructor arg: NewIncarnationHandler is called
// from 140+ sites (mostly tests with nil deps), so widening the signature would
// bloat the diff for no gain (pattern [OperatorHandler.SetProvisioningGate]).
// Called once in `keeper run` before the HTTP server starts; nil → cheap
// upgrade-paths returns 500. No thread safety needed (called before serving).
func (h *IncarnationHandler) SetServiceRefs(refs ServiceRefsLister) {
	h.refs = refs
}

// SetRunTasksAuditReader late-binds the read-side audit_log for [RunTasksTyped]
// (per-host run task totals, NIM-37). A separate setter, not a constructor arg
// (same motive as [SetServiceRefs]); nil → /tasks returns the plan without per-host
// results. Called once in `keeper run` before the server starts.
func (h *IncarnationHandler) SetRunTasksAuditReader(r RunTaskAuditReader) {
	h.runTasksAudit = r
}

// SetVaultReader late-binds the Vault KV reader for the secret reveal endpoint
// (NIM-74). A separate setter, not a constructor arg (same motive as
// [SetServiceRefs]); nil → RevealSecretTyped returns 404 (endpoint not
// configured). Called once in `keeper run` before the server starts.
func (h *IncarnationHandler) SetVaultReader(v VaultKVReader) {
	h.vault = v
}

// ContextReader returns the handler's DB read surface for the RBAC extractor
// [IncarnationScopeSelector] (landing an existing incarnation's service/covens
// into the permission context). `db` itself is [IncarnationDB], which embeds
// [incarnation.ExecQueryRower]; the extractor needs only the read part. nil when
// db=nil (stub construction in the drift test — the extractor isn't called there).
func (h *IncarnationHandler) ContextReader() IncarnationContextReader {
	if h.db == nil {
		return nil
	}
	return h.db
}

// --- host-role validation (PATCH .../hosts) ----------------------------

// hostsRolePattern — kebab-case role label (lowercase + hyphens), 1..63 chars.
// The declared role is an operator-asserted string from `incarnation.spec.hosts[].role`
// (ADR-008): values are not predefined in code (master/replica are common but not
// exhaustive), so we validate only the shape, like Coven labels (same kebab-case
// invariant, no conflict with the scenario-on: grammar).
const hostsRolePattern = `^[a-z][a-z0-9]*(-[a-z0-9]+)*$`

var hostsRoleRe = regexp.MustCompile(hostsRolePattern)

func validHostRole(role string) bool {
	if role == "" {
		return true
	}
	if len(role) > 63 {
		return false
	}
	return hostsRoleRe.MatchString(role)
}

// specHostsToPayload — a snapshot of hosts[] for the audit payload. Symmetric to
// the jsonb form of `spec.hosts` (see [incarnation.readSpecHosts]).
func specHostsToPayload(hosts []incarnation.SpecHost) []map[string]any {
	out := make([]map[string]any, 0, len(hosts))
	for _, h := range hosts {
		obj := map[string]any{"sid": h.SID}
		if h.Role != "" {
			obj["role"] = h.Role
		}
		out = append(out, obj)
	}
	return out
}

// --- RBAC scope (ADR-047 S3b-3) ---------------------------------------

// incStateResolver — shared statepredicate.Resolver for scoped List/Get (the
// state-CEL Purview dimension). Does NOT duplicate the CEL engine: statepredicate
// delegates to shared/cel (migration sandbox, root `state`). One resolver per
// process (thread-safe, shared compile cache) — lazily once, like rbac.stateResolver.
var (
	incStateResolverOnce sync.Once
	incStateResolverInst statepredicate.Resolver
	incStateResolverErr  error
)

func incStateResolver() (statepredicate.Resolver, error) {
	incStateResolverOnce.Do(func() {
		incStateResolverInst, incStateResolverErr = statepredicate.New()
	})
	return incStateResolverInst, incStateResolverErr
}

// resolveStateNames returns the names of incarnations whose state satisfies the
// combined OR predicate of the state-CEL scope (StateExprs joined into `p1 || ...`).
// Reuses statepredicate.ResolveIncarnations over incarnation.StateLister
// (page-by-page pushdown). serviceFilter narrows the CEL-eval set to the same
// service (BaseFilter pushdown) as the main query.
func (h *IncarnationHandler) resolveStateNames(ctx context.Context, exprs []string, serviceFilter string) ([]string, error) {
	resolver, err := incStateResolver()
	if err != nil {
		return nil, fmt.Errorf("incarnation: state-scope CEL engine: %w", err)
	}
	combined := joinStateExprs(exprs)
	lister := incarnation.NewStateLister(h.db)
	return resolver.ResolveIncarnations(ctx, combined, statepredicate.BaseFilter{Service: serviceFilter}, lister)
}

// joinStateExprs joins several state-CEL predicates into one OR expression
// `(p1) || (p2) || ...` (union within a dimension — "available by any of them"). A
// single predicate is returned as-is. Each is wrapped in parens: a predicate was
// validated at snapshot load as a standalone bool expression, and the parens
// preserve its boundary when joined (precedence of `||`).
func joinStateExprs(exprs []string) string {
	if len(exprs) == 1 {
		return exprs[0]
	}
	parts := make([]string, len(exprs))
	for i, e := range exprs {
		parts[i] = "(" + e + ")"
	}
	return strings.Join(parts, " || ")
}

// scopeEmpty — true for a fail-closed Purview: not Unrestricted and no dimension
// meaningful for incarnation scope (Covens / StateExprs / TraitExprs) is set.
// regex/soulprint are soul facts, NOT applied to incarnations (S3b-3 spec), so they
// don't count as "set dimensions" here: a Purview with only soulprint/regex (no
// coven/state/trait) for incarnation scope is empty (nothing to match) →
// fail-closed. Deny (S2 stub) is treated as fail-closed.
func scopeEmpty(pv rbac.Purview) bool {
	if pv.Deny {
		return true
	}
	return len(pv.Covens) == 0 && len(pv.StateExprs) == 0 && len(pv.TraitExprs) == 0
}

// statePredicateOps — allowed operator prefixes in the query value
// `state.<field>=<op>:<value>`. Maps the operator-facing name → [incarnation.StateOp].
var statePredicateOps = map[string]incarnation.StateOp{
	"eq":  incarnation.StateOpEq,
	"ne":  incarnation.StateOpNe,
	"gt":  incarnation.StateOpGt,
	"gte": incarnation.StateOpGte,
	"lt":  incarnation.StateOpLt,
	"lte": incarnation.StateOpLte,
}

// parseStatePredicatesFromMap — request-free parser of state predicates for
// FULL-TYPED ListTyped (the huma layer binds the typed query, the domain function
// never sees *http.Request). Takes an already-assembled map `state.<field>` →
// values (the caller filters the query by prefix). The field part is validated by
// the format whitelist [statePathQueryPattern] — an invalid path/op → error (handler
// maps to 422), so injection into a jsonb identifier never reaches CRUD/DB. Only the
// first value of each key (multi-value on the same state key is not MVP). Returns nil
// with no error when there are no state filters.
func parseStatePredicatesFromMap(stateParams map[string][]string) ([]incarnation.StateEq, error) {
	var preds []incarnation.StateEq
	for field, vals := range stateParams {
		if !statePathQueryPattern.MatchString(field) {
			return nil, fmt.Errorf("query 'state.%s': field must match [a-z][a-z0-9_]*", field)
		}
		raw := ""
		if len(vals) > 0 {
			raw = vals[0]
		}
		op := incarnation.StateOpEq
		value := raw
		if prefix, rest, found := strings.Cut(raw, ":"); found {
			mapped, known := statePredicateOps[prefix]
			if !known {
				return nil, fmt.Errorf("query 'state.%s': unknown operator %q (eq/ne/gt/gte/lt/lte)", field, prefix)
			}
			op = mapped
			value = rest
		}
		preds = append(preds, incarnation.StateEq{Path: field, Op: op, Value: value})
	}
	return preds, nil
}

// statePathQueryPattern duplicates the CRUD-side whitelist (a format reject at the
// handler level → a clean 422 without a round-trip). The source of truth is the
// CRUD-side validation in SelectAll (defense in depth).
var statePathQueryPattern = regexp.MustCompile(`^[a-z][a-z0-9_]*$`)

// GetInScopeFor — request-free factory of a scope predicate for FULL-TYPED GetTyped/
// HistoryTyped (huma layer). claims/action arrive explicitly (instead of reading
// from *http.Request). Semantics: Unrestricted → true; empty/Deny Purview → false;
// coven∪{name} OR a state-CEL match → true. nil claims/nil scoper → the predicate is
// always false (fail-closed).
func (h *IncarnationHandler) GetInScopeFor(claims *jwt.Claims, action string) func(*incarnation.Incarnation) bool {
	return func(inc *incarnation.Incarnation) bool {
		if claims == nil || h.scoper == nil {
			return false
		}
		pv := h.scoper.ResolvePurview(claims.Subject, "incarnation", action)
		if pv.Unrestricted {
			return true
		}
		if scopeEmpty(pv) {
			return false
		}
		for _, sc := range pv.Covens {
			if sc == inc.Name {
				return true
			}
			for _, c := range inc.Covens {
				if c == sc {
					return true
				}
			}
		}
		for _, expr := range pv.StateExprs {
			matched, err := rbac.EvalStateExpr(expr, inc.State)
			if err != nil {
				h.logger.Warn("incarnation.get: state-CEL eval упал — предикат не даёт доступ",
					slog.String("name", inc.Name), slog.Any("error", err))
				continue
			}
			if matched {
				return true
			}
		}
		// trait dimension (ADR-047 amendment, ADR-060 §7 slice 1): the scope pair
		// `key:value` grants access if incarnation.traits[key] == value (scalar).
		for _, pair := range pv.TraitExprs {
			key, value, ok := splitTraitPair(pair)
			if !ok {
				continue
			}
			if traitScalarEquals(inc.Traits, key, value) {
				return true
			}
		}
		return false
	}
}

// splitTraitPair splits a trait-scope string `key:value` (normalized by
// [rbac.parseTraitValue] — exactly one `:`, non-empty halves) into key and value.
// ok=false when `:` is absent (defensive against parser drift).
func splitTraitPair(pair string) (key, value string, ok bool) {
	return strings.Cut(pair, ":")
}

// traitScalarEquals — true if traits[key] is a scalar whose string form equals
// value (slice 1 is scalar-only trait scope). A list Trait isn't covered by a single
// equality (follow-up), so non-scalar → false. fmt.Sprint gives the canonical string
// for string/number/bool (jsonb numbers arrive as float64/json.Number).
func traitScalarEquals(traits map[string]any, key, value string) bool {
	v, ok := traits[key]
	if !ok {
		return false
	}
	switch v.(type) {
	case string, float64, bool, json.Number, int, int64:
		return fmt.Sprint(v) == value
	default:
		// map / slice (list Trait) — not a scalar match (slice 1 doesn't cover it).
		return false
	}
}

// ResolveListScopeFor — request-free factory of a scope resolver for FULL-TYPED
// ListTyped (huma layer). Semantics: fail-closed on nil claims/nil scoper/Empty
// Purview (the caller returns an empty list); Unrestricted → the whole list;
// otherwise coven∪{name} pushdown ∪ pre-resolved names by state-CEL.
func (h *IncarnationHandler) ResolveListScopeFor(ctx context.Context, claims *jwt.Claims) func(serviceFilter string) (incarnation.ListScope, bool) {
	return func(serviceFilter string) (incarnation.ListScope, bool) {
		return h.resolveListScope(ctx, claims, "list", serviceFilter)
	}
}

// resolveListScope — shared Purview→[incarnation.ListScope] resolution for
// list-like reads (List action=list; global runs/stats action=history). Semantics
// as in [IncarnationHandler.ResolveListScopeFor] (same fail-closed boundary).
func (h *IncarnationHandler) resolveListScope(ctx context.Context, claims *jwt.Claims, action, serviceFilter string) (incarnation.ListScope, bool) {
	if claims == nil || h.scoper == nil {
		return incarnation.ListScope{}, false
	}
	pv := h.scoper.ResolvePurview(claims.Subject, "incarnation", action)
	if pv.Unrestricted {
		return incarnation.ListScope{Unrestricted: true}, true
	}
	if scopeEmpty(pv) {
		return incarnation.ListScope{}, false
	}
	scope := incarnation.ListScope{Covens: pv.Covens}
	// state dimension fail-OPEN: resolution failed → don't extend the output with
	// its names, but the coven dimension stays in force (don't drop the whole List).
	// Log it.
	if len(pv.StateExprs) > 0 {
		names, err := h.resolveStateNames(ctx, pv.StateExprs, serviceFilter)
		if err != nil {
			h.logger.Warn("incarnation."+action+": state-scope резолв упал — применяется только coven-измерение (fail-closed по state)",
				slog.String("aid", claims.Subject), slog.Any("error", err))
		} else {
			scope.StateNames = names
		}
	}
	// trait dimension (ADR-047 amendment, ADR-060 §7 slice 1): scope pairs
	// `key:value` → SQL pushdown `traits->>$key = $value` (scalar equality, no
	// CEL/resolution; NOT containment `@>` — BUG#1 fix, [incarnation.appendScopeClause]).
	// A broken pair (parser drift) is skipped, doesn't drop List.
	for _, pair := range pv.TraitExprs {
		key, value, ok := splitTraitPair(pair)
		if !ok {
			continue
		}
		scope.Traits = append(scope.Traits, incarnation.TraitPair{Key: key, Value: value})
	}
	return scope, true
}

// --- RBAC route scope selectors ---------------------------------------

// IncarnationContextReader — read surface for the RBAC extractors of incarnation
// routes: "return service + declared covens of the incarnation by name".
// Implemented by [IncarnationDB] (via [incarnation.SelectByName]); the extractor
// holds it in a closure to land the incarnation's own scope attributes into the
// RBAC context (ADR-008 amendment a; architect: the context is one-dimensional —
// incarnation attributes, not bulk over hosts, so accessing the data in the
// extractor is cleaner than moving the check into the handler).
type IncarnationContextReader interface {
	incarnation.ExecQueryRower
}

// incarnationCovenContexts expands an incarnation's coven scope into a set of
// per-candidate RBAC contexts for [middleware.RequirePermissionMulti].
//
// Effective coven scope = covens ∪ {name} (declared env tags + name as the root
// Coven label, ADR-008). Each candidate → a separate context
// `{incarnation, service, coven=<candidate>}`; the permission check ORs them.
// service is put into ALL contexts (a service-only permission matches on any
// coven iteration). Dedup — the name may already be in covens.
//
// Empty name → nil (the caller returns 422 on a broken path before RBAC, or create
// passes name=its-own-name).
//
// IncarnationCovenContexts — exported wrapper over [incarnationCovenContexts] for
// reuse outside the package (MCP incarnation tools mirror the REST coven/service
// scope, RBAC parity endpoint↔MCP).
func IncarnationCovenContexts(name, service string, covens []string) []map[string]string {
	return incarnationCovenContexts(name, service, covens)
}

func incarnationCovenContexts(name, service string, covens []string) []map[string]string {
	if name == "" {
		return nil
	}
	seen := make(map[string]struct{}, len(covens)+1)
	candidates := make([]string, 0, len(covens)+1)
	add := func(c string) {
		if c == "" {
			return
		}
		if _, ok := seen[c]; ok {
			return
		}
		seen[c] = struct{}{}
		candidates = append(candidates, c)
	}
	for _, c := range covens {
		add(c)
	}
	add(name) // name — root Coven label (ADR-008).

	out := make([]map[string]string, 0, len(candidates))
	for _, c := range candidates {
		ctx := map[string]string{"incarnation": name, "coven": c}
		if service != "" {
			ctx["service"] = service
		}
		out = append(out, ctx)
	}
	return out
}

// IncarnationScopeSelector builds a [middleware.MultiSelectorExtractor] for routes
// over an EXISTING incarnation (get / history / run / unlock / upgrade / destroy):
// reads the incarnation row by path-`{name}` via reader and lands it into the RBAC
// context `incarnation=<name>`, `service=<inc.service>` and multi-value `coven=`
// (covens ∪ {name}) — closing the docs↔code drift where roles
// `incarnation.* on coven=…` / `on service=…` silently did NOT match
// (ADR-008 amendment a).
//
// The same [incarnation.SelectByName] these routes already do in the handler (the
// double select is the cold RBAC-gate path, not a hot path; the alternative —
// carrying inc from middleware into the handler via context — is needless coupling
// for one round-trip on a non-bulk operation).
//
// Fail-closed: an invalid/empty name or a not-found incarnation → nil set;
// [middleware.RequirePermissionMulti] then admits only bare/`*` roles (scoped —
// deny). The handler returns 404 for a nonexistent incarnation itself after a
// bare/`*` operator passes (parity with prior behavior: RBAC never knew about the
// incarnation's existence before either).
func IncarnationScopeSelector(reader IncarnationContextReader) middleware.MultiSelectorExtractor {
	return func(r *http.Request) []map[string]string {
		name := chi.URLParam(r, "name")
		if !incarnation.ValidName(name) {
			return nil
		}
		inc, err := incarnation.SelectByName(r.Context(), reader, name)
		if err != nil {
			// Not found / DB error → fail-closed for scoped roles. bare/`*` pass the
			// empty set, the handler returns 404 / 500 as before.
			return nil
		}
		return incarnationCovenContexts(inc.Name, inc.Service, inc.Covens)
	}
}

// IncarnationCreateScopeSelector — [middleware.MultiSelectorExtractor] for
// `POST /v1/incarnations` (the incarnation doesn't exist yet): scope from the
// request BODY — `service=<body.service>` + multi-value `coven=` from declared
// `body.covens` ∪ `{body.name}`. Prevents a coven-scoped operator from creating an
// incarnation tagged outside their scope (least-privilege; otherwise create =
// privilege escalation).
//
// The body is read under the already-wired `/v1/*` MaxBytesReader limit and
// restored for the handler (pattern [SoulCovenLabelSelector]): the handler decodes
// the body again (strict decoder). An invalid/empty body or broken name → nil set:
// scoped roles — deny, bare/`*` — pass (the handler then returns 400/422 on the
// body). covens from the body are NOT format-validated here (the handler does that
// before insert); an invalid label simply won't match any correct permission
// (scoped → deny), bare/`*` — pass, the handler returns 422.
func IncarnationCreateScopeSelector(r *http.Request) []map[string]string {
	if r.Body == nil {
		return nil
	}
	body, err := io.ReadAll(r.Body)
	_ = r.Body.Close()
	r.Body = io.NopCloser(bytes.NewReader(body))
	if err != nil || len(body) == 0 {
		return nil
	}
	var probe struct {
		Name    string   `json:"name"`
		Service string   `json:"service"`
		Covens  []string `json:"covens"`
	}
	if err := json.Unmarshal(body, &probe); err != nil || probe.Name == "" {
		return nil
	}
	return incarnationCovenContexts(probe.Name, probe.Service, probe.Covens)
}
