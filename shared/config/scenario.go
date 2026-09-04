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
// It holds the scenario name/description, the `input:` contract (docs/input.md)
// and the `tasks[]` list. What the scenario writes to `incarnation.state` is a
// `core.state.<verb>` task among the others ([ADR-0084]), not a section of its
// own.
//
// The task DSL core is inherited from destiny (ADR-009): scenario supports all 22
// keys from docs/destiny/tasks.md plus the scenario delta (on/where/serial/
// run_once). Polymorphic task decode (module / apply / include / block) lives in
// scenario_task.go.
type ScenarioManifest struct {
	Name        string         `yaml:"name"`
	Description string         `yaml:"description,omitempty"`
	Input       InputSchemaMap `yaml:"input,omitempty"`
	Validate    []ValidateRule `yaml:"validate,omitempty"`
	Compute     ComputeBlock   `yaml:"compute,omitempty"`
	Vars        map[string]any `yaml:"vars,omitempty"`
	Tasks       []Task         `yaml:"tasks"`

	// Extends names a covenant fragment at the service-repo root (`covenant.yml`
	// without extension, `<dir>/<name>` at most one directory deep, each segment
	// matching refPathSegment — see reCovenantName in covenant.go, which is the
	// authority) whose input/compute/validate sections the scenario inherits
	// (covenant.go). Empty /
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

	// IDTemplate composes the incarnation id from `input:` components at create
	// time instead of taking it as free text (`id_template`, ADR-0079 as amended by
	// [ADR-0085]). A `${ … }` template over input only (id_template.go);
	// empty/absent = the operator names the incarnation (unchanged behavior). Read
	// ONLY on the create path — the keeper renders it over the resolved input BEFORE
	// inserting the row, so the components that feed an id are write-once identity:
	// a later run with different values does NOT rename anything.
	//
	// Filled from [ScenarioManifest.LegacyNameTemplate] when only the old spelling
	// is present ([ScenarioManifest.normalizeIDTemplate]) — every reader past the
	// load sees one field and never has to know which spelling the file used.
	IDTemplate string `yaml:"id_template,omitempty"`

	// LegacyNameTemplate is the pre-[ADR-0085] spelling of [ScenarioManifest.IDTemplate],
	// kept for the compatibility window that ticket opens: a service repository
	// still on `name_template:` loads and runs, and soul-lint warns with the line
	// and the replacement. Both spellings in one file is an error, not a merge
	// (`id_template_conflict`) — the two would silently disagree. Removing this
	// field closes the window and is its own ticket.
	//
	// Never read as a TEMPLATE past [ScenarioManifest.normalizeIDTemplate]: it
	// exists so the deprecation can be reported, not so a second template can be
	// composed. It is still read as EVIDENCE of which spelling the file used —
	// [KeeperFeaturesOfScenario] cites the key that is actually in the file.
	//
	// [ADR-0085]: ../../docs/adr/0085-entity-id-and-label.md
	LegacyNameTemplate string `yaml:"name_template,omitempty"`

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
// vars/soulprint/register/vault in `that` → compile-time undeclared-reference
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
// it ONCE per run in the RUN-LEVEL scenario context (input/vars/incarnation/
// register), then the result is available as `compute.<name>` wherever a task
// interpolates (cel_render.resolveCompute).
//
// Purpose: remove duplication of a shared expression otherwise written twice
// (apply.input does not see task-level `vars:`) — declare a big merge() once and
// reference `${ compute.<name> }`.
//
// Isolation barrier (architect aebb2d39 §5):
//   - compute does NOT leak into the isolated destiny pass (destiny sees only the
//     result via apply.input — RenderInput.Compute is not forwarded there, ADR-009 V2);
//   - compute's resolve context is RUN-LEVEL (WITHOUT soulprint.self/soulprint.hosts):
//     compute is host-invariant by construction, so the same value correctly flows
//     to apply.input (resolved on targeted[0]) and to every task's params
//     (per-run, not
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

// StateVerb is the operation a `core.state.<verb>` capture step performs on one
// field of `incarnation.state` ([ADR-0084]). The state suffix of the module
// address IS the verb — see [keeper/internal/stateop] for the author form and the
// engine that applies it.
type StateVerb string

const (
	// VerbSet overwrites a field wholesale.
	VerbSet StateVerb = "set"
	// VerbAdd adds an element to a collection (map/list) idempotently.
	VerbAdd StateVerb = "add"
	// VerbModify patches ALL collection elements matching Match (all-by-default;
	// orchestration.md §7.1).
	VerbModify StateVerb = "modify"
	// VerbRemove removes ALL collection elements matching Match.
	VerbRemove StateVerb = "remove"
	// VerbPresent writes a field ONLY if it has no value yet; an existing one
	// wins. Field-level, unlike the secret-property rule of the same name in
	// [ADR-0083] §4 — see the amendment there.
	VerbPresent StateVerb = "present"
	// VerbAppend appends an element to a list field unconditionally. `add` is
	// idempotent by identity and is the right verb for a set; this one is for a
	// sequence where the same element may legitimately occur twice.
	VerbAppend StateVerb = "append"
	// VerbUnset removes the field itself, as opposed to `remove`, which removes
	// matching elements from inside a collection.
	VerbUnset StateVerb = "unset"
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

// stringFromNode extracts a node's string value. Non-string → "" (the validator
// raises type_mismatch).
func stringFromNode(node ast.Node) string {
	if sn, ok := node.(*ast.StringNode); ok {
		return sn.Value
	}
	return ""
}

// nodeToAny decodes an arbitrary YAML node into a Go value via goccy NodeToValue:
// a CEL string, a literal, or a nested object/list (CEL strings in cells are
// rendered recursively Keeper-side). Decode failure → nil (the validator raises
// the diagnostic by yaml_path).
func nodeToAny(node ast.Node) any {
	var v any
	if err := yaml.NodeToValue(node, &v); err != nil {
		return nil
	}
	return v
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
	// Removed by [ADR-0084]: a state field is written by an explicit
	// `core.state.<verb>` step where its value becomes known, not by an
	// end-of-run block that could only ever run after every host was already
	// configured.
	"state_changes": "state_changes: removed ([ADR-0084]); write each field with a `core.state.<verb>` task (module: core.state.set / present / add / append / modify / remove / unset) placed where the value becomes known -- the address routes it keeper-side, so it carries no on: key",
}

// deprecatedTaskKeys — deprecated task-level keys (inside a `tasks[]` element or
// inside `block:`). Symmetric with deprecatedScenarioKeys.
var deprecatedTaskKeys = map[string]string{
	"wait":   "wait: removed (orchestration.md §2); express with retry:+until: on a probe step",
	"filter": "filter: removed (orchestration.md §4); use where: predicate instead",
	// Removed by [ADR-0083] §8 rather than deprecated: an author no longer says
	// which output is secret. The module declares it per field in its manifest
	// and the platform masks exactly those fields wherever the output is
	// observable — which is narrower than no_log ever was and cannot be
	// forgotten on a task.
	"no_log": "no_log: removed (ADR-0083 §8); a module declares `secret: true` on the output fields it returns, and the platform masks them — delete the key",
}

// schemaValidateScenario runs post-decode checks on a ScenarioManifest.
func schemaValidateScenario(path string, root *ast.MappingNode, m *ScenarioManifest) []diag.Diagnostic {
	_ = path
	var out []diag.Diagnostic

	topKeys := topLevelKeys(root)

	// 0) `name_template:` → `id_template:` ([ADR-0085] compatibility window). Folded
	// FIRST, so every later check — here, in the semantic phase, and post-merge in
	// the covenant resolver — reads one field whichever spelling the file used.
	out = append(out, m.normalizeIDTemplate(root)...)

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

	// 4a) `compute:` — structural validation (only if the key is present).
	if topKeys["compute"] {
		out = append(out, validateComputeBlock(root, "$.compute")...)
	}

	// 5) `input:` — shared schema validator.
	if topKeys["input"] {
		out = append(out, validateInputSchemaMap(m.Input, findInputMapping(root, "input"), "$.input", dialectInput, nil)...)
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

	// 5c) `id_template:` — server-side id composition from input components
	// (ADR-0079). Under the SAME covenant gate as `form:`: the cross-check
	// "every ${input.X} is declared" is correct only against the EFFECTIVE input,
	// which for an extends scenario exists only post-merge (checked there by the
	// same core, config.ResolveScenarioCovenant). The path is the spelling the file
	// actually used, so the diagnostic points at a key that is in the file.
	if (topKeys[idTemplateKey] || topKeys[nameTemplateKey]) && m.Extends == "" {
		out = append(out, validateIDTemplate(root, m, m.writtenIDTemplateKey(topKeys))...)
	}

	// 6) `tasks[]` — polymorphic validation of each task.
	tasksNode := findSequenceValue(root, "tasks")
	if tasksNode != nil {
		for i, item := range tasksNode.Values {
			out = append(out, validateTaskNode(item, fmt.Sprintf("$.tasks[%d]", i))...)
		}
	}

	// 7) Where each `assert:` can be answered — a roster-reading assert in a
	// create scenario is deferred to render, not answered pre-flight
	// ([validateAssertReachability], NIM-272). WARNING: the construct is legal,
	// only the author's expectation of a 422 is not.
	out = append(out, validateAssertReachability(tasksNode, m.Tasks, m.Create)...)

	return out
}

// reComputeName — compute variable name: must be CEL-field-accessible
// (`compute.<name>`), i.e. a snake/camel identifier starting with a letter or `_`,
// with letters/digits/underscore inside. Dash/dot/space are forbidden (they would
// break `compute.<name>` access in CEL).
var reComputeName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// computeReservedNames — names a compute variable must not shadow: the root CEL
// context names (input/register/incarnation/soulprint/vars) + the `compute`
// root itself. Compute variables live under `compute.<name>`, but the name `compute`
// as a variable would clobber the whole block — forbidden for self-documentation.
var computeReservedNames = map[string]bool{
	"input": true, "register": true, "incarnation": true,
	"soulprint": true, "vars": true, "compute": true,
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
				Hint:     "reserved: input, register, incarnation, soulprint, vars, compute",
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
// no kind filter.
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
	out := validateTaskRefs(findSequenceValue(root, "tasks"), "$.tasks", nil)
	out = append(out, validateExtendsField(m, root)...)
	return out
}

// validateExtendsField — semantic check of the `extends:` form (covenant.go). Empty/
// absent extends = no inheritance (valid, nothing checked — forward-compat). A
// non-empty name must be a valid covenant reference (ValidExtendsName: one segment,
// optionally under one subdirectory, traversal-clamped by the name grammar): else
// covenant_extends_invalid.
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
		Message: fmt.Sprintf("extends %q is not a valid covenant name: %s", m.Extends, refNameRejection(m.Extends, "")),
		Hint:    "one segment, optionally under one subdirectory (`covenant`, `shared/scenario_create`); names a covenant.yml-family fragment relative to the service root, no `..` and no second subdirectory level",
	})}
}
