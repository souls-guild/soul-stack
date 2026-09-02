package trial

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	securejoin "github.com/cyphar/filepath-securejoin"
	"google.golang.org/protobuf/types/known/structpb"

	"github.com/souls-guild/soul-stack/keeper/internal/render"
	"github.com/souls-guild/soul-stack/keeper/internal/stateop"
	"github.com/souls-guild/soul-stack/keeper/internal/topology"
	"github.com/souls-guild/soul-stack/shared/cel"
	"github.com/souls-guild/soul-stack/shared/config"
)

// trialHostSID — synthetic SID of single-host sugar host (fixtures.soulprint).
// L0 is hermetic and does not target real registry; for render-only assert single
// host is sufficient. Multi-host roster (fixtures.hosts) carries its own SIDs
// (per-host dispatch variability — layer L3, outside pilot).
const trialHostSID = "trial-host"

// Level — test level (ADR-023) by which case is routed at
// run time. Distinguishes report lines: L0 (render-only) / L1 (migration) / L2
// (stand, skip in MVP).
type Level int

const (
	LevelL0 Level = iota // render-only, hermetic (RunCase)
	LevelL1              // state_schema migration test (RunMigrationCase)
	LevelL2              // stand, skipped in MVP (ADR-023 post-MVP)
)

// Result — outcome of a single case run.
//
// Level — level by which case is routed (for report). Skipped=true —
// case recognized as L2 (stand:/verify: marker) and skipped: MVP-harness does not
// execute it (ADR-023 post-MVP). For skipped case Pass=true (does not fail
// run), Failures/Coverage are empty, Case — file name. Coverage is filled
// only for L0 (L1/L2 render pipeline is not run).
type Result struct {
	Case     string
	Level    Level
	Pass     bool
	Skipped  bool
	Failures []string // human-readable assert mismatches; empty if Pass
	Coverage CoverageReport

	// Service is the identity the own-namespace Vault fence ran against ([ADR-0083] §7);
	// ServiceStated says whether the case named it (`fixtures.service`) or the harness
	// derived it from the directory.
	//
	// Reported, not merely carried. A derived name that is not the name the service is
	// REGISTERED under is non-empty and simply matches nothing, so the fence runs and
	// finds nothing — indistinguishable from a clean scenario in any output that does not
	// print the name, and offline there is no second source to check it against. The
	// operator is the only party who knows the registered name. Same rule soul-lint
	// applies with `own_namespace_fence_unchecked`: a check whose input was guessed says so.
	Service       string
	ServiceStated bool
}

// renderedCase — result of hermetic render pass of case: flat plan
// of tasks + reusable pipeline/RenderInput plus the coverage-sink with
// accumulated CEL branches. One per case run,
// shared by L0-assert (RunCase) and L2-execution (RunL2Case):
// both start with the same Keeper-side render plan.
type renderedCase struct {
	tasks    []*render.RenderedTask
	pipeline *render.Pipeline
	in       render.RenderInput
	sink     *coverageSink

	// service / serviceStated — the fence identity and its source, carried to the
	// report. See Result.Service.
	service       string
	serviceStated bool
}

// loadResolvedScenario loads scenario/<name>/main.yml from case.yml path and
// performs covenant resolution mirroring prod (keeper LoadScenarioManifestResolved):
// merges covenant.yml (by scn.Extends, sibling service.yml in test tree root,
// serviceRootFor) and validates form post-merge. Without this covenant-scenario
// would fail with false form_field_unknown in semantic phase (form gated before merge,
// scenario.go), and CEL compute/input on covenant fields («${ compute.install }» etc.)
// would not resolve. $type resolution is not run in L0 (no wrapper directory) —
// covenant-merge is sufficient.
//
// SINGLE source of covenant resolution for ALL trial render helpers (renderCase from
// harness.go AND renderCreateReadSet from redis_create_secrets_coverage_test.go): any
// new render-helper must call IT, otherwise will again forget covenant and fail with
// «no such key: compute.install». Guard tests at plan level (loadCreatePlan/
// loadScenarioPlan) DO NOT resolve covenant intentionally — they compare []Task before render,
// covenant fields are not computed there.
func loadResolvedScenario(caseFile string) (*config.ScenarioManifest, *config.Document, error) {
	scnPath := scenarioPathFor(caseFile)
	scn, doc, diags, err := config.LoadScenarioManifest(scnPath, config.ValidateOptions{})
	if err != nil {
		return nil, nil, fmt.Errorf("trial: loading scenario %s: %w", scnPath, err)
	}
	diags = append(diags, config.ResolveScenarioCovenant(scn, doc, serviceRootFor(caseFile))...)
	if hasErrors(diags) {
		return nil, nil, fmt.Errorf("trial: scenario %s invalid: %s", scnPath, formatDiags(diags))
	}
	return scn, doc, nil
}

// renderCase runs Keeper-side render pipeline of case hermetically: loads
// scenario next to case.yml, expands include, builds RenderInput from fixtures,
// renders plan with fixture-vault and coverage-sink. Returns renderedCase —
// common start for L0-check and L2-execution. Does not assert anything itself.
//
// caseFile — path to case.yml itself (from LoadCase). scenario/<name>/main.yml
// resolves as `<dir(case.yml)>/../../main.yml` (tests/<case>/case.yml).
func renderCase(ctx context.Context, c *Case, caseFile string) (renderedCase, error) {
	var rc renderedCase

	// FIRST, before anything that can return early. Every `return rc, err` below hands
	// the caller this struct and RunCase reports rc.service from it — a case that aborts
	// (expect_render_error, a broken scenario) still fenced on a name, and printing an
	// empty one would have the report claim the fence had no identity when it did. Pure:
	// no I/O, nothing to fail.
	svcName, svcStated := trialServiceIdentity(caseFile, c.Fixtures)
	rc.service, rc.serviceStated = svcName, svcStated

	scn, _, err := loadResolvedScenario(caseFile)
	if err != nil {
		return rc, err
	}
	scnPath := scenarioPathFor(caseFile)

	// Expand scenario-include to flat list before render (orchestration.md §6),
	// same as in prod scenario.run. Two-level resolution scenario-locally →
	// service-level from fixture tree.
	expanded, iDiags := config.ExpandIncludes(scn.Tasks, fixtureScenarioIncludeResolver(scnPath))
	if hasErrors(iDiags) {
		return rc, fmt.Errorf("trial: expanding include in scenario %s: %s", scnPath, formatDiags(iDiags))
	}
	scn.Tasks = expanded

	// Synthesize install steps from service.yml::modules[] (ADR-065) — mirror prod
	// scenario.run (after ExpandIncludes, before render): L0-plan ≡ prod-plan.
	svcManifest, err := loadTrialServiceManifest(caseFile)
	if err != nil {
		return rc, err
	}
	if svcManifest != nil {
		scn.Tasks, _ = config.SynthesizeModuleInstalls(scn.Tasks, svcManifest.Modules)
	}

	// fixtureVault implements both render.KVReader (vault-resolve params) and
	// cel.KVReader (CEL vault() function) — one hermetic reader for both phases.
	fv := newFixtureVault(c.Fixtures.Vault)
	engine, err := cel.New(cel.WithVault(fv))
	if err != nil {
		return rc, fmt.Errorf("trial: building CEL engine: %w", err)
	}
	sink := newCoverageSink()
	engine.SetCoverageSink(sink)

	pipeline := render.NewPipeline(fv, engine, nil, nil)

	// apply:destiny resolves by prod model mirror (slice A, ADR-023):
	// service.yml::destiny[] (dependency declaration + ref/git) + template
	// default_destiny_source from case.yml (file://, hermetic). Scenarios without
	// apply:destiny do not call resolver; service.yml without destiny[] — not an error.
	var deps []config.DependencyRef
	if svcManifest != nil {
		deps = svcManifest.Destiny
	}
	destiny := newFixtureDestinyResolver(serviceRootFor(caseFile), c.Fixtures.DefaultDestinySource, deps)

	// Effective input mirroring prod (scenario.run §4.5) through the SHARED gate
	// config.ResolveInputContract: merge defaults + required/required_when + value
	// validation, then the `validate:` invariants (ADR-009 amendment, DSL wave 2)
	// over the merged input. Same single composition the prod pre-flight gate
	// (scenario.ValidateInput) and the destiny render pass use — L0 cannot drift
	// from prod on what a contract means. The first failure aborts the case
	// (testable via expect_render_error).
	effectiveInput, err := config.ResolveInputContract(scn.Input, scn.Validate, c.Fixtures.Input)
	if err != nil {
		var fail *config.ValidateRuleFailure
		if errors.As(err, &fail) {
			return rc, fmt.Errorf("trial: validate %s: %s", scn.Name, fail.Error())
		}
		if errors.Is(err, config.ErrValidateRuleEval) {
			return rc, fmt.Errorf("trial: validate %s: %w", scn.Name, err)
		}
		return rc, fmt.Errorf("trial: input %s: %w", scn.Name, err)
	}

	// Templates: reader of .tmpl snapshot of case service (two-level resolution
	// scenario-local→service-level, ADR-009). serviceRoot — service root/
	// _trial wrapper; scenario-prefix `scenario/<name>` taken from scenario name.
	// readWithin clamps output beyond serviceRoot (securejoin), mirroring
	// prod snapshot.
	svcRoot := serviceRootFor(caseFile)
	templates := render.NewSnapshotTemplateReader(
		func(rel string) ([]byte, error) { return readWithin(svcRoot, rel) },
		"scenario/"+scn.Name,
	)

	in := render.RenderInput{
		Scenario:    scn,
		ServiceVars: orEmptyMap(c.Fixtures.Vars),
		Input:       effectiveInput,
		Register:    orEmptyMap(c.Mocks.Register),
		Incarnation: render.IncarnationMeta{Name: incarnationName(scn.Name, c.Fixtures), Service: svcName}, // NIM-58
		Hosts:       fixtureHosts(c.Fixtures),
		Destiny:     destiny,
		Templates:   templates,
		// State — fixtures.state as pre-run snapshot of incarnation.state: available in
		// CEL as `incarnation.state.<path>` (ADR-009/010), same as merge below
		// takes as stateBefore. nil (case without state) → key not declared
		// (`incarnation.state.x` = no-such-key), behavior of previous cases BIT-FOR-BIT.
		State: c.Fixtures.State,
	}

	// Mocks.Register — a single L0 payload for the probe (probe-per-host = dispatch
	// layer L3, outside pilot): the same register context applied to every host of
	// the roster by its SID. On a single-host roster exactly {trialHostSID:
	// register}.
	//
	// Seeded BEFORE the render, not after: `register.hosts.<name>` (NIM-711) is
	// projected from this map when a keeper task renders, so an L0 case could not
	// reach the accessor at all if it were filled afterwards. For host tasks nothing
	// changes — [render.hostRegister] unions the per-host bucket over an empty
	// keeper bucket, which is the flat Register content it already returned.
	mockReg := orEmptyMap(c.Mocks.Register)
	in.RegisterByHost = make(map[string]map[string]any, len(in.Hosts))
	for _, h := range in.Hosts {
		in.RegisterByHost[h.SID] = mockReg
	}

	tasks, _, err := pipeline.Render(ctx, in)
	if err != nil {
		return rc, fmt.Errorf("trial: render: %w", err)
	}

	rc.tasks = tasks
	rc.pipeline = pipeline
	rc.in = in
	rc.sink = sink
	return rc, nil
}

// RunCase runs single L0-case hermetically: loads scenario next to
// case.yml, builds render.RenderInput from fixtures, runs render pipeline with
// fixture-vault and coverage-sink, compares []RenderedTask with
// assert.rendered_tasks.
//
// caseFile — path to case.yml itself (from LoadCase). scenario/<name>/main.yml
// resolves as `<dir(case.yml)>/../../main.yml` (tests/<case>/case.yml).
func RunCase(ctx context.Context, c *Case, caseFile string) (Result, error) {
	res := Result{Case: c.Name}

	rc, err := renderCase(ctx, c, caseFile)
	// Recorded even when render failed: a case that aborted still fenced on a name, and
	// the report is what tells the operator which one.
	res.Service, res.ServiceStated = rc.service, rc.serviceStated

	// expect_render_error (ADR-023 amendment): case EXPECTS render abort
	// (assert failure / required_when). Render success → FAIL; error without substring
	// → FAIL; error with substring → PASS. Coverage empty (plan not built), but case
	// passes. Check raw error from renderCase before wrapping in return err below.
	if c.ExpectRenderError != "" {
		if err == nil {
			res.Failures = append(res.Failures, fmt.Sprintf("expected render abort with substring %q, but render succeeded", c.ExpectRenderError))
		} else if !strings.Contains(err.Error(), c.ExpectRenderError) {
			res.Failures = append(res.Failures, fmt.Sprintf("expected render error with substring %q, got: %v", c.ExpectRenderError, err))
		}
		res.Pass = len(res.Failures) == 0
		return res, nil
	}

	if err != nil {
		return res, err
	}
	pipeline, in, sink, tasks := rc.pipeline, rc.in, rc.sink, rc.tasks

	res.Failures = compareRenderedTasks(c.Assert.RenderedTasks, tasks)
	// Presence form (assert-by-presence, PILOT): coexists with positional —
	// both checks independent, case can carry any combination.
	res.Failures = append(res.Failures, compareTaskPresence(c.Assert.TaskPresent, c.Assert.TaskAbsent, tasks)...)

	in.Ctx = ctx

	// The `core.state.<verb>` steps of the plan, in the order they run
	// ([ADR-0084]). This is the offline half of the capture: prod runs each step
	// against the live row and the step writes there, L0 has no row and threads the
	// accumulating state through [stateop.Merge] instead. The op comes from the same
	// [stateop.OpsFromPlan] the run's own param checking sits behind, so a param
	// means here what it means in the run.
	//
	// Built ALWAYS, even when the case asserts no state: a capture step whose params
	// do not survive [stateop.CheckParams] is a defect the scenario carries whether
	// or not this case asserts state, and a case that asserts nothing would
	// otherwise hide it until the run. A step the render SKIPPED is not built —
	// [stateop.OpsFromPlan] drops the placeholder, which has no params to check and
	// no write to fold.
	//
	// What L0 does NOT reproduce is per-Passage granularity: the plan is rendered
	// once, so a step's `value:` is the one rendered against fixtures.state, not
	// against the state its predecessors just wrote. The divergence is a false green
	// that this harness cannot see; what catches it is the ordering guard
	// ([config.StaleStateRead], `state_stale_same_passage_read`), and that guard runs
	// in SOUL-LINT, not here — a project whose CI runs the trial without the linter
	// keeps the false green.
	captures, cerr := stateop.OpsFromPlan(tasks)
	if cerr != nil {
		return res, fmt.Errorf("trial: %w", cerr)
	}

	// assert.state_after — the deterministic final incarnation.state: base
	// fixtures.state, then the capture steps applied in plan order, which is the
	// order prod applies them in. The engine is the prod one ([stateop.Merge]), so
	// a verb cannot mean one thing here and another in the run.
	//
	// The check is a SUBSET ([ADR-0084] F-C/C4): a field the case does not name is
	// a field the case has no opinion about. Migration L1 keeps the full form.
	if c.Assert.StateAfter != nil || len(c.Assert.StateAbsent) > 0 {
		schema, serr := loadServiceStateSchema(caseFile)
		if serr != nil {
			return res, serr
		}
		matchEval, opEval := pipeline.StateOpEvaluators(ctx, in.Incarnation.Service)
		stateAfter, merr := stateop.Merge(c.Fixtures.State, captures, schema, matchEval, opEval)
		if merr != nil {
			return res, fmt.Errorf("trial: apply state writes: %w", merr)
		}
		if c.Assert.StateAfter != nil {
			res.Failures = append(res.Failures, compareStateSubset(c.Assert.StateAfter, stateAfter)...)
		}
		res.Failures = append(res.Failures, compareStateAbsent(c.Assert.StateAbsent, stateAfter)...)
	}

	res.Pass = len(res.Failures) == 0
	res.Coverage = sink.Report()
	return res, nil
}

// scenarioPathFor derives scenario/<name>/main.yml path from case.yml path.
// Layout ([ADR-023]/orchestration.md): scenario/<name>/tests/<case>/case.yml.
func scenarioPathFor(caseFile string) string {
	caseDir := filepath.Dir(caseFile)                  // .../tests/<case>
	scenarioDir := filepath.Dir(filepath.Dir(caseDir)) // .../scenario/<name>
	return filepath.Join(scenarioDir, "main.yml")
}

// serviceRootFor derives service directory from case.yml path. Layout:
// `<service-root>/scenario/<name>/tests/<case>/case.yml` (or for standalone
// L0 wrapper of destiny — `<destiny>/_trial/scenario/apply/tests/<case>/case.yml`,
// where service-root = `_trial/`). service.yml (if exists) lives in this directory.
func serviceRootFor(caseFile string) string {
	caseDir := filepath.Dir(caseFile)                  // .../tests/<case>
	scenarioDir := filepath.Dir(filepath.Dir(caseDir)) // .../scenario/<name>
	return filepath.Dir(filepath.Dir(scenarioDir))     // .../<service-root>
}

// trialServiceName is the service identity the render pipeline fences on
// ([ADR-0083] §7). Without it both halves of the fence are inert — the guard and
// the runtime scan both no-op on an empty service — and L0 would report a scenario
// green that a real run rejects.
//
// It came off the manifest's `name:` until NIM-726 removed that field. Offline there
// is no registry to ask, so the default is the service directory itself, which is what
// every in-tree service named itself after; `fixtures.service` overrides it.
//
// Be precise about what this buys. The guarantee is that the name is never EMPTY, so
// the fence cannot switch itself off in silence — that is the ticket's invariant, and
// it holds. It is NOT a guarantee that the name is right: the directory is a
// convention, not an authority, and a checkout laid out as the documented
// `service-<name>/` (docs/service/manifest.md) derives `service-redis` where the
// registry says `redis`. Such a name is non-empty and simply never matches, so the
// fence runs and finds nothing. A repository whose directory is not its registered
// name MUST state `fixtures.service:`; nothing offline can detect that it didn't,
// because there is no second source to compare against — which is the same reason
// the manifest copy this replaced was worth deleting.
//
// ABSOLUTE first, and this is not tidiness. serviceRootFor is four lexical Dir
// calls, so `scenario/create/tests/<case>/case.yml` — what soul-trial is handed
// when it is run from inside the service repo — decomposes to ".", and Base(".")
// is ".". A "." service is not empty, so it clears every `service == ""` guard and
// installs a fence that can never fire: PathAddressesOwnNamespace drops "." while
// splitting, so no path ever matches it. That is the silent fail-open this ticket
// removed, re-entering through the back door, and it would make the verdict a
// function of how the operator typed the path. soul-lint's scenarioServiceRoot
// carries the same fix for the same reason.
//
// A `_`-prefixed root is the standalone-destiny wrapper convention
// (`<destiny>/_trial/`, see serviceRootFor): the wrapper is not the identity, the
// destiny it wraps is, so the name comes from the level above. Otherwise the four
// in-tree wrappers would all be called `_trial` — one word, no service, shared by
// all of them.
func trialServiceName(caseFile string, f Fixtures) string {
	name, _ := trialServiceIdentity(caseFile, f)
	return name
}

// trialServiceIdentity is trialServiceName plus WHERE the name came from. The caller
// reports the derived case, because a derived name that is wrong is indistinguishable
// from a right one at this layer and the operator is the only one who can tell — see
// the godoc above. A stated name (`fixtures.service`) needs no report: someone decided.
func trialServiceIdentity(caseFile string, f Fixtures) (name string, stated bool) {
	if f.Service != "" {
		return f.Service, true
	}
	root := serviceRootFor(caseFile)
	if abs, err := filepath.Abs(root); err == nil {
		root = abs
	}
	if base := filepath.Base(root); !strings.HasPrefix(base, "_") {
		return base, false
	}
	return filepath.Base(filepath.Dir(root)), false
}

// loadTrialServiceManifest reads `<service-root>/service.yml` of case. Absence
// of file — not an error (nil, nil): standalone destiny wrappers live without manifest.
// Single loader for destiny[]-deps, state_schema and modules[] synthesis.
func loadTrialServiceManifest(caseFile string) (*config.ServiceManifest, error) {
	svcPath := filepath.Join(serviceRootFor(caseFile), "service.yml")
	if _, err := os.Stat(svcPath); errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	manifest, _, diags, err := config.LoadServiceManifest(svcPath, config.ValidateOptions{})
	if err != nil {
		return nil, fmt.Errorf("trial: loading service.yml %s: %w", svcPath, err)
	}
	if hasErrors(diags) {
		return nil, fmt.Errorf("trial: service.yml %s invalid: %s", svcPath, formatDiags(diags))
	}
	return manifest, nil
}

// loadServiceDestinyDeps — destiny[]-dependencies from service.yml (mirror of prod
// DestinySource.resolverFor). Without service.yml deps is empty, and first
// apply:destiny will be rejected as undeclared dependency, symmetric to prod.
func loadServiceDestinyDeps(caseFile string) ([]config.DependencyRef, error) {
	manifest, err := loadTrialServiceManifest(caseFile)
	if manifest == nil || err != nil {
		return nil, err
	}
	return manifest.Destiny, nil
}

// loadServiceStateSchema — state_schema-map from service.yml (collection type for
// add materialization, mirror of prod art.Manifest.StateSchema). Absence
// of service.yml / state_schema — not an error (nil): add to already existing
// collection derives type from state value.
//
// The `$type` references are resolved here, against the sibling types.yml, because
// prod resolves them at its own load ([artifact.ServiceLoader.parseManifest]) and this
// is the Trial twin of that load. Without it the twin diverges silently and in the
// worst direction: [stateop.Merge] ends in [config.StripDeclaredSecrets], which reads
// the element shape to find the declared secrets, and an unresolved `{$type: AclUser}`
// has none — so a password prod deletes from the record would survive into
// `assert.state_after` and the trial diff, and the case that pinned it would be
// pinning the wrong record.
//
// A broken catalog is not fatal here, unlike in prod: L0 lints a service tree that may
// legitimately be mid-edit, and the schema's own errors are reported by
// `soul-lint validate-service` at the file they belong to. The unresolved schema is
// used, which loses the secrets — the same "no schema, no check" asymmetry the offline
// half of the collection-key rule carries.
func loadServiceStateSchema(caseFile string) (config.InputSchemaMap, error) {
	manifest, err := loadTrialServiceManifest(caseFile)
	if manifest == nil || err != nil {
		return nil, err
	}
	if !config.SchemaHasTypeRef(manifest.StateSchema) {
		return manifest.StateSchema, nil
	}
	data, rerr := os.ReadFile(filepath.Join(serviceRootFor(caseFile), config.TypesCatalogFile))
	if rerr != nil && !errors.Is(rerr, fs.ErrNotExist) {
		return nil, fmt.Errorf("trial: reading %s: %w", config.TypesCatalogFile, rerr)
	}
	catalog, cdiags := config.ParseTypeCatalog(config.TypesCatalogFile, data)
	resolved, rdiags := config.ResolveStateSchemaTypeRefs(manifest.StateSchema, catalog)
	if hasErrors(cdiags) || hasErrors(rdiags) {
		return manifest.StateSchema, nil
	}
	return resolved, nil
}

// fixtureScenarioIncludeResolver — two-level scenario-include resolver for L0
// (orchestration.md §6): locally `scenario/<name>/<file>`, then service-level
// `scenario/<file>`. scnPath — path to scenario main.yml
// (`.../scenario/<name>/main.yml`). securejoin clamps output within base.
func fixtureScenarioIncludeResolver(scnPath string) config.IncludeResolver {
	// securejoin on relative base with leading `..` normalizes and loses upward exit
	// (see newFixtureDestinyResolver) — convert to absolute.
	if abs, err := filepath.Abs(scnPath); err == nil {
		scnPath = abs
	}
	scenarioDir := filepath.Dir(scnPath)            // .../scenario/<name>
	serviceScenarioDir := filepath.Dir(scenarioDir) // .../scenario
	return func(name string) ([]byte, string, error) {
		local := filepath.Join(scenarioDir, name)
		data, err := readWithin(scenarioDir, name)
		if err == nil {
			return data, local, nil
		}
		// On service-level fallback ONLY when local file is absent;
		// I/O error (permission denied, broken symlink) must not be masked.
		if !errors.Is(err, fs.ErrNotExist) {
			return nil, "", fmt.Errorf("include %q: reading locally (%s): %w", name, local, err)
		}
		service := filepath.Join(serviceScenarioDir, name)
		data, err = readWithin(serviceScenarioDir, name)
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				return nil, "", fmt.Errorf("include %q not found locally (%s) or at service-level (%s)", name, local, service)
			}
			return nil, "", fmt.Errorf("include %q: reading service-level (%s): %w", name, service, err)
		}
		return data, service, nil
	}
}

// readWithin reads name strictly within base (securejoin clamp).
func readWithin(base, name string) ([]byte, error) {
	full, err := securejoin.SecureJoin(base, name)
	if err != nil {
		return nil, err
	}
	return os.ReadFile(full)
}

// incarnationName — incarnation name of L0 run: fixtures.incarnation_name (override,
// NIM-58) or scenario name by default.
func incarnationName(scenarioName string, f Fixtures) string {
	if f.IncarnationName != "" {
		return f.IncarnationName
	}
	return scenarioName
}

// fixtureHosts builds roster of L0 run from fixtures.
//
// Multi-host (fixtures.hosts set): roster of N hosts in deterministic
// order by SID (soulprint.hosts projection of render engine goes in order of
// in.Hosts, does not sort itself — we ensure determinism here). Mirror
// of run topology: covens/role/choirs/soulprint taken from host entry as-is.
// A fixture's covens are the host's OWN tags, exactly as prod reads them off
// `souls.coven` (NIM-281) — an `on:` naming a tag the fixture does not declare
// drops the host, and belonging to the incarnation adds nothing back. The
// incarnation's NAME is not among them: membership lives in
// `incarnation_membership` (NIM-124) and the roster is already scoped to it, so
// "every member" is `on:` omitted, not `on: [<incarnation-name>]`.
//
// Single-host (fixtures.soulprint, multi not set): one synthetic host trial-host
// with NO covens, for the same reason — the sugar must not hand a host a label
// prod would never give it, or a case would pass here and target nothing live.
// Per-host variability — dispatch layer (L3, outside pilot).
func fixtureHosts(f Fixtures) []*topology.HostFacts {
	if len(f.Hosts) == 0 {
		return []*topology.HostFacts{{
			SID:       trialHostSID,
			Soulprint: orEmptyMap(f.Soulprint),
		}}
	}

	hosts := make([]*topology.HostFacts, 0, len(f.Hosts))
	for _, h := range f.Hosts {
		hosts = append(hosts, &topology.HostFacts{
			SID:       h.SID,
			Coven:     h.Covens,
			Role:      h.Role,
			Choirs:    h.Choirs,
			Soulprint: orEmptyMap(h.Soulprint),
		})
	}
	sort.Slice(hosts, func(i, j int) bool { return hosts[i].SID < hosts[j].SID })
	return hosts
}

func orEmptyMap(m map[string]any) map[string]any {
	if m == nil {
		return map[string]any{}
	}
	return m
}

// compareRenderedTasks compares expected tasks with rendered plan.
// Returns list of mismatches (empty = pass). Comparison by index: for each
// ExpectedTask takes RenderedTask with same Index.
func compareRenderedTasks(expected []ExpectedTask, got []*render.RenderedTask) []string {
	byIndex := make(map[int]*render.RenderedTask, len(got))
	for _, rt := range got {
		byIndex[rt.Index] = rt
	}

	var fails []string
	for _, et := range expected {
		rt, ok := byIndex[et.Index]
		if !ok {
			fails = append(fails, fmt.Sprintf("task index %d: expected in plan, but %d tasks rendered", et.Index, len(got)))
			continue
		}
		if rt.Module != et.Module {
			fails = append(fails, fmt.Sprintf("task index %d: module = %q, expected %q", et.Index, rt.Module, et.Module))
		}
		if et.Params != nil {
			if diff := compareParams(et.Index, et.Params, rt.Params); diff != "" {
				fails = append(fails, diff)
			}
		}
	}
	return fails
}

// compareTaskPresence implements assert-by-presence (PILOT of new L0 model):
// checks PRESENCE/ABSENCE of task call in plan, not position.
//
// task_present: for each entry in plan must be EXACTLY ONE (after
// disambiguation) matching task — 0 matches → fail «expected task,
// not found»; >1 match without when/id-disambiguator → fail collision with
// suggestion to narrow assert. task_absent: ≥1 match → fail.
//
// Match of one task — taskMatches (module== ∧ params_subset⊆params ∧ opt.when==
// ∧ opt.id==register∪id). params_subset checked by same compareParams as
// positional check (partial by-key, <present> marker), so subset semantics
// identical to Params in rendered_tasks.
func compareTaskPresence(present, absent []ExpectedTask, got []*render.RenderedTask) []string {
	var fails []string

	for i, et := range present {
		var matched []*render.RenderedTask
		for _, rt := range got {
			if taskMatches(et, rt) {
				matched = append(matched, rt)
			}
		}
		switch {
		case len(matched) == 0:
			fails = append(fails, fmt.Sprintf("task_present[%d]: expected task matching %s, in plan (%d tasks) not found",
				i, describeExpected(et), len(got)))
		case len(matched) > 1:
			fails = append(fails, fmt.Sprintf("task_present[%d]: %s — found %d matches (collision); add id/register or when, or narrow params_subset",
				i, describeExpected(et), len(matched)))
		}
	}

	for i, et := range absent {
		for _, rt := range got {
			if taskMatches(et, rt) {
				fails = append(fails, fmt.Sprintf("task_absent[%d]: %s — expected absence, but task found in plan",
					i, describeExpected(et)))
				break
			}
		}
	}

	return fails
}

// taskMatches — predicate of one rendered task matching presence
// expectation. All specified conditions are conjunctive; unspecified (empty) — do not
// restrict. params_subset matches by reusing compareParams (empty
// diff == match): it is partial by-key and understands <present> marker.
//
// Skip placeholder (disabled branch: static when:false / block-skip / loop-skip,
// and also future-passage stub staged-render) NEVER matches — neither as
// task_present nor as task_absent. Semantics «task not called»: present on it
// must not green, absent on it must not false-positive. Skip marker —
// `rt.Params == nil`: render of real module task always carries non-nil
// *structpb.Struct (renderParams returns Struct even on empty params), both
// skip constructors (staticSkipPlaceholder/loopSkipPlaceholder) and future-passage
// stub leave Params nil. No explicit bool Skip flag on RenderedTask, and
// FlowContext is uninformative — it is set even on real task.
func taskMatches(et ExpectedTask, rt *render.RenderedTask) bool {
	if rt.Params == nil {
		return false
	}
	if rt.Module != et.Module {
		return false
	}
	if et.When != "" && rt.When != et.When {
		return false
	}
	// id-disambiguator: register∪id (T1 — both at once forbidden in DSL, so
	// in one line address either of the two).
	if et.ID != "" && rt.Register != et.ID && rt.ID != et.ID {
		return false
	}
	if len(et.ParamsSubset) > 0 {
		if diff := compareParams(rt.Index, et.ParamsSubset, rt.Params); diff != "" {
			return false
		}
	}
	return true
}

// describeExpected — human-readable description of presence expectation for
// mismatch text. Does not print params_subset in full — only the identifying
// keys, which is all a reader needs to find the task the expectation missed.
func describeExpected(et ExpectedTask) string {
	var b strings.Builder
	fmt.Fprintf(&b, "module=%q", et.Module)
	if et.When != "" {
		fmt.Fprintf(&b, ", when=%q", et.When)
	}
	if et.ID != "" {
		fmt.Fprintf(&b, ", id=%q", et.ID)
	}
	if len(et.ParamsSubset) > 0 {
		fmt.Fprintf(&b, ", params_subset keys=%v", sortedKeys(et.ParamsSubset))
	}
	return b.String()
}

// compareFieldsByKey compares a want map against a got map BY KEY: every field
// named in want must exist in got and be deep-equal to it. Fields present only
// in got are NOT reported here — whether "extra" is a failure belongs to the
// caller ([compareState] adds that check, [compareStateSubset] deliberately does
// not). label prefixes every message with the assert section that spoke.
//
// Both sides are normalized through structpb (numbers → float64), as in
// compareParams, so a YAML-decoded assert and a CEL render output are compared in
// the same form.
func compareFieldsByKey(label string, want, got map[string]any) []string {
	wantStruct, err := structpb.NewStruct(want)
	if err != nil {
		return []string{fmt.Sprintf("assert.%s invalid: %v", label, err)}
	}
	gotStruct, err := structpb.NewStruct(got)
	if err != nil {
		return []string{fmt.Sprintf("%s not comparable: %v", label, err)}
	}
	wantMap := wantStruct.AsMap()
	gotMap := gotStruct.AsMap()

	var fails []string
	for _, field := range sortedKeys(wantMap) {
		gv, ok := gotMap[field]
		if !ok {
			fails = append(fails, fmt.Sprintf("%s.%s: expected, but the field is absent from the result", label, field))
			continue
		}
		if !deepEqualJSON(wantMap[field], gv) {
			fails = append(fails, fmt.Sprintf("%s.%s mismatch:\n    expected: %v\n    got:      %v", label, field, wantMap[field], gv))
		}
	}
	return fails
}

// compareStateSubset is the scenario-side `assert.state_after`: a case names the
// fields it has an opinion about and only those are compared ([ADR-0084] F-C/C4).
//
// It is deliberately NOT the full comparison [compareState] runs for a migration.
// A case forced to restate the whole post-run state to assert one field is a case
// that gets updated by pasting in whatever the run produced — which asserts
// nothing. Subset also survives a service gaining a state field, instead of
// reddening every case in the suite.
func compareStateSubset(want, got map[string]any) []string {
	return compareFieldsByKey("state_after", want, got)
}

// presentMarker — sentinel value for assert params: «key is present and carries
// non-empty string, exact value is NOT checked». Introduced for `template_content`
// of core.file.rendered step (A1, ADR-012(d)): at L0 important is fact of delivery
// of literal .tmpl content Keeper→Soul (handoff not broken), but exact character-by-character
// check of multiline template is fragile and checked at L2/Real-Linux E2E.
// Regression «template-path moved instead of content» caught by absence of
// template_content key in render (assert template-key does NOT enumerate).
// compareStateAbsent is the scenario-side `assert.state_absent`: every named
// field must be gone from the final state ([ADR-0084] F-C/C4).
//
// It exists because [compareStateSubset] cannot assert a removal — a field the
// case does not name is a field the case has no opinion about, which is precisely
// what an absence looks like. `core.state.unset`, a `remove` that empties a field
// and a declared secret stripped on the way out all end here.
//
// Absence is strict key absence. A field left in place holding null is reported
// WITH its value rather than accepted: a dropped key and a blanked one are two
// different states, and the message has to say which one the run produced or the
// case author is left guessing.
func compareStateAbsent(fields []string, got map[string]any) []string {
	var fails []string
	for _, field := range fields {
		v, ok := got[field]
		if !ok {
			continue
		}
		fails = append(fails, fmt.Sprintf("state_absent.%s: expected to be gone from the state, got: %v", field, v))
	}
	return fails
}

const presentMarker = "<present>"

// compareParams compares expected params with CEL-rendered *structpb.Struct.
// Comparison by-key (assert.params is partial: extra render keys do not fail case,
// symmetric to rendered_tasks). Each value compared via normalized
// Go form (structpb normalizes numbers to float64).
//
// presentMarker (`<present>`) in expected value weakens check to «key
// exists, non-empty string» (see presentMarker) — for template_content.
//
// Values are printed in full on FAIL. The `no_log` branch that used to hide them
// bought nothing here: trial is offline, so the only plaintext a rendered param
// can carry is a `fixtures.vault` literal the case author wrote into the same
// file as the assertion. A production secret never reaches this point — keeper
// resolves a `vault:` ref against a live Vault, and trial has none.
func compareParams(idx int, want map[string]any, got *structpb.Struct) string {
	wantStruct, err := structpb.NewStruct(want)
	if err != nil {
		return fmt.Sprintf("task index %d: assert.params invalid: %v", idx, err)
	}
	wantMap := wantStruct.AsMap()
	gotMap := map[string]any{}
	if got != nil {
		gotMap = got.AsMap()
	}

	var diffs []string
	for _, key := range sortedKeys(wantMap) {
		wv := wantMap[key]
		gv, ok := gotMap[key]
		if !ok {
			diffs = append(diffs, fmt.Sprintf("key %q: expected in params, absent", key))
			continue
		}
		if wv == presentMarker {
			s, isStr := gv.(string)
			if !isStr || s == "" {
				diffs = append(diffs, fmt.Sprintf("key %q: expected non-empty string (present), got %v", key, gv))
			}
			continue
		}
		if !deepEqualJSON(wv, gv) {
			diffs = append(diffs, key)
		}
	}
	if len(diffs) == 0 {
		return ""
	}
	return fmt.Sprintf("task index %d: params mismatch: %v\n    expected: %v\n    got:      %v", idx, diffs, wantMap, gotMap)
}

// sortedKeys — deterministic list of map keys, so a params diff is reported in
// a stable order.
func sortedKeys(m map[string]any) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
