// Package scenario is the top-level orchestrator for scenario runs on the
// Keeper side (architect-recon slice .g). It wires together the git-artifact
// loader, topology resolver, service-vars resolver, render pipeline, gRPC
// outbound, and incarnation/applyrun CRUD into one async run.
//
// Run lifecycle (see [Runner.Start] → run-goroutine in run.go):
//
//	SelectByName → status=applying → Load(service) → ParseScenario →
//	LoadIncarnationHosts → Resolve(service vars) → Render → dispatch(per-task
//	cross-host barrier) → UpdateStateFromRun(commit | error_locked)
//
// Pilot DSL scope (PM decision): sequential tasks + per-host fan-out +
// apply:destiny + include (expanded before render via config.ExpandIncludes)
// + serial/run_once (slice D: run_once narrows the target at render time,
// serial rolls hosts in waves at dispatch time) + block/loop/async. What is
// still out of pilot scope is a key on a node that cannot carry it (loop: on an
// apply: task, loop:/async: on a keeper-side task, scenario orchestration inside
// a destiny) — render.Pipeline rejects those (ErrUnsupportedDSL).
// Cross-host barrier (orchestration.md §7): the run's terminal transition happens once after
// ALL waves/tasks on ALL hosts of the run finish, never per-wave.
//
// RunResult collection uses Variant A (poll apply_runs.status, PM decision):
// simpler cluster coordination — DB polling works across the cluster
// (subscribe is local-only). Polls [applyrun.SelectStatusesByApplyID] until
// all SIDs reach a terminal state or the per-scenario timeout fires.
package scenario

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"regexp"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/souls-guild/soul-stack/keeper/internal/applybus"
	"github.com/souls-guild/soul-stack/keeper/internal/artifact"
	"github.com/souls-guild/soul-stack/keeper/internal/auditpg"
	"github.com/souls-guild/soul-stack/keeper/internal/render"
	"github.com/souls-guild/soul-stack/keeper/internal/servicevars"
	"github.com/souls-guild/soul-stack/keeper/internal/topology"
	keeperv1 "github.com/souls-guild/soul-stack/proto/gen/go/keeper/v1"
	"github.com/souls-guild/soul-stack/sdk/module"
	"github.com/souls-guild/soul-stack/sdk/schema"
	"github.com/souls-guild/soul-stack/shared/audit"
)

// scenarioMainFile is the scenario entry point in the service repo
// (orchestration.md §1): `scenario/<name>/main.yml`.
const scenarioMainFile = "scenario/%s/main.yml"

// upgradeMainFile is the upgrade-scenario entry point (ADR-0068 §3): a second
// load channel alongside scenario/, selected via [RunSpec.FromUpgrade]
// (scenarioRelPath); format mirrors scenarioMainFile.
const upgradeMainFile = "upgrade/%s/main.yml"

// CreateScenarioName is the bootstrap scenario name that incarnation.Create
// (REST + MCP) runs first for a new incarnation. Lifecycle-kind (see
// [LifecycleScenarioNames]).
const CreateScenarioName = "create"

// DestroyScenarioName is the teardown scenario name in the service snapshot
// that [Runner.StartDestroy] runs in [TerminalDestroy] mode (S-D2b). Same
// value as incarnation.destroyScenarioName / destroyScenarioLabel but a
// different role (scenario filename vs. state_history transition label) —
// kept as a separate constant so changing one doesn't silently drag the
// other. Lifecycle-kind (see [LifecycleScenarioNames]).
const DestroyScenarioName = "destroy"

// LifecycleScenarioNames is the canonical keeper convention: scenario names
// treated as a specialized scenario-kind for the corresponding lifecycle
// phase. create → bootstrap a new incarnation; destroy → teardown
// (TerminalDestroy). All other scenarios are operational (free-form state
// operations) run via a normal run. Single source of truth for the DTO field
// [artifact.Scenario.Kind]: name ∈ set → lifecycle, else operational.
// Imperative name checks across keeper reference the per-name constants
// (CreateScenarioName / DestroyScenarioName), not string literals.
//
// `converge` is NOT in this set (amend ADR-031, 2026-06-10): it's an
// operational scenario — it runs via a normal run (Apply-reconcile) like any
// other. Its second role, as a dry-run target, left with NIM-446; nothing
// special-cases the name any more.
var LifecycleScenarioNames = map[string]struct{}{
	CreateScenarioName:  {},
	DestroyScenarioName: {},
}

// IsLifecycleScenario reports whether name is in [LifecycleScenarioNames]
// (lifecycle-kind). Used to tag [artifact.Scenario.Kind] in the listing
// handler.
func IsLifecycleScenario(name string) bool {
	_, ok := LifecycleScenarioNames[name]
	return ok
}

// IsRunnableScenario reports whether an operator can run this scenario from
// the Run form (ADR-042 "dumb frontend": UI reads the flag from the catalog,
// doesn't hardcode names). Canon: lifecycle-create=true (bootstrap a new
// incarnation), lifecycle-destroy=false (deletion is the DELETE
// /v1/incarnations/{name} flow, not a run), operational=true (free-form state
// operation). Tags [artifact.Scenario.Runnable] in the listing handler.
func IsRunnableScenario(name string) bool {
	return name != DestroyScenarioName
}

// scenarioTemplatePrefix is the scenario-local layer directory of the
// two-level resource resolve (orchestration.md §6): `scenario/<name>`.
// render.TemplateReader looks for `.tmpl` here first (scenario-local), then
// at service level. name has already passed ScenarioNamePattern validation
// (traversal/garbage rejected before path resolve).
func scenarioTemplatePrefix(name string) string {
	return "scenario/" + name
}

// ScenarioNamePattern is the canonical scenario name form: snake_case
// (`create`, `add_user`, `update_acl`), same grammar as register/loop/input
// identifiers (shared/config). Differs from incarnation kebab-case; name
// validation before resolving `scenario/<name>/main.yml` rejects path
// traversal (`/`, `..`) and garbage.
const ScenarioNamePattern = `^[a-z][a-z0-9_]*$`

var scenarioNameRe = regexp.MustCompile(ScenarioNamePattern)

// ValidScenarioName checks whether name matches [ScenarioNamePattern].
func ValidScenarioName(name string) bool { return scenarioNameRe.MatchString(name) }

// defaultPollInterval is the apply_runs.status polling period for the
// cross-host barrier fan-in (PM decision: 200ms). Optimization
// (subscribe/event-driven) is deferred.
const defaultPollInterval = 200 * time.Millisecond

// defaultRunTimeout is the ceiling on a single scenario run's duration.
// Guards against an "eternal barrier" (Soul hung, no RunResult). On expiry:
// abort + error_locked (PM decision: 5min).
const defaultRunTimeout = 5 * time.Minute

// deployBudget is added to the onboarding ceiling for a provision-from-zero
// run (ADR-0061): effective run-timeout = ResolvedMaxAwaitTimeout (the
// `await_online` barrier ceiling) + deployBudget. Covers the stage AFTER
// onboarding — deploying the role to newly onboarded hosts (apply redis etc.)
// in the Passage past the refresh boundary. Internal const, not a config key
// (the `run_timeout` knob is deferred separately); this is just a generous
// margin (deploy rarely exceeds a few minutes). Effective timeout applies
// ONLY to a plan with a refresh emitter ([config.HasRefreshEmitter]); a
// normal run keeps defaultRunTimeout (eternal barrier still aborts).
const deployBudget = 10 * time.Minute

// Sentinel errors for [Runner.Start].
var (
	// ErrAlreadyRunning — applyID already has an active run registered, or the
	// incarnation is in status applying (PM decision: pilot rejects, doesn't
	// queue).
	ErrAlreadyRunning = errors.New("scenario: run already in progress")
	// ErrShuttingDown — Runner is shutting down, new runs are not accepted.
	ErrShuttingDown = errors.New("scenario: runner is shutting down")
	// ErrLocked — incarnation is in status error_locked: the next run is
	// rejected until an explicit unlock (ADR-009, "Atomicity and
	// error_locked"). Checked under the same FOR UPDATE as the transition to
	// applying — the gate is authoritative in the transaction, not just the
	// HTTP handler (TOCTOU-safe).
	ErrLocked = errors.New("scenario: incarnation is error_locked")

	// ErrNotRunnable — incarnation status is outside the run allow-list
	// (anything but ready/applying/error_locked: destroying is mid-teardown;
	// migration_failed is locked by a failed migration, needs unlock/upgrade;
	// any future status). lockRun uses an explicit allow-list (fail-closed): a
	// new status is REJECTED by default, not silently allowed. Separate from
	// ErrLocked so logs/results distinguish "operator must unlock
	// error_locked" from "instance is in a non-flyable status".
	ErrNotRunnable = errors.New("scenario: incarnation status does not permit a run")

	// ErrKeeperModulesNotConfigured — scenario carries an `on: keeper` task but
	// the keeper-side core Registry isn't configured (Deps.KeeperModules ==
	// nil). A wire-up bug or a test build without keeper-side modules; the run
	// goes to error_locked (reason: keeper_dispatch_failed).
	ErrKeeperModulesNotConfigured = errors.New("scenario: keeper-side module registry is not configured")

	// errCancelRequested — internal barrier sentinel (G1): run was cancelled
	// via the cluster-wide cancel flag (apply_runs.cancel_requested, migration
	// 024). Any Keeper instance may have set the flag; the owning
	// run-goroutine breaks the barrier and aborts (error_locked) — same
	// behavior as a local ctx cancel. Not exported: externally the run is
	// visible via incarnation status, not this error.
	errCancelRequested = errors.New("scenario: cluster-wide cancel requested")
)

// TerminalMode is the finalization mode of a scenario run (S-D2b): decides
// what the run-goroutine does on successful completion of the
// teardown/apply cycle and how it records failure. Zero value
// [TerminalCommitState] = normal run — existing callers (Create /
// scenario-run) keep current behavior.
type TerminalMode int

const (
	// TerminalCommitState — normal run (apply/upgrade): success moves the
	// incarnation to ready and writes a state_history row; failure →
	// error_locked. The state fields themselves are already in the row, each
	// written at its own `core.state.<verb>` step ([ADR-0084]). Default (zero
	// value): all existing runs take this path.
	TerminalCommitState TerminalMode = iota

	// TerminalDestroy — teardown run (scenario `destroy`, S-D2b): success does
	// NOT commit state and does NOT transition to ready — the incarnation
	// stays in `destroying`; the physical row delete is done by S-D3. Teardown
	// failure (host down, barrier fail-closed) → status `destroy_failed` (NOT
	// error_locked): from there the operator retries destroy or unlocks to
	// ready (S-D2a added Unlock destroy_failed→ready).
	TerminalDestroy
)

// RunSpec holds the parameters of one scenario run, passed by the API
// handler to [Runner.Start].
//
// ApplyID is the run's ULID (already generated by the caller; flows into
// apply_runs / state_history / RunResult correlation). ServiceRef is the
// service repo's git coordinates (caller resolves from the service registry
// by incarnation.service, ADR-029).
// Input is the operator's `incarnation.spec.input`. StartedByAID is the
// initiator (Archon AID from the Operator API; empty string → NULL in
// apply_runs).
//
// TerminalMode is the finalization mode (S-D2b): zero value
// [TerminalCommitState] = normal run; [TerminalDestroy] = teardown scenario
// `destroy`.
type RunSpec struct {
	ApplyID         string
	IncarnationName string
	ServiceRef      artifact.ServiceRef
	ScenarioName    string
	Input           map[string]any
	StartedByAID    string
	TerminalMode    TerminalMode

	// Covens / Traits — the labels the request will write onto the incarnation
	// row. Read by ONE caller and for one reason: on the create path the row does
	// not exist yet, so [Runner.PreflightAssert] synthesises an incarnation, and a
	// `vars/_stack.yaml` keyed on `incarnation.covens` would otherwise resolve
	// fewer layers at the gate than the create run resolves a moment later — the
	// NIM-271 divergence, reappearing on the one path that still has no row.
	// Empty on every other path, where the real row is read instead.
	Covens []string
	Traits map[string]any

	// inputSnapshot — the masked operator-input snapshot (maskedInputSnapshot of
	// Input), computed once at run start and written to apply_runs.input on every
	// row (migration 101). Unexported: set by run(), not by the API caller.
	inputSnapshot json.RawMessage

	// FromLocked — run starts from an already-reserved applying status
	// (rerun-last: incarnation.UnlockForRerun under FOR UPDATE moved
	// error_locked→applying bypassing ready, race-free). lockRun with
	// FromLocked does NOT transition status again — it must observe applying,
	// otherwise the start is rejected (fail-closed). Zero value = normal
	// start.
	FromLocked bool

	// FromUpgrade — load upgrade/<name>/main.yml instead of scenario/
	// (ADR-0068): second auto-discover channel for version-to-version upgrade
	// scenarios. Zero value = normal scenario/ path (today's behavior). The
	// auto-start found-branch (who sets this flag) is Slice 2.
	FromUpgrade bool

	// CadenceID — back-link to the Cadence schedule that spawned this run
	// (ADR-046 §2, T4b foundation). nil ⇒ manual run (operator/Voyage without
	// a schedule); populated ⇒ a schedule's child Voyage. Threaded from
	// voyages.cadence_id (BuildVoyage → VoyageWorker → ScenarioSpawner).
	// Carried in the incarnation.run_completed terminal event payload ONLY
	// when != nil (manual runs omit the cadence_id key — conservative, same as
	// the drift payload), so a standing Tiding rule with a cadence selector
	// catches schedule run results.
	CadenceID *string

	// VoyageID — back-link to the Voyage that spawned this run
	// (voyages.voyage_id, ADR-043). Threaded from VoyageWorker through
	// ScenarioSpawner.SpawnScenarioRun (the production spawner sets it). nil ⇒
	// run is NOT via a Voyage: direct paths — scenario `create` (auto_create) /
	// rerun-last / destroy and their MCP equivalents call Runner directly,
	// bypassing the voyage orchestrator. Carried in the
	// incarnation.run_completed terminal event payload ONLY when != nil
	// (symmetric with CadenceID) — for the Voyage detail page's
	// per-incarnation run-event visibility fetch (ADR-052 amend §k: the event
	// is per-incarnation with correlation_id=apply_id, and the voyage page
	// filters by voyage_id in the payload).
	VoyageID *string
}

// ApplyDispatcher is the narrow gRPC-outbound surface the orchestrator needs:
// send an `ApplyRequest` to one Soul. An interface (not *grpc.Outbound) so
// the runner is unit-testable without standing up EventStream/StreamManager.
//
// Implemented by [grpc.Outbound] (SendApply method, same signature).
type ApplyDispatcher interface {
	SendApply(ctx context.Context, sid string, req *keeperv1.ApplyRequest) error
}

// SummonsPublisher is the narrow Redis-publish surface for the Summons
// signal (ADR-027(a)): "planned tasks appeared, check the queue". An
// interface (not a direct keeper/internal/redis import) keeps scenario-runner
// independent of the Redis client — same approach as
// [acolyte.SummonsSubscriber]. nil → the new dispatch path sends no Summons;
// planned tasks are picked up by Acolyte's poll-fallback (best-effort,
// ADR-027(a)).
//
// Implemented by a thin wrapper over [redis.PublishSummons] at wire-up
// (1.4.4).
type SummonsPublisher interface {
	PublishSummons(ctx context.Context) error
}

// LeaseOwnerChecker is the narrow Redis-read surface for the SID-lease owner:
// "which Keeper instance holds the EventStream to this Soul". An interface
// (not a direct keeper/internal/redis import) keeps scenario-runner
// independent of the Redis client — same approach as [SummonsPublisher].
//
// Needed ONLY by the run-goroutine path's multi-keeper guard (acolytes=0,
// [Runner.dispatchWave]): before SendApply it checks the lease owner against
// its own KID and logs a WARN on mismatch (footgun: "run hangs in applying" —
// the RunResult would go to the stream owner on another instance). With
// acolytes>0 (work-queue, ADR-027) this problem doesn't exist and the guard
// isn't invoked.
//
// ok=false — no lease key (Soul isn't on anyone's stream); error — network
// failure (guard degrades silently, no WARN). nil → guard disabled (no Redis
// / unit test without coordination).
//
// Implemented by a thin wrapper over [redis.SoulLeaseOwner] at wire-up.
type LeaseOwnerChecker interface {
	SoulLeaseOwner(ctx context.Context, sid string) (kid string, ok bool, err error)
}

// SoulCapabilityChecker is the narrow Redis-check surface for "which SIDs have
// NOT announced capability X" (ADR-056 §S5 forward-compat, generalized by
// ADR-0076(i)). An interface (not a direct keeper/internal/redis import) keeps
// scenario-runner independent of the Redis client — same approach as
// [LeaseOwnerChecker] / [SummonsPublisher].
//
// Two gates in run.go use it, both fail-closed BEFORE dispatch:
//   - staged (ADR-056 §S5): every target host must echo ApplyRequest.passage,
//     otherwise the next Passage's barrier would wait for a terminal state an old
//     binary never sends (hangs in applying);
//   - plan requirements (ADR-0076(i)): every host must announce the modules and
//     Soul-side DSL features the tasks targeting IT use, otherwise an old binary
//     reports OK/CHANGED with no effect (the silent-ignore mode).
//
// Returns the subset of passed SIDs LACKING the capability (empty/nil → all
// support it). Error — Redis network failure: the gate must reject the run
// (rather than guess support), so the error propagates up.
//
// nil in Deps → the gates degrade fail-closed: without a presence source there
// is nothing to confirm support against. Implemented by a thin wrapper over
// [redis.SoulsLackingCapability] at wire-up, parameterized by capability name —
// the Redis layer is already generic.
type SoulCapabilityChecker interface {
	SoulsLackingCapability(ctx context.Context, sids []string, capability string) ([]string, error)
}

// SoulVersionReader reads the raw version a SID announced on its current
// connection (heartbeat Hash field `ver`, written by the same Hello overwrite as
// `caps`). Feeds the engine provenance stamp on apply_runs (ADR-0076(l)) and
// nothing else — no gate may read it (ADR-0076(n)).
//
// A host that never announced returns ("", nil): "not recorded", not an error.
// A Redis failure returns an error, and unlike [SoulCapabilityChecker] the
// caller does NOT fail closed on it — a lost audit field must never cost an
// apply. nil in Deps degrades the same way.
//
// Implemented by a thin wrapper over [redis.ReadSoulVersion] at wire-up.
type SoulVersionReader interface {
	ReadSoulVersion(ctx context.Context, sid string) (string, error)
}

// KeeperModuleRegistry is the narrow keeper-side core Registry surface
// (keeper/internal/coremod) scenario-runner needs for local execution of
// `on: keeper` tasks (ADR-017, docs/keeper/modules.md). An interface (not a
// direct *coremod.Registry) keeps the scenario package testable without
// building all keeper-side modules and their deps (PG / Vault / PluginHost) —
// a fake implements just Lookup.
//
// Implemented by [coremod.Registry] (Lookup method, same signature). nil in
// Deps → `on: keeper` tasks are rejected ([ErrKeeperModulesNotConfigured]).
type KeeperModuleRegistry interface {
	Lookup(name string) (module.SoulModule, bool)
}

// KeeperPluginRegistry is the second half of keeper-side execution (NIM-758):
// the PLUGIN modules this Keeper may run itself, for an address that is not one
// of the built-in core ones. Implemented by
// [pluginhost.KeeperSideModules]; an interface for the same reason
// [KeeperModuleRegistry] is one — the scenario package must stay testable
// without a plugin cache, a Sigil registry or a forked binary.
//
// The two methods answer two different questions and the split is load-bearing.
// LookupKeeperSide only ever returns a module whose document declared
// `side: keeper`, so nothing reachable through it can be a Soul-side module —
// the fail-closed property does not rest on the caller. DeclaredSide adds the
// fact needed to write an honest refusal for everything else: "that module runs
// on the Soul" and "there is no such module" are opposite mistakes with
// opposite fixes, and answering the second for the first sends an author
// hunting a typo that is not there.
//
// nil in Deps → a plugin address stays `unknown keeper-side module`, exactly as
// before this executor existed.
type KeeperPluginRegistry interface {
	LookupKeeperSide(base string) (module.SoulModule, bool)
	DeclaredSide(base string) (schema.Side, bool)
}

// ChangedTaskReader is the narrow read-access surface to the audit log: sets
// of (sid, plan_index) run tasks that terminated CHANGED (T3, changed_tasks
// rollup) or FAILED/TIMED_OUT (ADR-056 R3, cross-passage onfail-rescue
// gating). Backed by `task.executed` events (events_taskevent.go), not a
// separate table. An interface (not a direct *auditpg.Reader) keeps the
// scenario package testable without PG — a fake implements the two methods.
//
// Implemented by [auditpg.Reader] (SelectChangedTaskKeys /
// SelectFailedTaskKeys methods, same signature). nil → the
// incarnation.run_completed terminal event is emitted without changed_tasks
// (rollup skipped, run finalization doesn't fail); cross-passage
// onchanges/onfail gating (R3) degrades fail-closed — a staged run with a
// cross-passage requisite is rejected (see run.go).
type ChangedTaskReader interface {
	SelectChangedTaskKeys(ctx context.Context, applyID string) (map[auditpg.ChangedTaskKey]struct{}, error)
	SelectFailedTaskKeys(ctx context.Context, applyID string) (map[auditpg.ChangedTaskKey]struct{}, error)
}

// Deps holds [Runner]'s constructor dependencies. All required except Logger
// (nil → discard) and Destiny (nil → apply:destiny unsupported,
// ErrUnsupportedDSL).
type Deps struct {
	Loader      *artifact.ServiceLoader
	Topology    *topology.Resolver
	ServiceVars *servicevars.Resolver
	Render      *render.Pipeline
	Outbound    ApplyDispatcher
	// Destiny — source of destiny artifacts for apply:destiny
	// (default_destiny_source + DestinyLoader). nil → apply:destiny in a
	// scenario is rejected at render phase (ErrUnsupportedDSL).
	Destiny *DestinySource
	// KeeperModules — keeper-side core Registry (ADR-017): `on: keeper` tasks
	// (`core.soul.registered`, `core.cloud.provisioned`, `core.vault.kv-read`)
	// execute locally on the instance through it. nil → `on: keeper` tasks
	// are rejected at dispatch phase ([ErrKeeperModulesNotConfigured]); a
	// pure Soul-side run works without it.
	KeeperModules KeeperModuleRegistry
	// KeeperPlugins — the keeper-side PLUGIN modules (NIM-758): an address the
	// core Registry above does not know is looked up here, and executes locally
	// when its schema document declares `side: keeper` ([ADR-0087]). nil → a
	// plugin address on a keeper-side task fails `unknown keeper-side module`,
	// which is what it did before this executor existed; a cluster with no
	// keeper-side plugins is unaffected either way.
	KeeperPlugins KeeperPluginRegistry
	// ModuleManifests — source of the plugin manifests this cluster has
	// allow-listed, so a definition's `params:` are checked against them while
	// it is parsed (NIM-228), the same four checks `core.*` already gets from
	// the embedded registry. nil → plugin params are not checked and each such
	// module is reported as `plugin_params_unchecked`; the run is not affected.
	ModuleManifests artifact.PluginManifestSource
	// DB — pool for incarnation + applyrun CRUD (single Postgres, ADR-005).
	DB     *pgxpool.Pool
	Logger *slog.Logger

	// Vault — shared keeper-vault client for scoped resolve of `vault:` refs
	// in operator input (docs/input.md → "vault_scope"). nil → input vault
	// refs are not resolved (a field with a `vault:` value is rejected at
	// input phase as a plain string, never reaching ReadKV). Same client as
	// the render pipeline.
	Vault InputVaultReader
	// Audit — write path for the security trail of input-vault-ref resolve
	// (`input.vault_resolved`, ok/denied) and the run's terminal event
	// (`incarnation.run_completed`, T3). nil → trail isn't written (resolve
	// still works, just without audit — fine for unit tests).
	Audit audit.Writer

	// ApplyBus — pub/sub bus for apply events (ADR-068 §A2): keeper-side
	// task.executed is published here SYMMETRICALLY with Soul-side
	// (grpc/events_taskevent.go), so `on: keeper` tasks are visible on the
	// operator SSE. nil → keeper-side events don't reach SSE (audit path
	// still works as before). Same bus as the grpc handlers.
	ApplyBus *applybus.EventBus
	// AuditReader — read access to the audit log for the per-task changed
	// rollup (T3): the set of (sid, task_idx) CHANGED tasks in the run. nil →
	// the incarnation.run_completed terminal event is emitted without
	// changed_tasks (rollup skipped). Same pool/Reader as the Operator API
	// (auditpg.NewReader).
	AuditReader ChangedTaskReader

	// InputDenyPaths — config extension to the system-floor hard deny-list
	// (keeper.yml → vault.input_deny_paths). Adds to the reserved-namespace floor
	// ([config.PathUnderReservedNamespace]), doesn't replace it.
	InputDenyPaths []string

	// Metrics — keeper_scenario_* collectors (ADR-024). nil → run metrics
	// disabled (nil-safe [ScenarioMetrics] methods are no-ops). Must be the
	// same descriptor registered in main on the shared metricsReg.
	Metrics *ScenarioMetrics

	// AcolyteEnabled — apply-execution cutover flag (ADR-027, Phase 1.4.2):
	// true → dispatch writes planned tasks + Summons (new path, executed by
	// the Acolyte pool); false → direct Insert(running)+SendApply (old path).
	// Set from cfg.Acolytes>0 at wire-up (1.4.4). The serial guard
	// (run.go::run) still forces scenarios with `serial:` tasks onto the old
	// path — distributed serial is Phase 3.
	AcolyteEnabled bool

	// KID — Keeper instance identifier (Summons signal origin, ADR-027(a)).
	// Needed by the new dispatch path for [SummonsPublisher]. Set at wire-up
	// (1.4.4); empty → the AcolyteEnabled path sends no Summons (poll-fallback
	// picks it up).
	KID string

	// Summons — publisher of the planned-tasks Summons signal (ADR-027(a)).
	// nil → the new dispatch path sends no Summons (best-effort, Acolyte's
	// poll-fallback picks it up). Set at wire-up (1.4.4).
	Summons SummonsPublisher

	// LeaseOwner — SID-lease owner checker for the run-goroutine path's
	// multi-keeper guard (acolytes=0). nil → guard disabled (no Redis / unit
	// test). Set at wire-up only with live Redis: on a foreign KID owner,
	// [Runner.dispatchWave] logs a WARN about a possible run hang in applying
	// (single-keeper-only footgun of the acolytes=0 default).
	LeaseOwner LeaseOwnerChecker

	// SoulCap — soul-capability checker for target hosts, used by run.go's staged
	// gate (ADR-056 §S5) and by the plan-requirement gate (ADR-0076(i)). nil →
	// both reject fail-closed (can't confirm support without Redis). Set at
	// wire-up with a thin wrapper over redis.SoulsLackingCapability.
	SoulCap SoulCapabilityChecker

	// SoulVersion — reader of the version a target host announced on its current
	// connection, for the engine provenance stamp on apply_runs (ADR-0076(l)).
	// nil → nothing is stamped: the stamp is audit-only (ADR-0076(n)), so its
	// absence degrades the record, never the run. Set at wire-up with a thin
	// wrapper over redis.ReadSoulVersion.
	SoulVersion SoulVersionReader

	// KeeperVersion — the raw build version of THIS keeper instance, checked
	// against the `compat:` window declared by the service and by every destiny a
	// run resolves (ADR-0076(f)): the rendering instance is the authority, since
	// a rolling upgrade means cluster instances differ in version. Empty (or any
	// string carrying no version, e.g. `0.0.0-dev`) → the window is NOT enforced
	// and the skip is logged; unit paths without a wire-up behave as before.
	KeeperVersion string

	// PollInterval / RunTimeout — override the defaults (for tests). Zero
	// value → default.
	PollInterval time.Duration
	RunTimeout   time.Duration

	// MaxAwaitTimeoutFn — hot-reload-aware source for the `await_online`
	// onboarding barrier ceiling ([config.KeeperConfig.ResolvedMaxAwaitTimeout],
	// ADR-0061): base for the provision-aware effective run-timeout (ceiling +
	// deployBudget) for a run with a refresh emitter. A closure (not a value)
	// so an operator override of keeper.yml::max_await_timeout is picked up by
	// the next run without a restart — same approach as coremod's
	// MaxAwaitTimeout (daemon.go). nil → effective timeout falls back to
	// [config.DefaultMaxAwaitTimeout] (30m) — unit test/L0 without
	// config.Store; a provision run still gets the extended ceiling, just
	// without the hot-reload override.
	MaxAwaitTimeoutFn func() time.Duration
}

// Runner is the singleton orchestrator of scenario runs. Injected into the
// API handler (incarnation.Create) and the future `/scenarios/{scenario}`
// endpoint.
//
// active holds cancel functions for active runs keyed by applyID — used by
// [Runner.Cancel] and graceful [Runner.Shutdown]. wg tracks live
// run-goroutines. shuttingDown closes off new runs.
type Runner struct {
	deps         Deps
	logger       *slog.Logger
	pollInterval time.Duration
	runTimeout   time.Duration

	// maxAwaitTimeoutFn — hot-reload-aware onboarding barrier ceiling (copy of
	// Deps.MaxAwaitTimeoutFn), base for the provision-aware effective
	// run-timeout (run.go::effectiveRunTimeout). nil → falls back to
	// [config.DefaultMaxAwaitTimeout].
	maxAwaitTimeoutFn func() time.Duration

	// acolyteEnabled / kid — cutover flag and origin KID for the new dispatch
	// path (ADR-027, Phase 1.4.2): copies of Deps.AcolyteEnabled / Deps.KID,
	// read in run.go::run when branching the dispatch path.
	acolyteEnabled bool
	kid            string

	// leaseOwner — SID-lease owner checker (copy of Deps.LeaseOwner) for the
	// old dispatch path's multi-keeper guard (acolytes=0, dispatch.go). nil →
	// guard disabled.
	leaseOwner LeaseOwnerChecker

	// soulCap — soul-capability checker for target hosts (copy of Deps.SoulCap)
	// for run.go's staged gate (ADR-056 §S5) and plan-requirement gate
	// (ADR-0076(i)). nil → both reject fail-closed.
	soulCap SoulCapabilityChecker

	// soulVersion — announced-version reader (copy of Deps.SoulVersion) for the
	// engine provenance stamp on dispatch (ADR-0076(l)). nil → the stamp is
	// simply not written; audit-only, never a gate.
	soulVersion SoulVersionReader

	// keeperModules — keeper-side core Registry (copy of Deps.KeeperModules)
	// for local execution of `on: keeper` tasks (run.go::dispatchKeeperTasks).
	// nil → `on: keeper` tasks are rejected ([ErrKeeperModulesNotConfigured]).
	keeperModules KeeperModuleRegistry

	// keeperPlugins — keeper-side PLUGIN modules (copy of Deps.KeeperPlugins),
	// consulted by applyKeeperTask when the core Registry misses. nil → a plugin
	// address stays `unknown keeper-side module`.
	keeperPlugins KeeperPluginRegistry

	mu           sync.Mutex
	active       map[string]context.CancelFunc
	wg           sync.WaitGroup
	shuttingDown bool
}

// NewRunner assembles a Runner. Panics on nil required dependencies — a
// wire-up bug (main), not a runtime condition.
func NewRunner(deps Deps) *Runner {
	if deps.Loader == nil || deps.Topology == nil || deps.ServiceVars == nil ||
		deps.Render == nil || deps.Outbound == nil || deps.DB == nil {
		panic("scenario: NewRunner: required dependency is nil")
	}
	logger := deps.Logger
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	pollInterval := deps.PollInterval
	if pollInterval <= 0 {
		pollInterval = defaultPollInterval
	}
	runTimeout := deps.RunTimeout
	if runTimeout <= 0 {
		runTimeout = defaultRunTimeout
	}
	return &Runner{
		deps:              deps,
		logger:            logger,
		pollInterval:      pollInterval,
		runTimeout:        runTimeout,
		maxAwaitTimeoutFn: deps.MaxAwaitTimeoutFn,
		acolyteEnabled:    deps.AcolyteEnabled,
		kid:               deps.KID,
		leaseOwner:        deps.LeaseOwner,
		soulCap:           deps.SoulCap,
		soulVersion:       deps.SoulVersion,
		keeperModules:     deps.KeeperModules,
		keeperPlugins:     deps.KeeperPlugins,
		active:            make(map[string]context.CancelFunc),
	}
}
