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

	"github.com/go-chi/chi/v5"

	"github.com/souls-guild/soul-stack/keeper/internal/api/middleware"
	"github.com/souls-guild/soul-stack/keeper/internal/artifact"
	"github.com/souls-guild/soul-stack/keeper/internal/auditpg"
	"github.com/souls-guild/soul-stack/keeper/internal/incarnation"
	"github.com/souls-guild/soul-stack/keeper/internal/jwt"
	"github.com/souls-guild/soul-stack/keeper/internal/rbac"
	"github.com/souls-guild/soul-stack/keeper/internal/scenario"
	"github.com/souls-guild/soul-stack/keeper/internal/statemigrate"
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

// --- RBAC scope (NIM-128 boolean scope) -------------------------------

// incScopeColumns maps the NIM-128 scope dimensions onto the `incarnation`
// table columns for [rbac.PurviewSQL] pushdown. Incarnations carry coven
// (covens TEXT[], migration 046), incarnation-name (name), service (service)
// and trait (traits jsonb, migration 088). They have NO host dimension, so
// ScopeColumns.Host is empty — a host scope condition renders FALSE
// (fail-closed), never matching an incarnation.
var incScopeColumns = rbac.ScopeColumns{
	Coven:       "covens",
	Incarnation: "name",
	Service:     "service",
	Traits:      "traits",
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
// from *http.Request). Semantics (NIM-128 boolean scope): the incarnation's own
// dimensions (covens / name / service / traits) are fed to [rbac.Purview.Match] —
// Unrestricted → true; Deny/empty Purview → false; otherwise true iff ANY scope
// predicate matches. The incarnation carries no host dimension (host conditions
// fail closed). nil claims / nil scoper → always false (fail-closed).
func (h *IncarnationHandler) GetInScopeFor(claims *jwt.Claims, action string) func(*incarnation.Incarnation) bool {
	return func(inc *incarnation.Incarnation) bool {
		if claims == nil || h.scoper == nil {
			return false
		}
		pv := h.scoper.ResolvePurview(claims.Subject, "incarnation", action)
		return pv.Match(rbac.ScopeInput{
			Covens:       inc.Covens,
			Incarnations: []string{inc.Name},
			Services:     []string{inc.Service},
			Traits:       traitsToScopeInput(inc.Traits),
		})
	}
}

// traitsToScopeInput projects an incarnation's `traits` jsonb (map[string]any)
// into the [rbac.ScopeInput.Traits] shape (key → string values). A scalar value
// becomes a one-element slice; a list value contributes each element's string
// form; nested maps are skipped (not addressable by a scalar/list scope trait).
// fmt.Sprint gives the canonical string for string/number/bool (jsonb numbers
// arrive as float64 / json.Number).
func traitsToScopeInput(traits map[string]any) map[string][]string {
	if len(traits) == 0 {
		return nil
	}
	out := make(map[string][]string, len(traits))
	for k, v := range traits {
		switch tv := v.(type) {
		case []any:
			vals := make([]string, 0, len(tv))
			for _, e := range tv {
				vals = append(vals, fmt.Sprint(e))
			}
			out[k] = vals
		case map[string]any:
			// nested object — not addressable by a scalar/list trait scope.
			continue
		default:
			out[k] = []string{fmt.Sprint(v)}
		}
	}
	return out
}

// ResolveListScopeFor — request-free factory of a scope resolver for FULL-TYPED
// ListTyped (huma layer). Semantics: fail-closed on nil claims/nil scoper/Empty
// Purview (the caller returns an empty list); Unrestricted → the whole list;
// otherwise the boolean scope predicate is pushed down into the list query via
// [rbac.PurviewSQL]. serviceFilter is unused under the NIM-128 boolean scope (no
// separate state-CEL resolution pass); the parameter is retained for the stable
// ListTyped resolver signature.
func (h *IncarnationHandler) ResolveListScopeFor(ctx context.Context, claims *jwt.Claims) func(serviceFilter string) (incarnation.ListScope, bool) {
	return func(serviceFilter string) (incarnation.ListScope, bool) {
		return h.resolveListScope(ctx, claims, "list", serviceFilter)
	}
}

// resolveListScope — shared Purview→[incarnation.ListScope] resolution for
// list-like reads (List action=list; global runs/stats action=history).
// Fail-closed on nil claims / nil scoper / empty(Deny) Purview (caller returns an
// empty list). Unrestricted → no scope narrowing. Otherwise the boolean scope
// predicates (OR of Exprs) are rendered into SQL over the incarnation columns by
// [rbac.PurviewSQL], carried as a placeholder-relative closure so the incarnation
// package needs no rbac import (see [incarnation.ScopeSQLFunc]).
func (h *IncarnationHandler) resolveListScope(ctx context.Context, claims *jwt.Claims, action, serviceFilter string) (incarnation.ListScope, bool) {
	_ = ctx
	_ = serviceFilter
	if claims == nil || h.scoper == nil {
		return incarnation.ListScope{}, false
	}
	pv := h.scoper.ResolvePurview(claims.Subject, "incarnation", action)
	if pv.Unrestricted {
		return incarnation.ListScope{Unrestricted: true}, true
	}
	if pv.IsEmpty() {
		return incarnation.ListScope{}, false
	}
	scope := incarnation.ListScope{
		Scope: func(startIdx int) (string, []any, int) {
			return rbac.PurviewSQL(pv, incScopeColumns, startIdx)
		},
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
// Effective coven scope = the declared `covens` ONLY (ADR-008 amendment
// 2026-07-17/NIM-124: `incarnation.name` is NOT a Coven — it is no longer added
// to the coven set). Each declared coven → a context
// `{incarnation, service, coven=<c>}`; scope by the incarnation's own name is the
// `incarnation=<name>` dimension carried in every context (a role
// `incarnation.* on incarnation=<name>` matches). If the incarnation declares no
// covens, a single `{incarnation, service}` context (no coven) is emitted so the
// incarnation/service dimensions still match. The permission check ORs them.
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
	seen := make(map[string]struct{}, len(covens))
	candidates := make([]string, 0, len(covens))
	for _, c := range covens {
		if c == "" {
			continue
		}
		if _, ok := seen[c]; ok {
			continue
		}
		seen[c] = struct{}{}
		candidates = append(candidates, c)
	}

	base := func() map[string]string {
		ctx := map[string]string{"incarnation": name}
		if service != "" {
			ctx["service"] = service
		}
		return ctx
	}

	// No declared covens → a single incarnation/service context (scope by the
	// incarnation's own name is the `incarnation=` dimension, not `coven=`).
	if len(candidates) == 0 {
		return []map[string]string{base()}
	}

	out := make([]map[string]string, 0, len(candidates))
	for _, c := range candidates {
		ctx := base()
		ctx["coven"] = c
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
