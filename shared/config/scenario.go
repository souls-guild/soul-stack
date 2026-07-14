package config

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"github.com/goccy/go-yaml"
	"github.com/goccy/go-yaml/ast"

	"github.com/souls-guild/soul-stack/shared/diag"
)

// ScenarioManifest is the typed representation of `scenario/<name>/main.yml` per
// the normative spec [`docs/scenario/orchestration.md`].
//
// It holds the scenario name/description, the `input:` contract (docs/input.md),
// the `state_changes` declaration (what the scenario writes to `incarnation.state`
// after a successful apply, §2), and the `tasks[]` list.
//
// The task DSL core is inherited from destiny (ADR-009): scenario supports all 22
// keys from docs/destiny/tasks.md plus the scenario delta (on/where/serial/
// run_once). Polymorphic task decode (module / apply / include / block) lives in
// scenario_task.go.
type ScenarioManifest struct {
	Name         string         `yaml:"name"`
	Description  string         `yaml:"description,omitempty"`
	Input        InputSchemaMap `yaml:"input,omitempty"`
	Validate     []ValidateRule `yaml:"validate,omitempty"`
	StateChanges *StateChanges  `yaml:"state_changes,omitempty"`
	Compute      ComputeBlock   `yaml:"compute,omitempty"`
	Vars         map[string]any `yaml:"vars,omitempty"`
	Tasks        []Task         `yaml:"tasks"`

	// Extends names a covenant fragment at the service-repo root (`covenant.yml`
	// without extension, base name `^[a-z][a-z0-9-]*$`) whose input/compute/
	// state_changes/validate sections the scenario inherits (covenant.go). Empty /
	// absent = no inheritance (forward-compat: existing scenarios without extends
	// are unaffected). Resolving the fragment against the snapshot FS is keeper-side
	// (S2 LoadScenarioManifest Resolved); the config layer gives types, mergeSections
	// and form validation.
	Extends string `yaml:"extends,omitempty"`

	// Create optionally marks a scenario as a bootstrap starter for a new
	// incarnation. `*bool` distinguishes "unset" (nil → not a starter, an ordinary
	// operational scenario) from an explicit `create: true|false`. A service may
	// declare SEVERAL create scenarios (`create_standalone`, `create_cluster`) from
	// which the operator picks one at `POST /v1/incarnations`; the default is the
	// scenario named `create` (back-compat). Compatible with auto-discover (ADR-029):
	// the create set is the subset of auto-discovered `scenario/<name>/` flagged here.
	// Read directly via `Create` (nil-safe: nil OR `false` → not a create starter).
	// `destroy` is NOT flagged here (teardown is a separate DELETE flow).
	Create *bool `yaml:"create,omitempty"`

	// FromVersions is the self-describing list of source versions an upgrade
	// scenario (`upgrade/<slug>/main.yml`) can upgrade from. Symmetric with
	// `create: true` (a self-describing discriminator inside the file); empty/absent
	// = not an upgrade scenario. YAML key `from` (Go name differs to avoid colliding
	// with artifact.StateSchemaMigration.From). ADR-0068.
	FromVersions []string `yaml:"from,omitempty"`

	// Form is the optional presentation layer for the `input:` form (form_layout.go):
	// how the UI groups/labels input fields into sections. nil = absent (UI renders
	// input flat, forward-compat). Does not affect the input contract or validation.
	Form *FormLayout `yaml:"form,omitempty"`
}

// ValidateRule is one rule of the top-level scenario `validate:` section (ADR-009
// amendment 2026-06-23, DSL wave 2). Declarative input validation ("must be X, not
// Y") instead of scattered assert tasks: a list `[{that, message}]` where each
// `that` is a CEL bool predicate (whole string = CEL, like `where:`/`assert.that`)
// and `message` is the human-readable reason for `that == false`.
//
// RULE CONTEXT IS INPUT-ONLY: the env carries the single variable `input` (the same
// narrow cel-go sandbox as `required_when` — input_required_when.go). validate:
// covers INPUT INVARIANTS (cross-field preconditions not expressible by a single
// schema key — e.g. "`port` is required when `tls` is off"). Referencing
// essence/soulprint/register/vault in `that` → compile-time undeclared-reference
// error (a structural barrier, not a textual guard). Topology/roster checks stay
// with `assert:` (which has the full scenario CEL context with soulprint.hosts);
// validate: COMPLEMENTS, it does not replace assert or required_when.
//
// WHEN: pre-flight on CreateTyped/RunTyped (request path) — the first failing rule
// yields HTTP 422 validation_failed BEFORE the incarnation commit and BEFORE
// applying (like required_when and pre-flight assert, WITHOUT error_locked).
// Evaluation is deterministic from input (config.EvalValidateRules), so the
// two-point render-fail-safe is unneeded (input does not change between the request
// path and goroutine start, unlike assert's roster).
type ValidateRule struct {
	That    string `yaml:"that"`
	Message string `yaml:"message"`
}

// ComputeBlock holds scenario-level computed variables (`compute:`, ADR-009
// amendment 2026-06-23). Each entry is `<name>: <CEL-expression>`: Keeper resolves
// it ONCE per run in the RUN-LEVEL scenario context (input/essence/incarnation/
// register), then the result is available as `compute.<name>` in both `apply.input`
// and `state_changes` (cel_render.resolveCompute).
//
// Purpose: remove duplication of a shared expression otherwise written twice
// (apply.input and state_changes do not see task-level `vars:`) — declare a big
// merge() once and reference `${ compute.<name> }`.
//
// Isolation barrier (architect aebb2d39 §5):
//   - compute does NOT leak into the isolated destiny pass (destiny sees only the
//     result via apply.input — RenderInput.Compute is not forwarded there, ADR-009 V2);
//   - compute's resolve context is RUN-LEVEL (WITHOUT soulprint.self/soulprint.hosts):
//     compute is host-invariant by construction, so the same value correctly flows
//     to apply.input (resolved on targeted[0]) and to state_changes (per-run, not
//     per-host). Referencing soulprint.* in compute → CEL no-such-key (a structural
//     barrier, not a textual guard).
//
// Declaration order matters: compute[i] may reference an earlier compute[j] (j < i)
// as `${ compute.<name_j> }`. Hence it is stored as an ordered list (not a map:
// decode preserves YAML key order).
type ComputeBlock []ComputeVar

// ComputeVar is one entry of the `compute:` block (name + CEL expression). Value is
// a CEL string (`${ … }` interpolation OR a native expression), resolved by
// cel_render.resolveCompute. A literal (number/bool/collection) is also allowed —
// non-string passes through as in `vars:`.
type ComputeVar struct {
	Name  string
	Value any
}

// StateChanges declares the `incarnation.state` mutations a scenario commits after
// a successful cross-host barrier (orchestration.md §7).
//
// Two decode forms (UnmarshalYAML discriminates by YAML node kind):
//
//   - NEW list form (sequence): `state_changes:` is an ordered list of verb
//     operations (`- set:` / `- add:` / `- modify:` / `- remove:` / `- foreach:`)
//     applied in declaration order to the intermediate state. Decoded into `Ops`
//     (see StateChange); `IsList` = true. The target grammar of ADR-057 (all verbs
//     implemented).
//   - OLD map form (mapping, DEPRECATED): `state_changes: { sets: {...},
//     appends: [...], modifies: [...] }`. Kept for backward-compat of existing
//     scenarios — decoded into `Sets`/`Appends`/`Modifies`, `IsList` = false. Same
//     semantics as before (orchestration.md §7.1): `Sets` is a map
//     `<field> → <CEL-expression>`; cross-host fold is last-wins by SID.
//     `Appends`/`Modifies` are not applied by the engine (a historical placeholder).
//
// An empty block is valid in either form (state unchanged): `state_changes: {}`
// (old) or `state_changes: []` (new).
type StateChanges struct {
	// IsList discriminates the form (true for the new list form). Render/merge branch
	// on it: list → ordered Ops; map → legacy Sets-overwrite.
	IsList bool `yaml:"-"`

	// Ops is the ordered operation list of the new list form. nil/empty in map form.
	Ops []StateChange `yaml:"-"`

	// Sets/Appends/Modifies are the old map form (DEPRECATED). nil in list form.
	Sets     map[string]string `yaml:"sets,omitempty"`
	Appends  []string          `yaml:"appends,omitempty"`
	Modifies []string          `yaml:"modifies,omitempty"`
}

// UnmarshalYAML decodes `compute:` as a mapping `<name>: <expression>` into an
// ORDERED ComputeVar list (YAML key order preserved — compute[i] may reference an
// earlier compute[j], j<i). Value is a CEL string or a literal (non-string passes
// through as in `vars:`). A non-mapping node (scalar/sequence) → empty block:
// validateComputeBlock raises type_mismatch by yaml_path. A key without a value /
// an empty key is skipped (the validator raises the diagnostic).
func (c *ComputeBlock) UnmarshalYAML(node ast.Node) error {
	mm, ok := node.(*ast.MappingNode)
	if !ok {
		return nil
	}
	out := make(ComputeBlock, 0, len(mm.Values))
	for _, kv := range mm.Values {
		tok := kv.Key.GetToken()
		if tok == nil || tok.Value == "" {
			continue
		}
		out = append(out, ComputeVar{Name: tok.Value, Value: nodeToAny(kv.Value)})
	}
	*c = out
	return nil
}

// StateVerb is the verb of one state_changes operation (new list form).
type StateVerb string

const (
	// VerbSet overwrites a field wholesale (semantics of the old `sets`).
	VerbSet StateVerb = "set"
	// VerbAdd adds an element to a collection (map/list) idempotently.
	VerbAdd StateVerb = "add"
	// VerbModify patches ALL collection elements matching Match (all-by-default;
	// orchestration.md §7.1).
	VerbModify StateVerb = "modify"
	// VerbRemove removes ALL collection elements matching Match.
	VerbRemove StateVerb = "remove"
	// VerbForeach fans out N operations from a CEL list/map (render-time, the form
	// from migration-DSL ADR-019). Expands into N RenderedOp before merge.
	VerbForeach StateVerb = "foreach"
)

// Expect is an optional runtime assert on match cardinality in modify/remove
// (ADR-057 §c). DEFAULT (empty) = ExpectAny (any count matched, including zero).
type Expect string

const (
	// ExpectAny is any number of matched elements (DEFAULT). An empty op.Expect is
	// treated as ExpectAny.
	ExpectAny Expect = "any"
	// ExpectOne is exactly one matched element (else error_locked before commit).
	ExpectOne Expect = "one"
	// ExpectAtMostOne is zero or one matched element.
	ExpectAtMostOne Expect = "at_most_one"
)

// OnConflict is the idempotency policy for `add` on an identity match.
type OnConflict string

const (
	// OnConflictSkip — an element with this identity already exists → no-op (DEFAULT).
	OnConflictSkip OnConflict = "skip"
	// OnConflictReplace overwrites the existing element with the new value.
	OnConflictReplace OnConflict = "replace"
	// OnConflictError fails the run (error_locked, state not committed).
	OnConflictError OnConflict = "error"
)

// StateChange is one operation of the ordered `state_changes` list (new list form).
// The verb determines which fields are significant:
//
//   - set:     Field + Value (overwrite the field wholesale);
//   - add:     Field + Value (+ Key for a map collection / Match|Key for list dedup,
//   - OnConflict skip|replace|error, default skip);
//   - modify:  Field + Match + Patch (+ optional Expect) — patch all matching;
//   - remove:  Field + Match (+ optional Expect) — remove all matching;
//   - foreach: In (CEL list/map) + As (binding name) + Do (nested verbs) —
//     render-time fan-out of N operations (the form from migration-DSL ADR-019).
//     Foreach carries its target field with Field=="" (the `foreach:` verb points
//     not at a collection but at the CEL collection expression to iterate, held in In).
//
// Value/Patch is an arbitrary YAML value: a CEL string (`${ … }`), a literal, or a
// nested object/list with CEL strings in cells (rendered recursively Keeper-side).
// Key/Match are CEL strings (element identity/predicate).
type StateChange struct {
	Verb  StateVerb
	Field string

	Value      any
	Key        string
	Match      string
	OnConflict OnConflict

	// Patch — map of path-in-element → CEL/literal (modify only). Merge-time: each
	// value is evaluated over the per-host scenario context + the current element's
	// bindings (elem/key/value). A dotted path (`config.maxmemory`) is a nested merge,
	// not a wholesale record overwrite (ADR-057 §a).
	Patch any
	// Expect — optional match-cardinality assert (modify/remove). "" → ExpectAny.
	Expect Expect

	// foreach: In is the CEL collection expression to iterate (`${ … }`); As is the
	// current-element binding name; Do are the nested operations applied each
	// iteration with the As binding active.
	In string
	As string
	Do []StateChange
}

// stateOpVerbs are the known operation verbs (discriminator in decode/validation).
// `expect` is NOT a verb — it is a modify/remove parameter (ADR-057 §c).
var stateOpVerbs = map[string]StateVerb{
	"set":     VerbSet,
	"add":     VerbAdd,
	"modify":  VerbModify,
	"remove":  VerbRemove,
	"foreach": VerbForeach,
}

// UnmarshalYAML DUAL-PARSEs StateChanges by YAML node kind:
//
//   - SequenceNode → new list form: decode each mapping element into a StateChange
//     (by verb key), set IsList=true. Structural validation (required/inapplicable
//     keys per verb) is raised by validateStateChanges over the AST — here only
//     value decode.
//   - MappingNode → old map form (DEPRECATED): the former sets/appends/modifies
//     decode path (reusing setsFromNode/stringSeqFromNode).
//   - other (scalar/null) → zero-value (walker/validator raises the diagnostic).
//
// A wrongly-shaped node inside an element is skipped without panic —
// validateStateChanges raises a meaningful diagnostic by yaml_path.
func (s *StateChanges) UnmarshalYAML(node ast.Node) error {
	switch n := node.(type) {
	case *ast.SequenceNode:
		s.IsList = true
		s.Ops = make([]StateChange, 0, len(n.Values))
		for _, item := range n.Values {
			if op, ok := stateOpFromNode(item); ok {
				s.Ops = append(s.Ops, op)
			}
		}
		return nil
	case *ast.MappingNode:
		for _, kv := range n.Values {
			tok := kv.Key.GetToken()
			if tok == nil {
				continue
			}
			switch tok.Value {
			case "sets":
				s.Sets = setsFromNode(kv.Value)
			case "appends":
				s.Appends = stringSeqFromNode(kv.Value)
			case "modifies":
				s.Modifies = stringSeqFromNode(kv.Value)
			}
		}
		return nil
	default:
		// state_changes: <scalar/null> — zero-value (validator raises type_mismatch).
		return nil
	}
}

// stateOpFromNode decodes one list-form element (a mapping with a verb key) into a
// StateChange. The verb key (`set`/`add`/…) carries the target Field; the other keys
// (`value`/`key`/`match`/`on_conflict`/`patch`/`in`/`as`/`do`) are op parameters. A
// non-mapping element / a missing verb → (zero, false): the validator raises the
// diagnostic by yaml_path.
func stateOpFromNode(node ast.Node) (StateChange, bool) {
	mm, ok := node.(*ast.MappingNode)
	if !ok {
		return StateChange{}, false
	}
	var op StateChange
	var hasVerb bool
	for _, kv := range mm.Values {
		tok := kv.Key.GetToken()
		if tok == nil {
			continue
		}
		key := tok.Value
		if verb, isVerb := stateOpVerbs[key]; isVerb {
			op.Verb = verb
			// `foreach:` carries the CEL collection expression (→ In); the other
			// verbs carry the target field name (→ Field). orchestration.md §7.1.
			if verb == VerbForeach {
				op.In = stringFromNode(kv.Value)
			} else {
				op.Field = stringFromNode(kv.Value)
			}
			hasVerb = true
			continue
		}
		switch key {
		case "value":
			op.Value = nodeToAny(kv.Value)
		case "key":
			op.Key = stringFromNode(kv.Value)
		case "match":
			op.Match = stringFromNode(kv.Value)
		case "on_conflict":
			op.OnConflict = OnConflict(stringFromNode(kv.Value))
		case "patch":
			op.Patch = nodeToAny(kv.Value)
		case "expect":
			op.Expect = Expect(stringFromNode(kv.Value))
		case "as":
			op.As = stringFromNode(kv.Value)
		case "do":
			if seq, isSeq := kv.Value.(*ast.SequenceNode); isSeq {
				for _, sub := range seq.Values {
					if subOp, okSub := stateOpFromNode(sub); okSub {
						op.Do = append(op.Do, subOp)
					}
				}
			}
		}
	}
	return op, hasVerb
}

// stringFromNode extracts a node's string value (for verb-Field, key, match,
// on_conflict). Non-string → "" (the validator raises type_mismatch).
func stringFromNode(node ast.Node) string {
	if sn, ok := node.(*ast.StringNode); ok {
		return sn.Value
	}
	return ""
}

// nodeToAny decodes an arbitrary YAML node (value/patch) into a Go value via goccy
// NodeToValue: a CEL string, a literal, or a nested object/list (CEL strings in
// cells are rendered recursively Keeper-side). Decode failure → nil (the validator
// raises the diagnostic by yaml_path).
func nodeToAny(node ast.Node) any {
	var v any
	if err := yaml.NodeToValue(node, &v); err != nil {
		return nil
	}
	return v
}

// setsFromNode decodes a mapping `<field>: <expression>` into map[string]string. A
// non-mapping node (old seq form, scalar) → nil (validateStateChanges raises
// type_mismatch). Non-string values are skipped — the validator raises the
// diagnostic by yaml_path.
func setsFromNode(node ast.Node) map[string]string {
	mm, ok := node.(*ast.MappingNode)
	if !ok {
		return nil
	}
	out := make(map[string]string, len(mm.Values))
	for _, kv := range mm.Values {
		tok := kv.Key.GetToken()
		if tok == nil {
			continue
		}
		if sn, ok := kv.Value.(*ast.StringNode); ok {
			out[tok.Value] = sn.Value
		}
	}
	return out
}

// stringSeqFromNode decodes a sequence of strings into []string. Non-sequence → nil.
func stringSeqFromNode(node ast.Node) []string {
	seq, ok := node.(*ast.SequenceNode)
	if !ok {
		return nil
	}
	vals := make([]string, 0, len(seq.Values))
	for _, item := range seq.Values {
		if sn, ok := item.(*ast.StringNode); ok {
			vals = append(vals, sn.Value)
		}
	}
	return vals
}

// reScenarioName — scenario name: snake_case or kebab-case (cluster operation names:
// `create`, `add_user`, `update_acl`, `add_replica`, `restart`). Unlike
// destiny/service names (strictly kebab), a scenario is a verb name for an operation;
// snake_case is canonical in the spec and examples ([scenario/concept.md],
// [architecture.md → service-repo layout]). A dash is also allowed (e.g. `add-user`).
var reScenarioName = regexp.MustCompile(`^[a-z][a-z0-9]*([_-][a-z0-9]+)*$`)

// reCovenName — kebab-case coven label in `on: [coven, ...]`. Same form as a
// service/scenario name (single-segment kebab). A CEL wrapper `${ ... }` (e.g.
// `${ incarnation.name }`) is also allowed — regex validation is skipped for it.
var reCovenName = regexp.MustCompile(`^[a-z][a-z0-9]*(-[a-z0-9]+)*$`)

// reSerialPercent — percent form `serial: "<N>%"` (§2.4). The integer part is a
// positive number without leading zeros (1..99 inclusive; 100% equals the default
// "full width" and is meaningless as an explicit form, but grammar-wise valid).
var reSerialPercent = regexp.MustCompile(`^[1-9][0-9]*%$`)

// ParseSerialPercent parses the percent form `serial: "<N>%"` (§2.4) — the single
// source of truth about the percent-string serial form for both sides: the config
// validator (validateSerialField, checks the form) and the runtime dispatcher
// (render: percent → wave width). Returns the integer part N and ok=true only for
// strings exactly matching reSerialPercent (1..99, no leading zeros); otherwise
// (0, false) for an invalid/non-percent form.
func ParseSerialPercent(s string) (pct int, ok bool) {
	if !reSerialPercent.MatchString(s) {
		return 0, false
	}
	// The regex guaranteed `[1-9][0-9]*` before `%` — Atoi cannot fail.
	n, _ := strconv.Atoi(s[:len(s)-1])
	return n, true
}

// deprecatedScenarioKeys — deprecated top-level keys of a scenario `main.yml`.
// `wait:` and `filter:` were explicitly removed by orchestration.md §2/§4 — we raise
// `unknown_key` with a replacement hint. Symmetric with `deprecatedDestinyKeys`.
var deprecatedScenarioKeys = map[string]string{
	"wait":   "wait: removed (orchestration.md §2); express the same with retry:+until: on a probe step",
	"filter": "filter: removed (orchestration.md §4); use where: with register.<probe>.* predicate or stable soulprint.self.* facts",
	// `version:` is a git ref, not a manifest field (ADR-007).
	"version": "version is a git ref under which the scenario is committed, not a manifest field; see ADR-007",
}

// deprecatedTaskKeys — deprecated task-level keys (inside a `tasks[]` element or
// inside `block:`). Symmetric with deprecatedScenarioKeys.
var deprecatedTaskKeys = map[string]string{
	"wait":   "wait: removed (orchestration.md §2); express with retry:+until: on a probe step",
	"filter": "filter: removed (orchestration.md §4); use where: predicate instead",
}

// stateChangesKnownKeys — the closed key set of the old map form of `state_changes:`.
var stateChangesKnownKeys = map[string]bool{
	"sets":     true,
	"appends":  true,
	"modifies": true,
}

// stateOpKnownKeys — the closed key set of one list-form operation (verb +
// parameters). Verbs come from stateOpVerbs; the rest are common op parameters. A
// key outside the set → unknown_key. `expect` is a parameter (modify/remove), not a
// verb; `as`/`do` are foreach parameters; `in` is not a key (foreach: carries the
// expression).
var stateOpKnownKeys = map[string]bool{
	"set": true, "add": true, "modify": true, "remove": true,
	"foreach": true,
	"value":   true, "key": true, "match": true, "on_conflict": true,
	"patch": true, "expect": true, "as": true, "do": true,
}

// stateOpConflictValues — the allowed `on_conflict` values.
var stateOpConflictValues = map[string]bool{
	"skip": true, "replace": true, "error": true,
}

// stateOpExpectValues — the allowed `expect` values (modify/remove).
var stateOpExpectValues = map[string]bool{
	"one": true, "at_most_one": true, "any": true,
}

// foreachReservedBindings — names `foreach.as:` must not shadow: the bare as-binding
// is declared in the merge-time CEL context (render.renderForeach) and would clobber
// the fixed scenario context OR the collection element's local bindings. Beyond
// loopReservedNames (input/register/incarnation/soulprint/essence/vars) it adds
// elem/key/value — the current element's local bindings in add-match/modify-patch
// (ADR-057 §b): `as: elem` would shadow the elem binding of a nested add operation
// (reserved_binding_name).
var foreachReservedBindings = map[string]bool{
	"input": true, "register": true, "incarnation": true,
	"soulprint": true, "essence": true, "vars": true,
	"elem": true, "key": true, "value": true,
}

// schemaValidateScenario runs post-decode checks on a ScenarioManifest.
func schemaValidateScenario(path string, root *ast.MappingNode, m *ScenarioManifest) []diag.Diagnostic {
	_ = path
	var out []diag.Diagnostic

	topKeys := topLevelKeys(root)

	// 1) Deprecated top-level keys → `unknown_key` with a meaningful hint.
	// Duplicate from the reflect-walker is suppressed via `scenarioManifestType` in walk.go.
	for _, kv := range root.Values {
		tok := kv.Key.GetToken()
		if tok == nil {
			continue
		}
		hint, dep := deprecatedScenarioKeys[tok.Value]
		if !dep {
			continue
		}
		out = append(out, diagAt(tok.Position.Line, tok.Position.Column, diag.Diagnostic{
			Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
			Code:     "unknown_key",
			Message:  `unknown field "` + tok.Value + `"`,
			Hint:     hint,
			YAMLPath: "$." + tok.Value,
		}))
	}

	// 2) `name:` — required + format.
	if !topKeys["name"] {
		out = append(out, diag.Diagnostic{
			Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
			Code:     "missing_required_field",
			Message:  "name is required at top-level",
			Hint:     "set name: <kebab-case>, matching scenario/<name>/ folder",
			YAMLPath: "$.name",
		})
	} else if !reScenarioName.MatchString(m.Name) {
		msg := fmt.Sprintf("name %q does not match %s", m.Name, reScenarioName)
		if m.Name == "" {
			msg = "name must be non-empty kebab-case string"
		}
		out = append(out, atPath(root, "$.name", diag.Diagnostic{
			Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
			Code:    "name_invalid_format",
			Message: msg,
			Hint:    "kebab-case: lowercase letters, digits, dashes; must start with letter",
		}))
	}

	// 3) `tasks:` — required (the key must be present). An empty list is valid (a
	// no-op scenario — e.g. `restart` could have no tasks, though in practice it has
	// at least one). A missing key is an error.
	if !topKeys["tasks"] {
		out = append(out, diag.Diagnostic{
			Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
			Code:     "missing_required_field",
			Message:  "tasks is required at top-level",
			Hint:     "declare tasks: [...] — list of scenario tasks; empty list is allowed for no-op scenarios",
			YAMLPath: "$.tasks",
		})
	}

	// 4) `state_changes:` — structural validation (only if the key is present).
	if topKeys["state_changes"] {
		out = append(out, validateStateChanges(root, "$.state_changes")...)
	}

	// 4a) `compute:` — structural validation (only if the key is present).
	if topKeys["compute"] {
		out = append(out, validateComputeBlock(root, "$.compute")...)
	}

	// 5) `input:` — shared schema validator.
	if topKeys["input"] {
		out = append(out, validateInputSchemaMap(m.Input, findInputMapping(root, "input"), "$.input")...)
	}

	// 5a) `validate:` — top-level list of input invariants (only if the key is present).
	if topKeys["validate"] {
		out = append(out, validateValidateBlock(root, "$.validate")...)
	}

	// 5b) `form:` — the form presentation layer + cross-invariants against input:
	// (form_field_unknown/duplicate/uncovered, section.key uniqueness). Active only
	// when the key is present.
	//
	// COVENANT GATE: the cross-field form check (`form` ⊆ effective `input`) is
	// correct only when `m.Input` already holds the effective field set. For a
	// non-extends scenario that is already so in the semantic phase — we validate here
	// as before (bit-for-bit). For a covenant scenario (extends != "") the effective
	// input exists ONLY AFTER the fragment merge (keeper-side, needs the snapshot FS):
	// here `m.Input` carries only the local delta, and a form field declared in the
	// covenant would yield a FALSE form_field_unknown. So under extends, form is
	// skipped here and checked post-merge by the same core on the merged input
	// (config.ResolveScenarioCovenant). The block structure (sections/key/show_when)
	// is checked post-merge by the same core — no separate structure-only branch needed.
	if topKeys["form"] && m.Extends == "" {
		out = append(out, validateFormLayout(root, m, "$.form")...)
	}

	// 6) `tasks[]` — polymorphic validation of each task.
	tasksNode := findSequenceValue(root, "tasks")
	if tasksNode != nil {
		for i, item := range tasksNode.Values {
			out = append(out, validateTaskNode(item, fmt.Sprintf("$.tasks[%d]", i))...)
		}
	}

	return out
}

// reComputeName — compute variable name: must be CEL-field-accessible
// (`compute.<name>`), i.e. a snake/camel identifier starting with a letter or `_`,
// with letters/digits/underscore inside. Dash/dot/space are forbidden (they would
// break `compute.<name>` access in CEL).
var reComputeName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// computeReservedNames — names a compute variable must not shadow: the root CEL
// context names (input/register/incarnation/soulprint/essence/vars) + the `compute`
// root itself. Compute variables live under `compute.<name>`, but the name `compute`
// as a variable would clobber the whole block — forbidden for self-documentation.
var computeReservedNames = map[string]bool{
	"input": true, "register": true, "incarnation": true,
	"soulprint": true, "essence": true, "vars": true, "compute": true,
}

// validateComputeBlock checks the structure of the `compute:` block (ADR-009
// amendment 2026-06-23): a mapping `<name>: <CEL-expression|literal>`. The name is a
// CEL-field-accessible identifier (reComputeName), not in computeReservedNames; a
// duplicate name is an error (would clobber the earlier compute). A string value must
// be non-empty; a non-string literal is valid (passes through like vars). A
// non-mapping block → type_mismatch.
func validateComputeBlock(root *ast.MappingNode, pathPrefix string) []diag.Diagnostic {
	node := findValueNode(root, "compute")
	mm, ok := node.(*ast.MappingNode)
	if !ok {
		line, col := 0, 0
		if vt := node.GetToken(); vt != nil {
			line, col = vt.Position.Line, vt.Position.Column
		}
		return []diag.Diagnostic{diagAt(line, col, diag.Diagnostic{
			Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
			Code:     "type_mismatch",
			Message:  "compute must be a mapping of <name> → CEL-expression",
			Hint:     "compute: { <name>: \"${ ... }\" } — scenario-level computed vars (ADR-009)",
			YAMLPath: pathPrefix,
		})}
	}

	var out []diag.Diagnostic
	seen := make(map[string]bool, len(mm.Values))
	for _, kv := range mm.Values {
		tok := kv.Key.GetToken()
		if tok == nil {
			continue
		}
		name := tok.Value
		switch {
		case computeReservedNames[name]:
			out = append(out, diagAt(tok.Position.Line, tok.Position.Column, diag.Diagnostic{
				Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
				Code:     "reserved_binding_name",
				Message:  fmt.Sprintf("compute.%s shadows a reserved CEL context name", name),
				Hint:     "reserved: input, register, incarnation, soulprint, essence, vars, compute",
				YAMLPath: pathPrefix + "." + name,
			}))
		case !reComputeName.MatchString(name):
			out = append(out, diagAt(tok.Position.Line, tok.Position.Column, diag.Diagnostic{
				Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
				Code:     "name_invalid_format",
				Message:  fmt.Sprintf("compute name %q is not a valid CEL identifier", name),
				Hint:     "use letters/digits/underscore, start with a letter or _ (accessed as compute.<name>)",
				YAMLPath: pathPrefix + "." + name,
			}))
		case seen[name]:
			out = append(out, diagAt(tok.Position.Line, tok.Position.Column, diag.Diagnostic{
				Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
				Code:     "duplicate_key",
				Message:  fmt.Sprintf("compute.%s is declared more than once", name),
				YAMLPath: pathPrefix + "." + name,
			}))
		}
		seen[name] = true

		// Value: a CEL string must be non-empty; a non-string literal is valid.
		if sn, isStr := kv.Value.(*ast.StringNode); isStr && sn.Value == "" {
			out = append(out, diagAt(tok.Position.Line, tok.Position.Column, diag.Diagnostic{
				Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
				Code:     "empty_value",
				Message:  fmt.Sprintf("compute.%s must be a non-empty expression", name),
				YAMLPath: pathPrefix + "." + name,
			}))
		}
	}
	return out
}

// validateStateChanges checks the structure of the `state_changes:` block. DUAL-FORM:
//
//   - a sequence in place of the block → new list form: each element is a verb
//     operation (validateStateOp); all verbs (set/add/modify/remove/foreach) are
//     validated against the full ADR-057 grammar.
//   - a mapping → old map form (DEPRECATED): the former path (sets/appends/modifies).
//     An empty `state_changes: {}` is valid.
//   - other (scalar/null) — decode already raised type_mismatch; silent here.
func validateStateChanges(root *ast.MappingNode, pathPrefix string) []diag.Diagnostic {
	node := findValueNode(root, "state_changes")
	switch n := node.(type) {
	case *ast.SequenceNode:
		var out []diag.Diagnostic
		for i, item := range n.Values {
			out = append(out, validateStateOp(item, fmt.Sprintf("%s[%d]", pathPrefix, i))...)
		}
		return out
	case *ast.MappingNode:
		return validateStateChangesMap(n, pathPrefix)
	default:
		return nil
	}
}

// validateStateChangesMap — old map form (sets/appends/modifies, DEPRECATED).
//
// ADR-057 transit safeguard (b): a valid map form is NOT an error (dual-parse for one
// release) but must emit a DEPRECATION WARN — otherwise a scenario silently rides a
// form the next release will remove. For appends/modifies a separate, stricter warn:
// they were no-op placeholders (state does NOT grow) and must be rewritten as
// add/modify, else a latent bug (ADR-057 §context).
func validateStateChangesMap(node *ast.MappingNode, pathPrefix string) []diag.Diagnostic {
	var out []diag.Diagnostic

	// One deprecation warn for the whole block (anchored on the state_changes key —
	// the position of the first known key), to avoid duplicating per sets/appends/modifies.
	if pos := firstKnownStateChangeKeyPos(node); pos != nil {
		out = append(out, diagAt(pos.Line, pos.Column, diag.Diagnostic{
			Level: diag.LevelWarning, Phase: diag.PhaseSchemaValidate,
			Code:     "deprecated_form",
			Message:  "state_changes map-form (sets/appends/modifies) is deprecated and will be removed next release",
			Hint:     "rewrite as the ordered list-of-verbs form (- set: / - add: / - modify: / - remove:) — ADR-057",
			YAMLPath: pathPrefix,
		}))
	}

	for _, kv := range node.Values {
		tok := kv.Key.GetToken()
		if tok == nil {
			continue
		}
		keyName := tok.Value
		if !stateChangesKnownKeys[keyName] {
			out = append(out, diagAt(tok.Position.Line, tok.Position.Column, diag.Diagnostic{
				Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
				Code:     "unknown_key",
				Message:  `unknown field "` + keyName + `"`,
				Hint:     "state_changes (map-form, deprecated) allows only sets / appends / modifies; prefer the ordered list-form (- set: / - add:)",
				YAMLPath: pathPrefix + "." + keyName,
			}))
			continue
		}
		if keyName == "sets" {
			out = append(out, validateSetsMap(tok.Position.Line, tok.Position.Column, kv.Value, pathPrefix)...)
			continue
		}
		// appends/modifies are no-op placeholders: state does NOT grow. A separate warn
		// so the author does not think the declaration works (ADR-057 transit).
		out = append(out, diagAt(tok.Position.Line, tok.Position.Column, diag.Diagnostic{
			Level: diag.LevelWarning, Phase: diag.PhaseSchemaValidate,
			Code:     "noop_placeholder",
			Message:  fmt.Sprintf("state_changes.%s is a no-op placeholder — it never applied, incarnation.state does not grow", keyName),
			Hint:     "rewrite on the list-form: appends → - add: / modifies → - modify: (otherwise state will not change) — ADR-057",
			YAMLPath: pathPrefix + "." + keyName,
		}))
		out = append(out, validateStringSeq(tok.Position.Line, tok.Position.Column, kv.Value, keyName, pathPrefix)...)
	}
	return out
}

// firstKnownStateChangeKeyPos returns the position of the first known map-form key
// (sets/appends/modifies) to anchor the block's deprecation warn. nil for an empty
// `state_changes: {}` (no deprecation warn needed: an empty block misleads no one and
// is valid in both forms).
func firstKnownStateChangeKeyPos(node *ast.MappingNode) *struct{ Line, Column int } {
	for _, kv := range node.Values {
		tok := kv.Key.GetToken()
		if tok == nil {
			continue
		}
		if stateChangesKnownKeys[tok.Value] {
			return &struct{ Line, Column int }{tok.Position.Line, tok.Position.Column}
		}
	}
	return nil
}

// validateStateOp validates one list-form operation (element `state_changes[i]`).
//
// The element must be a mapping with EXACTLY one verb key (`set`/`add`/…) whose value
// is a non-empty target field name. Parameters (`value`/`key`/`match`/`on_conflict`/
// `patch`/`expect`/`as`/`do`) are checked for applicability to the verb:
//
//   - set:    needs value; match/key/on_conflict/patch/expect inapplicable;
//   - add:    needs value; on_conflict ∈ {skip,replace,error}; key (map) / match
//     (list dedup) optional; patch/expect inapplicable;
//   - modify: needs match + patch; optional expect; value/key/on_conflict/as/do n/a;
//   - remove: needs match; optional expect; value/key/on_conflict/patch/as/do n/a;
//   - foreach: needs as + do (non-empty); a nested foreach in do is rejected.
func validateStateOp(node ast.Node, path string) []diag.Diagnostic {
	mm, ok := node.(*ast.MappingNode)
	if !ok {
		vt := node.GetToken()
		line, col := 0, 0
		if vt != nil {
			line, col = vt.Position.Line, vt.Position.Column
		}
		return []diag.Diagnostic{diagAt(line, col, diag.Diagnostic{
			Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
			Code:     "type_mismatch",
			Message:  "state_changes operation must be a mapping with a verb key (- set: / - add: / …)",
			YAMLPath: path,
		})}
	}

	var out []diag.Diagnostic
	var verbTok = struct {
		name string
		line int
		col  int
		set  bool
	}{}
	seen := make(map[string]*ast.MappingValueNode, len(mm.Values))

	for _, kv := range mm.Values {
		tok := kv.Key.GetToken()
		if tok == nil {
			continue
		}
		key := tok.Value
		seen[key] = kv
		if !stateOpKnownKeys[key] {
			out = append(out, diagAt(tok.Position.Line, tok.Position.Column, diag.Diagnostic{
				Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
				Code:     "unknown_key",
				Message:  `unknown field "` + key + `"`,
				Hint:     "operation keys: <verb> (set/add/modify/remove/foreach) + value/key/match/on_conflict/patch/expect/as/do",
				YAMLPath: path + "." + key,
			}))
			continue
		}
		if _, isVerb := stateOpVerbs[key]; isVerb {
			if verbTok.set {
				out = append(out, diagAt(tok.Position.Line, tok.Position.Column, diag.Diagnostic{
					Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
					Code:     "invalid_value",
					Message:  fmt.Sprintf("state_changes operation has multiple verbs (%q and %q) — exactly one expected", verbTok.name, key),
					YAMLPath: path,
				}))
				continue
			}
			verbTok.name, verbTok.line, verbTok.col, verbTok.set = key, tok.Position.Line, tok.Position.Column, true
			// `foreach:` carries the CEL collection expression; the other verbs carry
			// the target field name. In both cases the value must be a non-empty string
			// (foreach without an expression / a verb without a field is an error).
			if stringFromNode(kv.Value) == "" {
				msg := fmt.Sprintf("%s: target field must be a non-empty string", key)
				if key == "foreach" {
					msg = "foreach: requires a non-empty CEL collection expression (foreach: \"${ ... }\")"
				}
				out = append(out, diagAt(tok.Position.Line, tok.Position.Column, diag.Diagnostic{
					Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
					Code:     "empty_value",
					Message:  msg,
					YAMLPath: path + "." + key,
				}))
			}
		}
	}

	if !verbTok.set {
		return append(out, diagAt(lineOf(mm), colOf(mm), diag.Diagnostic{
			Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
			Code:     "missing_required_field",
			Message:  "state_changes operation has no verb (expected one of set/add/modify/remove/foreach)",
			YAMLPath: path,
		}))
	}

	switch verbTok.name {
	case "set":
		out = append(out, validateSetOp(seen, path, verbTok.line, verbTok.col)...)
	case "add":
		out = append(out, validateAddOp(seen, path, verbTok.line, verbTok.col)...)
	case "modify":
		out = append(out, validateModifyOp(seen, path, verbTok.line, verbTok.col)...)
	case "remove":
		out = append(out, validateRemoveOp(seen, path, verbTok.line, verbTok.col)...)
	case "foreach":
		out = append(out, validateForeachOp(seen, path, verbTok.line, verbTok.col)...)
	}
	return out
}

// validateModifyOp — `modify` needs match + patch (map path→CEL); optional expect;
// value/key/on_conflict/in/as/do inapplicable. patch must be a mapping. Wide-match
// safeguard: a constant-true (`match: true`) or missing match → WARN "a wide
// predicate patches the whole collection" (§7.1 (d)).
func validateModifyOp(seen map[string]*ast.MappingValueNode, path string, vline, vcol int) []diag.Diagnostic {
	var out []diag.Diagnostic
	out = append(out, warnWideMatch(seen, path, vline, vcol, "modify")...)
	if seen["patch"] == nil {
		out = append(out, diagAt(vline, vcol, diag.Diagnostic{
			Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
			Code:     "missing_required_field",
			Message:  "modify: requires patch: { <path-in-element>: \"${ ... }\" } (orchestration.md §7.1)",
			YAMLPath: path + ".patch",
		}))
	} else {
		out = append(out, validatePatchMap(seen["patch"], path)...)
	}
	out = append(out, validateExpectValue(seen, path)...)
	out = append(out, rejectKeys(seen, path, []string{"value", "key", "on_conflict", "as", "do"}, "modify:")...)
	return out
}

// validateRemoveOp — `remove` needs match; optional expect; value/key/on_conflict/
// patch/in/as/do inapplicable. Same wide-match safeguard.
func validateRemoveOp(seen map[string]*ast.MappingValueNode, path string, vline, vcol int) []diag.Diagnostic {
	var out []diag.Diagnostic
	out = append(out, warnWideMatch(seen, path, vline, vcol, "remove")...)
	out = append(out, validateExpectValue(seen, path)...)
	out = append(out, rejectKeys(seen, path, []string{"value", "key", "on_conflict", "patch", "as", "do"}, "remove:")...)
	return out
}

// validateForeachOp — `foreach` needs as: (binding name) + do: (non-empty list of
// nested operations); value/key/match/on_conflict/patch/expect inapplicable. Each
// nested do operation is validated recursively (validateStateOp).
//
// `as:` must not shadow a reserved CEL-context name or an element-local binding
// (foreachReservedBindings) → reserved_binding_name.
//
// A nested foreach in do is out of grammar (ADR-057: do carries CRUD verbs, not a
// re-loop). validateStateOp does NOT reject it (foreach is a valid top-level verb),
// so each do element is checked explicitly here: a do-foreach would pass lint, and
// render.renderForeach would expand it via renderOneStateOp with Verb=foreach → merge
// would fail at runtime (`verb foreach not supported` → state_changes_apply_failed →
// error_locked AFTER apply on the hosts). Caught at validation time (BUG-2).
func validateForeachOp(seen map[string]*ast.MappingValueNode, path string, vline, vcol int) []diag.Diagnostic {
	var out []diag.Diagnostic
	if as := seen["as"]; as == nil || stringFromNode(as.Value) == "" {
		out = append(out, diagAt(vline, vcol, diag.Diagnostic{
			Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
			Code:     "missing_required_field",
			Message:  "foreach: requires as: <name> (binding for the current iteration element)",
			YAMLPath: path + ".as",
		}))
	} else if name := stringFromNode(as.Value); foreachReservedBindings[name] {
		tok := as.Key.GetToken()
		line, col := vline, vcol
		if tok != nil {
			line, col = tok.Position.Line, tok.Position.Column
		}
		out = append(out, diagAt(line, col, diag.Diagnostic{
			Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
			Code:     "reserved_binding_name",
			Message:  fmt.Sprintf("foreach.as %q shadows a reserved name (CEL context or per-element binding)", name),
			Hint:     "reserved: input, register, incarnation, soulprint, essence, vars, elem, key, value",
			YAMLPath: path + ".as",
		}))
	}
	doKV := seen["do"]
	if doKV == nil {
		out = append(out, diagAt(vline, vcol, diag.Diagnostic{
			Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
			Code:     "missing_required_field",
			Message:  "foreach: requires do: [<verb...>] (operations applied per iteration)",
			YAMLPath: path + ".do",
		}))
	} else if seq, ok := doKV.Value.(*ast.SequenceNode); ok {
		if len(seq.Values) == 0 {
			out = append(out, diagAt(vline, vcol, diag.Diagnostic{
				Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
				Code:     "empty_value",
				Message:  "foreach.do must contain at least one operation",
				YAMLPath: path + ".do",
			}))
		}
		for i, item := range seq.Values {
			doPath := fmt.Sprintf("%s.do[%d]", path, i)
			out = append(out, validateStateOp(item, doPath)...)
			out = append(out, rejectNestedForeach(item, doPath)...)
		}
	} else {
		out = append(out, diagAt(vline, vcol, diag.Diagnostic{
			Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
			Code:     "type_mismatch",
			Message:  "foreach.do must be a sequence of operations",
			YAMLPath: path + ".do",
		}))
	}
	out = append(out, rejectKeys(seen, path, []string{"value", "key", "match", "on_conflict", "patch", "expect"}, "foreach:")...)
	return out
}

// rejectNestedForeach rejects a foreach verb inside do: a nested loop is out of the
// ADR-057 grammar (do carries CRUD verbs only). Checked over the AST — it looks for a
// `foreach` key among the do element's keys.
func rejectNestedForeach(node ast.Node, path string) []diag.Diagnostic {
	mm, ok := node.(*ast.MappingNode)
	if !ok {
		return nil
	}
	for _, kv := range mm.Values {
		tok := kv.Key.GetToken()
		if tok == nil || tok.Value != "foreach" {
			continue
		}
		return []diag.Diagnostic{diagAt(tok.Position.Line, tok.Position.Column, diag.Diagnostic{
			Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
			Code:     "nested_foreach_unsupported",
			Message:  "nested foreach in do: is not supported (do: carries CRUD verbs set/add/modify/remove only) — ADR-057",
			Hint:     "flatten the iteration: a single foreach with the combined collection, or precompute the list in vars:",
			YAMLPath: path + ".foreach",
		})}
	}
	return nil
}

// validatePatchMap checks that `patch:` is a mapping (path-in-element → value). An
// empty patch is grammatically valid (no-op merge) but meaningless — allowed without
// an error (symmetric with an empty state_changes).
func validatePatchMap(patchKV *ast.MappingValueNode, path string) []diag.Diagnostic {
	if _, ok := patchKV.Value.(*ast.MappingNode); ok {
		return nil
	}
	tok := patchKV.Key.GetToken()
	line, col := 0, 0
	if tok != nil {
		line, col = tok.Position.Line, tok.Position.Column
	}
	return []diag.Diagnostic{diagAt(line, col, diag.Diagnostic{
		Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
		Code:     "type_mismatch",
		Message:  "modify.patch must be a mapping of <path-in-element> → CEL/literal",
		YAMLPath: path + ".patch",
	})}
}

// validateExpectValue checks that `expect` ∈ {one, at_most_one, any}.
func validateExpectValue(seen map[string]*ast.MappingValueNode, path string) []diag.Diagnostic {
	exp := seen["expect"]
	if exp == nil {
		return nil
	}
	val := stringFromNode(exp.Value)
	if stateOpExpectValues[val] {
		return nil
	}
	tok := exp.Key.GetToken()
	line, col := 0, 0
	if tok != nil {
		line, col = tok.Position.Line, tok.Position.Column
	}
	return []diag.Diagnostic{diagAt(line, col, diag.Diagnostic{
		Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
		Code:     "invalid_value",
		Message:  fmt.Sprintf("expect %q is invalid (expected one / at_most_one / any)", val),
		YAMLPath: path + ".expect",
	})}
}

// warnWideMatch — safeguard (a) ADR-057 §d: modify/remove without match: OR with a
// constant-true predicate (`match: true`) will re-patch/remove the WHOLE collection.
// Not an error (the author may have meant "all"), but WARN — intent must be explicit.
// soul-lint prints the warn, exit code stays 0.
func warnWideMatch(seen map[string]*ast.MappingValueNode, path string, vline, vcol int, verb string) []diag.Diagnostic {
	m := seen["match"]
	if m == nil {
		return []diag.Diagnostic{diagAt(vline, vcol, diag.Diagnostic{
			Level: diag.LevelWarning, Phase: diag.PhaseSchemaValidate,
			Code:     "wide_match",
			Message:  fmt.Sprintf("%s without match: affects the WHOLE collection (all elements)", verb),
			Hint:     "add match: \"<CEL-predicate>\" to scope the operation, or confirm bulk intent is desired",
			YAMLPath: path,
		})}
	}
	if isConstTrueMatch(stringFromNode(m.Value)) {
		tok := m.Key.GetToken()
		line, col := vline, vcol
		if tok != nil {
			line, col = tok.Position.Line, tok.Position.Column
		}
		return []diag.Diagnostic{diagAt(line, col, diag.Diagnostic{
			Level: diag.LevelWarning, Phase: diag.PhaseSchemaValidate,
			Code:     "wide_match",
			Message:  fmt.Sprintf("%s with constant-true match affects the WHOLE collection (all elements)", verb),
			Hint:     "narrow the predicate (key == X / elem.id == Y), or confirm bulk intent is desired",
			YAMLPath: path + ".match",
		})}
	}
	return nil
}

// isConstTrueMatch recognizes a constant-true predicate (`true`, `1 == 1`) that
// removes/patches the whole collection. A full CEL analysis is unnecessary — we catch
// the obvious literal form `true` (allowing surrounding spaces / `${ }` wrapper).
//
// TODO(wide-match): extend to "the predicate does not reference elem/key/value" (any
// match ignoring the element hits the whole collection — suspicious). Doing it
// correctly needs a CEL-AST walk (shared/cel): a regex over identifiers false-positives
// on `register.value`/`input.key`/a field `x.elem`. For now we catch only the literal
// `true`; full coverage is a separate slice with AST parsing.
func isConstTrueMatch(expr string) bool {
	s := strings.TrimSpace(expr)
	s = strings.TrimPrefix(s, "${")
	s = strings.TrimSuffix(s, "}")
	return strings.TrimSpace(s) == "true"
}

// validateSetOp — `set` needs value; match/key/on_conflict/patch/expect inapplicable.
// `expect` is a cardinality assert ONLY for modify/remove (ADR-057 §c); on set the
// engine would silently ignore it (an operator trap, BUG-1).
func validateSetOp(seen map[string]*ast.MappingValueNode, path string, vline, vcol int) []diag.Diagnostic {
	var out []diag.Diagnostic
	if seen["value"] == nil {
		out = append(out, diagAt(vline, vcol, diag.Diagnostic{
			Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
			Code:     "missing_required_field",
			Message:  "set: requires value: (CEL-expression or literal to overwrite the field)",
			YAMLPath: path + ".value",
		}))
	}
	out = append(out, rejectKeys(seen, path, []string{"match", "key", "on_conflict", "patch", "expect", "in", "as", "do"}, "set:")...)
	return out
}

// validateAddOp — `add` needs value; on_conflict ∈ {skip,replace,error}; key (map) /
// match (list dedup) optional; patch/expect/in/as/do inapplicable. `expect` is a
// cardinality assert ONLY for modify/remove (ADR-057 §c); on add the engine would
// silently ignore it (a trap: the operator expects dup protection on add but there is
// none — dedup is done by on_conflict, BUG-1).
func validateAddOp(seen map[string]*ast.MappingValueNode, path string, vline, vcol int) []diag.Diagnostic {
	var out []diag.Diagnostic
	if seen["value"] == nil {
		out = append(out, diagAt(vline, vcol, diag.Diagnostic{
			Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
			Code:     "missing_required_field",
			Message:  "add: requires value: (element to add — object or scalar)",
			YAMLPath: path + ".value",
		}))
	}
	if oc := seen["on_conflict"]; oc != nil {
		val := stringFromNode(oc.Value)
		if !stateOpConflictValues[val] {
			tok := oc.Key.GetToken()
			line, col := vline, vcol
			if tok != nil {
				line, col = tok.Position.Line, tok.Position.Column
			}
			out = append(out, diagAt(line, col, diag.Diagnostic{
				Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
				Code:     "invalid_value",
				Message:  fmt.Sprintf("on_conflict %q is invalid (expected skip / replace / error)", val),
				YAMLPath: path + ".on_conflict",
			}))
		}
	}
	out = append(out, rejectKeys(seen, path, []string{"patch", "expect", "in", "as", "do"}, "add:")...)
	return out
}

// rejectKeys raises unknown_key for each present but verb-inapplicable key (e.g.
// patch: on add, match: on set).
func rejectKeys(seen map[string]*ast.MappingValueNode, path string, keys []string, verb string) []diag.Diagnostic {
	var out []diag.Diagnostic
	for _, k := range keys {
		kv := seen[k]
		if kv == nil {
			continue
		}
		tok := kv.Key.GetToken()
		line, col := 0, 0
		if tok != nil {
			line, col = tok.Position.Line, tok.Position.Column
		}
		out = append(out, diagAt(line, col, diag.Diagnostic{
			Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
			Code:     "unknown_key",
			Message:  fmt.Sprintf("%s does not accept %q", verb, k),
			YAMLPath: path + "." + k,
		}))
	}
	return out
}

// lineOf/colOf — node position (fallback 0 when the token is absent).
func lineOf(node ast.Node) int {
	if tok := node.GetToken(); tok != nil {
		return tok.Position.Line
	}
	return 0
}

func colOf(node ast.Node) int {
	if tok := node.GetToken(); tok != nil {
		return tok.Position.Column
	}
	return 0
}

// findValueNode — the raw value node under the top-level key name (any form:
// mapping/sequence/scalar). Parallel to findInputMapping/findSequenceValue but with
// no kind filter — needed for dual-form dispatch (validateStateChanges).
func findValueNode(root *ast.MappingNode, name string) ast.Node {
	if root == nil {
		return nil
	}
	for _, kv := range root.Values {
		tok := kv.Key.GetToken()
		if tok == nil || tok.Value != name {
			continue
		}
		return kv.Value
	}
	return nil
}

// validateSetsMap checks `sets` as a mapping `<field>: <expression>`: the block value
// is a mapping and each value is a non-empty string expression (CEL/literal).
// keyLine/keyCol — the `sets` key position (fallback when the value has no token).
func validateSetsMap(keyLine, keyCol int, value ast.Node, pathPrefix string) []diag.Diagnostic {
	mm, ok := value.(*ast.MappingNode)
	if !ok {
		vt := value.GetToken()
		line, col := keyLine, keyCol
		if vt != nil {
			line, col = vt.Position.Line, vt.Position.Column
		}
		return []diag.Diagnostic{diagAt(line, col, diag.Diagnostic{
			Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
			Code:     "type_mismatch",
			Message:  "state_changes.sets must be a mapping of field → CEL-expression",
			Hint:     "sets: { <field>: \"${ ... }\" } — orchestration.md §7.1",
			YAMLPath: pathPrefix + ".sets",
		})}
	}
	var out []diag.Diagnostic
	for _, kv := range mm.Values {
		ftok := kv.Key.GetToken()
		if ftok == nil {
			continue
		}
		sn, isStr := kv.Value.(*ast.StringNode)
		if !isStr {
			vt := kv.Value.GetToken()
			line, col := ftok.Position.Line, ftok.Position.Column
			if vt != nil {
				line, col = vt.Position.Line, vt.Position.Column
			}
			out = append(out, diagAt(line, col, diag.Diagnostic{
				Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
				Code:     "type_mismatch",
				Message:  fmt.Sprintf("state_changes.sets.%s must be a string expression", ftok.Value),
				YAMLPath: fmt.Sprintf("%s.sets.%s", pathPrefix, ftok.Value),
			}))
			continue
		}
		if sn.Value == "" {
			out = append(out, diagAt(ftok.Position.Line, ftok.Position.Column, diag.Diagnostic{
				Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
				Code:     "empty_value",
				Message:  fmt.Sprintf("state_changes.sets.%s must be a non-empty expression", ftok.Value),
				YAMLPath: fmt.Sprintf("%s.sets.%s", pathPrefix, ftok.Value),
			}))
		}
	}
	return out
}

// validateStringSeq checks `appends`/`modifies` as a sequence of strings (future).
// keyLine/keyCol — the key position (fallback when an element has no token).
func validateStringSeq(keyLine, keyCol int, value ast.Node, keyName, pathPrefix string) []diag.Diagnostic {
	seq, ok := value.(*ast.SequenceNode)
	if !ok {
		vt := value.GetToken()
		line, col := keyLine, keyCol
		if vt != nil {
			line, col = vt.Position.Line, vt.Position.Column
		}
		return []diag.Diagnostic{diagAt(line, col, diag.Diagnostic{
			Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
			Code:     "type_mismatch",
			Message:  fmt.Sprintf("state_changes.%s must be a sequence of strings", keyName),
			YAMLPath: pathPrefix + "." + keyName,
		})}
	}
	var out []diag.Diagnostic
	for i, item := range seq.Values {
		if _, isStr := item.(*ast.StringNode); !isStr {
			vt := item.GetToken()
			line, col := keyLine, keyCol
			if vt != nil {
				line, col = vt.Position.Line, vt.Position.Column
			}
			out = append(out, diagAt(line, col, diag.Diagnostic{
				Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
				Code:     "type_mismatch",
				Message:  fmt.Sprintf("state_changes.%s[%d] must be a string", keyName, i),
				YAMLPath: fmt.Sprintf("%s.%s[%d]", pathPrefix, keyName, i),
			}))
		}
	}
	return out
}

// findSequenceValue — the value node under key `name` if the value is a SequenceNode.
// Symmetric with findInputMapping for the sequence case.
func findSequenceValue(m *ast.MappingNode, name string) *ast.SequenceNode {
	if m == nil {
		return nil
	}
	for _, kv := range m.Values {
		tok := kv.Key.GetToken()
		if tok == nil || tok.Value != name {
			continue
		}
		if s, ok := kv.Value.(*ast.SequenceNode); ok {
			return s
		}
		return nil
	}
	return nil
}

// semanticValidateScenario — cross-field/cross-task invariants of a ScenarioManifest.
//
// Covered: duplicate_task_address (register ∪ id) + unknown_register_reference over
// the `tasks[]` list (including nested block:), see validateTaskRefs. CEL syntax and
// cross-ref inside CEL predicates (`when:`/`changed_when:`/`until:`) are deferred
// (M1.3/M1.5).
func semanticValidateScenario(m *ScenarioManifest, root *ast.MappingNode) []diag.Diagnostic {
	out := validateTaskRefs(findSequenceValue(root, "tasks"), "$.tasks")
	out = append(out, validateExtendsField(m, root)...)
	return out
}

// validateExtendsField — semantic check of the `extends:` form (covenant.go). Empty/
// absent extends = no inheritance (valid, nothing checked — forward-compat). A
// non-empty name must be a valid covenant reference (ValidExtendsName: single-segment
// kebab, traversal-clamped by the name grammar): else covenant_extends_invalid.
// Resolving the fragment against the FS is S2 (keeper-side); only the name form here.
func validateExtendsField(m *ScenarioManifest, root *ast.MappingNode) []diag.Diagnostic {
	if m.Extends == "" {
		return nil
	}
	if ValidExtendsName(m.Extends) {
		return nil
	}
	return []diag.Diagnostic{atPath(root, "$.extends", diag.Diagnostic{
		Level: diag.LevelError, Phase: diag.PhaseSemanticValidate,
		Code:    "covenant_extends_invalid",
		Message: fmt.Sprintf("extends %q is not a valid covenant name", m.Extends),
		Hint:    "single-segment kebab-case (^[a-z][a-z0-9-]*$); names the root covenant.yml-family fragment, must not contain path separators",
	})}
}
