// Package trial implements a hermetic test runner for Trial/Destiny/Scenario
// ([ADR-023]), binary `soul-trial`. This package implements L0 level
// (render-only): execution of Keeper-side render pipeline (`keeper/internal/render`)
// on fixtures without host and external infrastructure, verification of rendered
// plan (`[]RenderedTask`) against `assert.rendered_tasks`, collection of trial
// coverage by CEL branches through cel.CoverageSink.
//
// L0 assert sections: rendered_tasks (flat task plan), state_after and
// state_absent (the deterministic final incarnation.state).
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
// Fixtures specify the entire hermetic context of the run (input/vars/soulprint/
// vault). Mocks.Register provides register context for probe steps in `where:`/`when:`
// (in L0 pilot, ready register payload is passed without probe execution).
// Assert is the expected result; L0 verifies RenderedTasks, StateAfter and
// StateAbsent (see AssertBlock).
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
// only for assert.state_after, where the expected outcome = State + what the
// scenario's `core.state.<verb>` steps write; for create scenarios (state "from
// scratch") omitted.
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
	Vars                 map[string]any            `yaml:"vars,omitempty"`
	Soulprint            map[string]any            `yaml:"soulprint,omitempty"`
	Hosts                []HostFixture             `yaml:"hosts,omitempty"`
	Vault                map[string]map[string]any `yaml:"vault,omitempty"`
	State                map[string]any            `yaml:"state,omitempty"`
	DefaultDestinySource string                    `yaml:"default_destiny_source,omitempty"`

	// IncarnationName overrides incarnation name for L0 (NIM-58 guard-tests);
	// empty → scenario name (scn.Name), previous BIT-EXACT behavior.
	IncarnationName string `yaml:"incarnation_name,omitempty"`

	// Service is the name L0 fences on ([ADR-0083] §7) — the name the case pretends
	// the service is registered under. Empty → the service directory's own name
	// (NIM-726: the manifest no longer states one, and offline there is no registry
	// to ask). Set it only when the directory is not the registered name; the fence
	// is never disabled by leaving it out.
	Service string `yaml:"service,omitempty"`
}

// HostFixture is one entry in the multi-host roster of L0 (`fixtures.hosts[]`).
// Mirror of stable fields from topology.HostFacts, visible in render
// (`soulprint.hosts[]`): sid/covens/role/soulprint/choirs.
//
// SID is required; Covens are the host's OWN tags and nothing else — the exact
// set prod reads off `souls.coven` (NIM-281). Membership adds none of them: the
// roster comes from `incarnation_membership` (NIM-124) and is already scoped to
// the incarnation, so "every member" is `on:` omitted, and `on: [<tag>]` reaches
// only the hosts a fixture tagged by hand. Never list the incarnation's name
// here — no host carries it live, and a case that did would target nothing in
// prod. Role/Soulprint/Choirs are optional. Order of roster in `soulprint.hosts`
// is deterministic via sorting by SID (harness, not YAML order).
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
// optional ([ADR-023]); L0 implements RenderedTasks, StateAfter and StateAbsent.
//
// StateAfter is the expected final `incarnation.state` after the run: base
// `fixtures.state` with the scenario's `core.state.<verb>` writes applied on top,
// in plan order (mirror of the prod run, [ADR-0084]). Hermetic, without a host
// (L0).
//
// Verification is a SUBSET ([ADR-0084] F-C/C4): the case names the fields it
// asserts and only those are compared; a field present in the result that the
// case does not mention is not a mismatch. Whole-state equality would force
// every case to restate fields it has no opinion about, and a case that must
// restate the world to assert one field is a case that gets updated by pasting
// in whatever the run produced — which asserts nothing. It also survives a
// service gaining a state field instead of reddening the whole suite.
//
// The L1 migration form (`state_before:`/`state_after:` at case top level, not
// this section) keeps the COMPLETE comparison: a migration is a pure function of
// the old state, so an unpredicted field there IS the defect (see compareState).
//
// StateAbsent is the other half of that subset: the fields the run must NOT
// leave in `incarnation.state`. Without it a subset check has no way to assert a
// REMOVAL — `core.state.unset` and a `remove` that empties a field both produce
// an absence, and an absence is exactly what naming no field says nothing about.
// The check is strict key absence: a field present but null is reported, with its
// value, rather than counted as gone — dropping a key and blanking it are two
// different states, and a case that meant the second one says so in StateAfter.
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

	StateAfter  map[string]any `yaml:"state_after,omitempty"`
	StateAbsent []string       `yaml:"state_absent,omitempty"`
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
