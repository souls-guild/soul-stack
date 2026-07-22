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
}

// renderedCase — result of hermetic render pass of case: flat plan
// of tasks + reusable pipeline/RenderInput for subsequent folding
// of state_changes and coverage-sink with accumulated CEL branches. One per case run,
// shared by L0-assert (RunCase) and L2-execution (RunL2Case):
// both start with the same Keeper-side render plan.
type renderedCase struct {
	tasks    []*render.RenderedTask
	pipeline *render.Pipeline
	in       render.RenderInput
	sink     *coverageSink
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

	// Effective input mirroring prod (scenario.run §4.5): merge defaults
	// scenario `input:` + required + value validation. L0 now does not mask
	// absence of merge phase — case may provide only required input.
	effectiveInput, err := config.ResolveInputValues(scn.Input, c.Fixtures.Input)
	if err != nil {
		return rc, fmt.Errorf("trial: input %s: %w", scn.Name, err)
	}

	// validate: — declarative input invariants (ADR-009 amendment, DSL wave 2),
	// mirror of prod pre-flight gate (scenario.ValidateInput). Input-only eval over
	// merged input; first failure aborts case with same error as
	// required_when (testable via expect_render_error). no-op without validate section.
	if fail, evErr := config.EvalValidateRules(scn.Validate, effectiveInput); evErr != nil {
		return rc, fmt.Errorf("trial: validate %s: %w", scn.Name, evErr)
	} else if fail != nil {
		return rc, fmt.Errorf("trial: validate %s: %s", scn.Name, fail.Error())
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
		Essence:     orEmptyMap(c.Fixtures.Essence),
		Input:       effectiveInput,
		Register:    orEmptyMap(c.Mocks.Register),
		Incarnation: render.IncarnationMeta{Name: incarnationName(scn.Name, c.Fixtures)}, // NIM-58
		Hosts:       fixtureHosts(scn.Name, c.Fixtures),
		Destiny:     destiny,
		Templates:   templates,
		// State — fixtures.state as pre-run snapshot of incarnation.state: available in
		// CEL as `incarnation.state.<path>` (ADR-009/010), same as merge below
		// takes as stateBefore. nil (case without state) → key not declared
		// (`incarnation.state.x` = no-such-key), behavior of previous cases BIT-FOR-BIT.
		State: c.Fixtures.State,
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

	// Render state_changes.sets — mirror of prod (scenario.run §7.1,
	// RenderStateChanges after barrier). In L0 no dispatch/register accumulation, but
	// CEL folding render of sets is always run: unprotected `${ input.X }` on
	// optional-without-default input (CEL «no such key») is caught here without asserts —
	// this was a blind spot of harness seeing only tasks. Mocks.Register gives
	// `register.*` in sets same per-host register context as in `where:`.
	in.Ctx = ctx
	// Mocks.Register — single L0-payload probe (probe-per-host = dispatch layer L3,
	// outside pilot): same register context applied to each host
	// of roster by its SID. On single-host roster exactly {trialHostSID: register}
	// (back-compat bit-for-bit).
	mockReg := orEmptyMap(c.Mocks.Register)
	in.RegisterByHost = make(map[string]map[string]any, len(in.Hosts))
	for _, h := range in.Hosts {
		in.RegisterByHost[h.SID] = mockReg
	}

	// Render state_changes is ALWAYS run (like prod after barrier), even without
	// assert: unprotected `${ input.X }` on optional-without-default input (CEL «no
	// such key») is caught here — blind spot of harness seeing only tasks.
	ops, err := pipeline.RenderStateOps(in)
	if err != nil {
		return res, fmt.Errorf("trial: render state_changes: %w", err)
	}

	// assert.state_changes — projection of set operations (field→value, back-compat).
	if c.Assert.StateChanges != nil {
		res.Failures = append(res.Failures, compareStateChanges(c.Assert.StateChanges, setOpsProjection(ops))...)
	}

	// assert.state_after — deterministic final incarnation.state: base
	// fixtures.state + applied in order state_changes operations (mirror
	// of prod commit, run.go: mergeStateChanges(stateBefore, ops, schema, EvalStateMatch)).
	// Check is FULL (compareState, like L1): extra key — mismatch.
	if c.Assert.StateAfter != nil {
		schema, serr := loadServiceStateSchema(caseFile)
		if serr != nil {
			return res, serr
		}
		stateAfter, merr := mergeStateChanges(c.Fixtures.State, ops, schema, pipeline.EvalStateMatch, pipeline.EvalStateOpExpr)
		if merr != nil {
			return res, fmt.Errorf("trial: apply state_changes: %w", merr)
		}
		res.Failures = append(res.Failures, compareState(c.Assert.StateAfter, stateAfter)...)
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
func loadServiceStateSchema(caseFile string) (map[string]any, error) {
	manifest, err := loadTrialServiceManifest(caseFile)
	if manifest == nil || err != nil {
		return nil, err
	}
	return manifest.StateSchema, nil
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
// of run topology: covens/role/choirs/soulprint taken from host entry as-is;
// correctness of incarnation.name tag in covens — on case author
// (rosterSQL `WHERE $1 = ANY(coven)`: without it host drops from target).
//
// Single-host (fixtures.soulprint, multi not set): previous behavior
// BIT-FOR-BIT — one synthetic host trial-host with root incarnation tag
// (incarnationName == RenderInput.Incarnation.Name), per-host variability —
// dispatch layer (L3, outside pilot).
func fixtureHosts(incarnationName string, f Fixtures) []*topology.HostFacts {
	if len(f.Hosts) == 0 {
		return []*topology.HostFacts{{
			SID:       trialHostSID,
			Coven:     []string{incarnationName},
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
			if diff := compareParams(et.Index, et.Params, rt.Params, rt.NoLog); diff != "" {
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
		if diff := compareParams(rt.Index, et.ParamsSubset, rt.Params, rt.NoLog); diff != "" {
			return false
		}
	}
	return true
}

// describeExpected — human-readable description of presence expectation for
// mismatch text. Does not print params_subset in full (may carry vault secrets) —
// only keys, like no_log branch of compareParams.
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

// compareStateChanges compares expected assert.state_changes with rendered
// state_changes.sets (field → CEL-folded value). Returns list of
// mismatches (empty = match). Both sides normalized via structpb
// (numbers → float64), as in compareParams, so YAML decode of assert and CEL output
// of render are compared in same form. Extra fields in render (not mentioned in
// assert) are NOT mismatches — assert is partial, like rendered_tasks.
func compareStateChanges(want, got map[string]any) []string {
	wantStruct, err := structpb.NewStruct(want)
	if err != nil {
		return []string{fmt.Sprintf("assert.state_changes invalid: %v", err)}
	}
	gotStruct, err := structpb.NewStruct(got)
	if err != nil {
		return []string{fmt.Sprintf("state_changes not comparable: %v", err)}
	}
	wantMap := wantStruct.AsMap()
	gotMap := gotStruct.AsMap()

	var fails []string
	for _, field := range sortedKeys(wantMap) {
		gv, ok := gotMap[field]
		if !ok {
			fails = append(fails, fmt.Sprintf("state_changes.%s: expected in set, but field not rendered", field))
			continue
		}
		if !deepEqualJSON(wantMap[field], gv) {
			fails = append(fails, fmt.Sprintf("state_changes.%s mismatch:\n    expected: %v\n    got:      %v", field, wantMap[field], gv))
		}
	}
	return fails
}

// presentMarker — sentinel value for assert params: «key is present and carries
// non-empty string, exact value is NOT checked». Introduced for `template_content`
// of core.file.rendered step (A1, ADR-012(d)): at L0 important is fact of delivery
// of literal .tmpl content Keeper→Soul (handoff not broken), but exact character-by-character
// check of multiline template is fragile and checked at L2/Real-Linux E2E.
// Regression «template-path moved instead of content» caught by absence of
// template_content key in render (assert template-key does NOT enumerate).
const presentMarker = "<present>"

// compareParams compares expected params with CEL-rendered *structpb.Struct.
// Comparison by-key (assert.params is partial: extra render keys do not fail case,
// symmetric to rendered_tasks). Each value compared via normalized
// Go form (structpb normalizes numbers to float64).
//
// presentMarker (`<present>`) in expected value weakens check to «key
// exists, non-empty string» (see presentMarker) — for template_content.
//
// noLog — RenderedTask.NoLog flag: params may contain vault-resolved
// secrets, so on FAIL values are masked (only keys printed),
// to prevent secret leak to stdout/report.
func compareParams(idx int, want map[string]any, got *structpb.Struct, noLog bool) string {
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
	if noLog {
		return fmt.Sprintf("task index %d: params mismatch (values hidden, no_log):\n    keys: %v", idx, diffs)
	}
	return fmt.Sprintf("task index %d: params mismatch: %v\n    expected: %v\n    got:      %v", idx, diffs, wantMap, gotMap)
}

// sortedKeys — deterministic list of map keys (for no_log-diff, where
// values are hidden).
func sortedKeys(m map[string]any) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
