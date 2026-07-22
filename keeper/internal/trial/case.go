// Package trial implements a hermetic test runner for Trial/Destiny/Scenario
// ([ADR-023]), binary `soul-trial`. This package implements L0 level
// (render-only): execution of Keeper-side render pipeline (`keeper/internal/render`)
// on fixtures without host and external infrastructure, verification of rendered
// plan (`[]RenderedTask`) against `assert.rendered_tasks`, collection of trial
// coverage by CEL branches through cel.CoverageSink.
//
// L0 assert sections: rendered_tasks (flat task plan), state_changes
// (rendered sets), and state_after (deterministic final incarnation.state).
// assert.dispatch is an L3 level section (multi-host orchestration, [ADR-023]);
// on single-host there is one synthetic host, so it is meaningful only on
// multi-host and is not implemented in L0 (strict-decode rejects it as
// unknown-key — this is intentional, see AssertBlock).
//
// [ADR-023]: docs/adr/0023-trial-test-runner.md
package trial

// Case is one trial file `case.yml` ([ADR-023], format is an extension of
// migration template). Structure is read-only after loading.
//
// Fixtures specify the entire hermetic context of the run (input/essence/soulprint/
// vault). Mocks.Register provides register context for probe steps in `where:`/`when:`
// (in L0 pilot, ready register payload is passed without probe execution).
// Assert is the expected result; L0 verifies RenderedTasks, StateChanges, and
// StateAfter (see AssertBlock).
type Case struct {
	Name     string      `yaml:"name"`
	Fixtures Fixtures    `yaml:"fixtures"`
	Mocks    Mocks       `yaml:"mocks,omitempty"`
	Assert   AssertBlock `yaml:"assert,omitempty"`

	// ExpectRenderError indicates the case EXPECTS Keeper-side render to ABORT
	// with an error containing this substring (ADR-023 amendment 2026-06-23).
	// Enables fail-cases for mechanisms that fail during render: assert: (ADR-009
	// amendment) and future required_when. Render-success when ExpectRenderError is
	// set → FAIL case; render-error without substring → FAIL; render-error with
	// substring → PASS.
	//
	// Mutually exclusive with assert.rendered_tasks: "expect abort" and "expect plan"
	// are opposite outcomes (validate rejects both in one case). When ExpectRenderError
	// is set, assert section is empty (no plan). Optional (omitempty): normal L0
	// cases do not carry it — render path is BIT-EXACT.
	ExpectRenderError string `yaml:"expect_render_error,omitempty"`
}

// Fixtures is the hermetic input for a run. All fields are optional; empty field
// = empty context for the corresponding CEL variable.
//
// Soulprint in L0 is facts for ONE host (map `soulprint.self.<path>`),
// single-host sugar: harness builds roster from one synthetic host.
// Hosts is multi-host roster for the run (N hosts, render-invariants of topology:
// `soulprint.hosts`/`.where(...)`/`size()`/nodes-determinism). Soulprint and Hosts
// are MUTUALLY EXCLUSIVE: both in one case → strict-error (validate), in the spirit
// of strict-decode harness. Corresponds to L0 render-only level — which host
// actually executes (dispatch) remains L3 ([ADR-023] amendment 2026-06-22).
//
// Vault is a mock for vault resolution: key = logical path of secret (`secret/<...>`),
// value = map of secret fields (KV v2 `data.data` form).
//
// State is the base `incarnation.state` BEFORE scenario execution (for operations
// that accumulate state on top of existing — add_user/update_acl/…). Needed
// only for assert.state_after, where expected outcome = State + rendered
// state_changes.sets; for create scenarios (state "from scratch") omitted.
//
// DefaultDestinySource is L0 analog of keeper.yml::default_destiny_source (same
// key name): URL template with {name} substitution, by which apply:destiny
// resolves destiny dependencies. In L0, source must be hermetic
// (`file://` scheme, path relative to case service-root, e.g.
// `file://../../destiny/{name}`); per-entry `destiny[].git` override wins
// template, but in L0 must also be `file://`. Empty value is allowed for
// cases without apply:destiny — resolver is then not called.
type Fixtures struct {
	Input                map[string]any            `yaml:"input,omitempty"`
	Essence              map[string]any            `yaml:"essence,omitempty"`
	Soulprint            map[string]any            `yaml:"soulprint,omitempty"`
	Hosts                []HostFixture             `yaml:"hosts,omitempty"`
	Vault                map[string]map[string]any `yaml:"vault,omitempty"`
	State                map[string]any            `yaml:"state,omitempty"`
	DefaultDestinySource string                    `yaml:"default_destiny_source,omitempty"`

	// IncarnationName overrides incarnation name for L0 (NIM-58 guard-tests);
	// empty → scenario name (scn.Name), previous BIT-EXACT behavior.
	IncarnationName string `yaml:"incarnation_name,omitempty"`
}

// HostFixture is one entry in the multi-host roster of L0 (`fixtures.hosts[]`).
// Mirror of stable fields from topology.HostFacts, visible in render
// (`soulprint.hosts[]`): sid/covens/role/soulprint/choirs.
//
// SID is required; Covens must carry incarnation.name label (scenario name
// of the case) — mirror of prod-roster (`rosterSQL WHERE $1 = ANY(coven)`),
// otherwise host does not get into `on:`/`where:` target. Role/Soulprint/Choirs
// are optional. Order of roster in `soulprint.hosts` is deterministic via
// sorting by SID (harness, not YAML order).
type HostFixture struct {
	SID       string         `yaml:"sid"`
	Covens    []string       `yaml:"covens,omitempty"`
	Role      string         `yaml:"role,omitempty"`
	Soulprint map[string]any `yaml:"soulprint,omitempty"`
	Choirs    []string       `yaml:"choirs,omitempty"`
}

// Mocks provides mocks for steps that interact with host environment.
//
// Register is a map register-name → payload of probe step. In L0 pilot,
// payload is substituted as ready register result for `where:`/`when:`
// without probe execution (probe is Soul-side, L0 has no host).
type Mocks struct {
	Register map[string]any `yaml:"register,omitempty"`
}

// AssertBlock is the expected result of a run. Subsections are independent and
// optional ([ADR-023]); L0 implements RenderedTasks, StateChanges, and
// StateAfter.
//
// StateChanges is the expected result of rendering `state_changes.sets` of
// scenario (field → value after CEL reduction, symmetric to RenderedTasks for
// tasks). Optional: even without this section state_changes are ALWAYS rendered
// during case execution, and render error (e.g., unguarded `${ input.X }` with
// optional-without-default input → CEL "no such key") is case failure. Section
// is needed when you want to fix specific values, not just the fact of
// successful render.
//
// StateAfter is the expected deterministic final `incarnation.state` after
// run: base `fixtures.state` + rendered `state_changes.sets`
// (mirror of prod commit, orchestration.md §7.1). Hermetic, without host (L0).
// Verification is COMPLETE (like L1-migration): extra key in result is also
// mismatch, state is fixed entirely. Optional: case chooses state_after when
// the final state fact matters, not just delta sets (state_changes is
// partial delta verification, state_after is full result verification).
type AssertBlock struct {
	RenderedTasks []ExpectedTask `yaml:"rendered_tasks,omitempty"`

	// TaskPresent/TaskAbsent is assert-by-presence form (PILOT of new L0 model,
	// user decision 2026-06-24): test checks PRESENCE/ABSENCE of task invocation
	// in plan, not its POSITION. Coexists with positional rendered_tasks during
	// migration (no mutual exclusion — forms are independent, see compareTaskPresence).
	// For each TaskPresent record, plan must have ≥1 task matching {module== ∧
	// params_subset⊆params ∧ optional.when== ∧ optional.id==(register∪id)};
	// 0 matches → fail. For TaskAbsent — ≥1 match → fail. Disambiguators when/id
	// resolve >1 match collision for task_present.
	TaskPresent []ExpectedTask `yaml:"task_present,omitempty"`
	TaskAbsent  []ExpectedTask `yaml:"task_absent,omitempty"`

	StateChanges map[string]any `yaml:"state_changes,omitempty"`
	StateAfter   map[string]any `yaml:"state_after,omitempty"`
	// Dispatch is an L3 level section (multi-host orchestration, [ADR-023]): on
	// single-host there is one synthetic host, dispatch-plan is meaningful only on
	// topology. Field is NOT declared so strict-decode rejects cases relying on
	// it with explicit unknown-key error, not silent skip (test —
	// TestLoadCase_RejectsUnknownSection).
}

// ExpectedTask is an expectation for one rendered task in the plan. Serves both
// forms of L0 task assert: positional (assert.rendered_tasks, by Index) and
// presence (assert.task_present/task_absent, by attribute match).
//
// Positional form: Index is position in scenario.tasks[] (link to
// RenderedTask.Index). Module is expected module address. Params is expected
// CEL-rendered params (deep-compare). Params are optional: if not set,
// only index+module are verified. ParamsSubset/When/ID are not used in this form.
//
// Presence form: Index is ignored (position not verified). Module is required —
// module address of sought task. ParamsSubset is a SUBSET of expected params
// (by-key, supports <present> marker; extra render keys do not prevent
// match — same semantics as Params in positional verify). When/ID are
// optional DISAMBIGUATORS on multiple match collisions: When is
// exact equality of CEL string RenderedTask.When; ID is equality of
// RenderedTask.Register OR RenderedTask.ID (register∪id, T1).
type ExpectedTask struct {
	Index  int            `yaml:"index"`
	Module string         `yaml:"module"`
	Params map[string]any `yaml:"params,omitempty"`

	ParamsSubset map[string]any `yaml:"params_subset,omitempty"`
	When         string         `yaml:"when,omitempty"`
	ID           string         `yaml:"id,omitempty"`
}
