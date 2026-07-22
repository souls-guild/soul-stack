package config

import (
	"fmt"
	"reflect"
	"regexp"
	"strings"

	"github.com/goccy/go-yaml"
	"github.com/goccy/go-yaml/ast"

	"github.com/souls-guild/soul-stack/shared/diag"
)

// Task is a polymorphic scenario task.
//
// The discriminator is the presence of exactly one of the keys `module:` / `apply:`
// / `include:` / `block:`. A non-nil discriminator field after Unmarshal indicates
// the task kind; the other three must be nil. Mutual exclusion is checked in
// validateTaskNode (via AST, not struct fields — that yields line/col for
// diagnostics).
//
// All "common" DSL-core fields (destiny/tasks.md §3) are opaque here
// (any/map[string]any) because at the shared/config level we validate only
// structure/types/regex; CEL parsing and cross-ref checks are deferred to M1.3/M1.5.
// At runtime the scenario-DSL applier fills/types them.
type Task struct {
	// Common (task DSL core).
	Name        string         `yaml:"name,omitempty"`
	Vars        map[string]any `yaml:"vars,omitempty"`
	When        string         `yaml:"when,omitempty"`
	Parallel    bool           `yaml:"parallel,omitempty"`
	Loop        *LoopSpec      `yaml:"loop,omitempty"`
	Register    string         `yaml:"register,omitempty"`
	ID          string         `yaml:"id,omitempty"`
	Output      map[string]any `yaml:"output,omitempty"`
	NoLog       bool           `yaml:"no_log,omitempty"`
	OnChanges   []string       `yaml:"onchanges,omitempty"`
	OnFail      []string       `yaml:"onfail,omitempty"`
	Require     any            `yaml:"require,omitempty"` // []string OR "all"
	ChangedWhen string         `yaml:"changed_when,omitempty"`
	FailedWhen  string         `yaml:"failed_when,omitempty"`
	Retry       *RetrySpec     `yaml:"retry,omitempty"`
	Timeout     string         `yaml:"timeout,omitempty"`

	// Scenario delta (orchestration.md §2).
	On      any    `yaml:"on,omitempty"`     // "keeper" | []string
	Where   string `yaml:"where,omitempty"`  // CEL string
	Serial  any    `yaml:"serial,omitempty"` // int >= 1 | "<N>%"
	RunOnce bool   `yaml:"run_once,omitempty"`

	// Discriminator (exactly one non-nil).
	Module  *ModuleTask  `yaml:"module,omitempty"`
	Apply   *ApplyTask   `yaml:"apply,omitempty"`
	Include *IncludeTask `yaml:"include,omitempty"`
	Block   *BlockTask   `yaml:"block,omitempty"`
	Assert  *AssertSpec  `yaml:"assert,omitempty"`

	// Carry-through conditional-include (ADR-009 amendment, conditional-include
	// group-drop). NON-YAML fields (`yaml:"-"` → not parsed from the manifest, absent
	// from taskKnownKeys/yamlFieldIndex — forward-compat like
	// RenderInput.destinyIsolated). Filled ONLY by [ExpandIncludes] when expanding an
	// include with `when:`: the include-when and the group id are propagated into
	// EVERY spliced task. Keeper-side render drops the whole group with one include-
	// when evaluation (by IncludeGroupID), BEFORE emitStaticWhenSkip. IncludeGroupID==0
	// — task outside a conditional include (the normal path); non-empty IncludeWhen ⇔
	// IncludeGroupID!=0.
	IncludeWhen    string `yaml:"-"`
	IncludeGroupID int    `yaml:"-"`
}

// AssertSpec is a keeper-side render-time precondition of a run (ADR-009 amendment
// 2026-06-23). `that` is a list of CEL-bool predicates (the whole string = CEL
// unwrapped, like `where:`), all required to be true in Keeper's render phase (full
// scenario context, soulprint.hosts available — AllowHosts=true). The first false
// aborts render with a clear error; assert emits no RenderedTask (a check, not a
// task). Task discriminator — mutually exclusive with module/apply/include/block.
type AssertSpec struct {
	That    []string `yaml:"that"`
	Message string   `yaml:"message,omitempty"`
}

// ModuleTask is a state-module invocation task. `params:` lives here (at the DSL-core
// level it is bound to the module task — destiny/tasks.md §4).
type ModuleTask struct {
	// Module is the module's string identifier "<ns>.<module>.<state>".
	Module string         `yaml:"-"`
	Params map[string]any `yaml:"params,omitempty"`
}

// ApplyTask is an applier task delegating work to a destiny.
type ApplyTask struct {
	Destiny string         `yaml:"destiny"`
	Input   map[string]any `yaml:"input,omitempty"`
}

// IncludeTask pulls in a sibling scenario file (or a service-level fallback via the
// two-level resolve, orchestration.md §6).
type IncludeTask struct {
	// Include is the relative file name (e.g. `install.yml`).
	Include string `yaml:"-"`
}

// BlockTask is an inline group of tasks. Its content is a top-level Task list, the
// same DSL core recursively.
type BlockTask struct {
	Block []Task `yaml:"-"`
}

// LoopSpec — DSL core §7. At the shared/config level we validate only structure; the
// type/value of `items` is CEL/template, parsing deferred.
type LoopSpec struct {
	Items   any    `yaml:"items"`
	As      string `yaml:"as,omitempty"`
	IndexAs string `yaml:"index_as,omitempty"`
	When    string `yaml:"when,omitempty"`
}

// RetrySpec — DSL core §9.
type RetrySpec struct {
	Count int    `yaml:"count"`
	Delay string `yaml:"delay,omitempty"`
	Until string `yaml:"until,omitempty"`
}

// taskCommonStringFields — a task's common string fields (DSL core §3 + scenario-
// delta `where:`). Used in validateTaskNode to check that the YAML node under the key
// is a string, not int/bool/seq/map. On NodeToValue into a string field goccy
// silently coerces an integer/bool scalar to a string, so without the AST check
// `name: 42` would pass as valid.
//
// `changed_when:`/`failed_when:` are EXCLUDED: they accept both a bool literal
// (force-shortcut) and a CEL string — a separate check, taskBoolOrCELFields.
var taskCommonStringFields = map[string]bool{
	"name":     true,
	"when":     true,
	"register": true,
	"id":       true,
	"timeout":  true,
	"where":    true,
}

// taskBoolOrCELFields — task-result override fields (`changed_when:` /
// `failed_when:`, destiny/tasks.md §"changed_when"/§"failed_when"). Two forms are
// allowed: a bool literal (`false`/`true` — a constant force-shortcut, "never changes
// state" / "never fails") OR a CEL string (predicate expression). int/float/seq/map —
// a type error.
var taskBoolOrCELFields = map[string]bool{
	"changed_when": true,
	"failed_when":  true,
}

// taskKnownKeys — the union of all legal task-level keys. Includes:
//   - DSL-core common keys (Task struct yaml tags, except discriminator fields
//     tagged `yaml:"-"`);
//   - discriminator keys (`module`, `apply`, `include`, `block`) — they are yaml:"-"
//     because UnmarshalYAML decodes them itself;
//   - `params:` — a neighbour of `module:`, technically living in ModuleTask;
//   - scenario delta (`on`, `where`, `serial`, `run_once`);
//   - deprecated (`wait`, `filter`) — their diagnostic is raised separately with a
//     hint in step 1 of validateTaskNode, but they must be in the whitelist to avoid
//     a duplicate `unknown_key`.
var taskKnownKeys = func() map[string]bool {
	out := map[string]bool{
		// discriminator keys (yaml:"-" in the struct → absent from yamlFieldIndex).
		"module":  true,
		"apply":   true,
		"include": true,
		"block":   true,
		// `params:` — a neighbour of `module:`, validated via ModuleTask.
		"params": true,
	}
	for k := range yamlFieldIndex(reflect.TypeOf(Task{})) {
		out[k] = true
	}
	for k := range deprecatedTaskKeys {
		out[k] = true
	}
	return out
}()

// Validation regexes.
var (
	// reModuleAddress — 3-level kebab-case `<ns>.<module>.<state>` for a scenario
	// module task. Symmetric to reRequiredModule (destiny.go) but adds the third
	// segment `<state>` — destiny/tasks.md §4.
	reModuleAddress = regexp.MustCompile(`^[a-z][a-z0-9]*(-[a-z0-9]+)*\.[a-z][a-z0-9]*(-[a-z0-9]+)*\.[a-z][a-z0-9]*(-[a-z0-9]+)*$`)

	// reIncludeFile — include file name. Only a `.yml` extension, no `/` and no `..`
	// (the engine does the two-level resolve, an author never writes `../`,
	// orchestration.md §6).
	reIncludeFile = regexp.MustCompile(`^[a-z][a-z0-9_-]*\.yml$`)

	// reRegisterID — identifier for `register:` and `id:` (a stable task address for
	// "task X changed" alerts, ADR-009-amend). One format because register and id
	// share ONE address space for per-task-changed subscriptions (ADR-052 §h).
	// Matches reInputParamName but is defined separately — different namespaces
	// (register/id vs input params) despite the identical form.
	reRegisterID = regexp.MustCompile(`^[a-z][a-z0-9_]*$`)

	// reLoopVar — identifier for `loop.as:`/`loop.index_as:`: snake_case, because the
	// name becomes a bare CEL variable (`${ <as>.* }` / `<as>.*` in expression keys,
	// destiny/tasks.md §7). Same form as the register id.
	reLoopVar = regexp.MustCompile(`^[a-z][a-z0-9_]*$`)
)

// SplitModuleAddr splits a module address `<namespace>.<module>.<state>` into
// (`<namespace>.<module>`, `state`). The last dot separates the state suffix; when
// absent it returns (addr, "", true) — a module without a concrete state (legacy
// plugins). Malformed strings (empty, `.state`, `core.`) return ok=false.
//
// The single source of truth for module-address parsing across all binaries: the
// Soul-side runtime (plantask/applyrunner) and Keeper-side scenario-dispatch call
// this instead of local copies. Symmetric to reModuleAddress, which validates the
// same three-segment form statically.
func SplitModuleAddr(addr string) (name, state string, ok bool) {
	if addr == "" {
		return "", "", false
	}
	idx := strings.LastIndexByte(addr, '.')
	if idx < 0 {
		// No dot — a module without a state (`core`).
		return addr, "", true
	}
	if idx == 0 || idx == len(addr)-1 {
		// `.state` / `core.` — malformed.
		return "", "", false
	}
	return addr[:idx], addr[idx+1:], true
}

// loopReservedNames — CEL context names that `loop.as:`/`loop.index_as:` must not
// shadow: a bare loop variable is declared at the top level of the activation
// (shared/cel) and would overwrite the fixed context. Symmetric to contextVars in
// shared/cel/engine.go.
var loopReservedNames = map[string]bool{
	"input":       true,
	"register":    true,
	"incarnation": true,
	"soulprint":   true,
	"essence":     true,
	"vars":        true,
}

// loopReservedPrefixes — name prefixes reserved for the engine's internal iter
// variables. `__host` is the filter-comprehension iter variable that
// `soulprint.hosts.where(...)` expands to (shared/cel/hosts.go, hostIterPrefix). A
// loop variable with this prefix would collide with the built-in iter variable if it
// landed in the same expression → forbidden at the config-validator level.
var loopReservedPrefixes = []string{"__host"}

// UnmarshalYAML is a custom Task decode. goccy's standard reflect decode cannot
// handle the discriminator (`module:` is a YAML scalar, *ModuleTask in Go), so we
// override it: decode common fields via an alias type, and the special ones
// (`module:`/`include:`/`block:`/`params:`) by hand.
func (t *Task) UnmarshalYAML(node ast.Node) error {
	mm, ok := node.(*ast.MappingNode)
	if !ok {
		// Scenario task as scalar/sequence: decode does nothing, leaving a zero-value
		// Task. validateTaskNode raises the `type_mismatch` diagnostic with
		// coordinates. Returning an error would stack a `decode_fault` from
		// parseAndValidate on top, giving a double diagnostic at the same coordinate.
		return nil
	}

	// Peel the "special" keys into separate nodes and build a filtered map without
	// them to pass through the alias type.
	var (
		moduleNode  ast.Node
		paramsNode  ast.Node
		includeNode ast.Node
		blockNode   ast.Node
	)
	filtered := &ast.MappingNode{
		BaseNode:    mm.BaseNode,
		Start:       mm.Start,
		End:         mm.End,
		IsFlowStyle: mm.IsFlowStyle,
		Values:      make([]*ast.MappingValueNode, 0, len(mm.Values)),
	}
	for _, kv := range mm.Values {
		tok := kv.Key.GetToken()
		if tok != nil {
			switch tok.Value {
			case "module":
				moduleNode = kv.Value
				continue
			case "params":
				paramsNode = kv.Value
				continue
			case "include":
				includeNode = kv.Value
				continue
			case "block":
				blockNode = kv.Value
				continue
			}
		}
		filtered.Values = append(filtered.Values, kv)
	}

	// Decode "everything else" via the alias type (avoids recursion).
	type rawTask Task
	var raw rawTask
	if err := yaml.NodeToValue(filtered, &raw); err != nil {
		return err
	}
	*t = Task(raw)

	// module: <string> → ModuleTask with an optional params:.
	if moduleNode != nil {
		mt := &ModuleTask{}
		if sn, ok := moduleNode.(*ast.StringNode); ok {
			mt.Module = sn.Value
		}
		if paramsNode != nil {
			if pm, ok := paramsNode.(*ast.MappingNode); ok {
				params := map[string]any{}
				if err := yaml.NodeToValue(pm, &params); err == nil {
					mt.Params = params
				}
			}
		}
		t.Module = mt
	} else if paramsNode != nil {
		// `params:` without `module:` is illegal (params is bound to the module
		// task), but validateTaskNode validates that via the AST. Do nothing here so
		// as not to lose the diagnostic.
		_ = paramsNode
	}

	// include: <string> → IncludeTask.
	if includeNode != nil {
		it := &IncludeTask{}
		if sn, ok := includeNode.(*ast.StringNode); ok {
			it.Include = sn.Value
		}
		t.Include = it
	}

	// block: [tasks...] → BlockTask.
	if blockNode != nil {
		bt := &BlockTask{}
		if seq, ok := blockNode.(*ast.SequenceNode); ok {
			bt.Block = make([]Task, len(seq.Values))
			for i, item := range seq.Values {
				if err := bt.Block[i].UnmarshalYAML(item); err != nil {
					return fmt.Errorf("block[%d]: %w", i, err)
				}
			}
		}
		t.Block = bt
	}

	return nil
}

// validateTaskNode validates one element of `tasks[]` (or an element inside
// `block:`). It takes an AST node (line/col are needed), not an already-decoded Task
// — the discriminator check and unknown_key are read straight from the AST.
//
// Returns the accumulated diagnostics. Never returns an error: anything that fails to
// parse is recorded as an error-level diag.Diagnostic.
func validateTaskNode(item ast.Node, pathPrefix string) []diag.Diagnostic {
	mm, ok := item.(*ast.MappingNode)
	if !ok {
		tok := item.GetToken()
		line, col := 0, 0
		if tok != nil {
			line, col = tok.Position.Line, tok.Position.Column
		}
		return []diag.Diagnostic{diagAt(line, col, diag.Diagnostic{
			Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
			Code:     "type_mismatch",
			Message:  "scenario task must be a mapping",
			YAMLPath: pathPrefix,
		})}
	}

	var out []diag.Diagnostic

	// Collect the present keys and their value nodes (for position diagnostics).
	present := map[string]*ast.MappingValueNode{}
	for _, kv := range mm.Values {
		tok := kv.Key.GetToken()
		if tok == nil {
			continue
		}
		present[tok.Value] = kv
	}

	// 1) Deprecated task-level keys (`wait:` / `filter:`) and unknown_key for anything
	// not in the whitelist. Symmetric to top-level destiny/service/scenario: without
	// this check typos like `wheree`/`reigster` passed with exit 0.
	for k, kv := range present {
		if hint, dep := deprecatedTaskKeys[k]; dep {
			tok := kv.Key.GetToken()
			out = append(out, diagAt(tok.Position.Line, tok.Position.Column, diag.Diagnostic{
				Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
				Code:     "unknown_key",
				Message:  `unknown field "` + k + `"`,
				Hint:     hint,
				YAMLPath: pathPrefix + "." + k,
			}))
			continue
		}
		if !taskKnownKeys[k] {
			tok := kv.Key.GetToken()
			out = append(out, diagAt(tok.Position.Line, tok.Position.Column, diag.Diagnostic{
				Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
				Code:     "unknown_key",
				Message:  `unknown field "` + k + `"`,
				Hint:     "see docs/destiny/tasks.md §3 and docs/scenario/orchestration.md §2 for the full list of task keys",
				YAMLPath: pathPrefix + "." + k,
			}))
		}
	}

	// 1a) Common string fields: name/when/register/timeout/where must be strings
	// (changed_when/failed_when moved to 1b — bool literal OR CEL). On NodeToValue
	// goccy silently coerces a scalar int/bool/float into a string field, so we check
	// via the AST.
	for k, kv := range present {
		if !taskCommonStringFields[k] {
			continue
		}
		if _, isStr := kv.Value.(*ast.StringNode); isStr {
			continue
		}
		// null is allowed — it means "field unset", same effect as absent.
		if _, isNull := kv.Value.(*ast.NullNode); isNull {
			continue
		}
		vt := kv.Value.GetToken()
		line, col := 0, 0
		if vt != nil {
			line, col = vt.Position.Line, vt.Position.Column
		}
		out = append(out, diagAt(line, col, diag.Diagnostic{
			Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
			Code:     "type_mismatch",
			Message:  fmt.Sprintf("%s: must be a string", k),
			YAMLPath: pathPrefix + "." + k,
		}))
	}

	// 1b) changed_when/failed_when: bool literal (force-shortcut) OR CEL string.
	// `changed_when: false`/`failed_when: false` is an idiomatic shortcut (read-only
	// step / ignore-errors, destiny/tasks.md). int/float/seq/map — type_mismatch.
	for k, kv := range present {
		if !taskBoolOrCELFields[k] {
			continue
		}
		switch kv.Value.(type) {
		case *ast.StringNode, *ast.BoolNode, *ast.NullNode:
			// string = CEL expression; bool = constant force; null = "unset".
			continue
		}
		vt := kv.Value.GetToken()
		line, col := 0, 0
		if vt != nil {
			line, col = vt.Position.Line, vt.Position.Column
		}
		out = append(out, diagAt(line, col, diag.Diagnostic{
			Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
			Code:     "type_mismatch",
			Message:  fmt.Sprintf("%s: must be a bool literal (force) or a CEL string (expression)", k),
			Hint:     "changed_when: false (force not-changed) | changed_when: \"<cel>\"",
			YAMLPath: pathPrefix + "." + k,
		}))
	}

	// 2) Discriminator: exactly one of {module, apply, include, block, assert}.
	// `assert:` is a render-time precondition (ADR-009 amendment): a check, NOT an
	// executable task, so it shares the discriminator slot with the other kinds
	// (mutually exclusive with module/apply/include/block).
	discrKeys := []string{"module", "apply", "include", "block", "assert"}
	var found []string
	for _, k := range discrKeys {
		if _, ok := present[k]; ok {
			found = append(found, k)
		}
	}
	switch {
	case len(found) == 0:
		tok := mm.GetToken()
		line, col := 0, 0
		if tok != nil {
			line, col = tok.Position.Line, tok.Position.Column
		}
		out = append(out, diagAt(line, col, diag.Diagnostic{
			Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
			Code:     "task_discriminator_missing",
			Message:  "task must declare exactly one of: module / apply / include / block / assert",
			Hint:     "see docs/destiny/tasks.md §2 and docs/scenario/orchestration.md §2",
			YAMLPath: pathPrefix,
		}))
	case len(found) > 1:
		// Diagnostic on the second+ discriminator key; the first is the "primary".
		for i := 1; i < len(found); i++ {
			kv := present[found[i]]
			tok := kv.Key.GetToken()
			out = append(out, diagAt(tok.Position.Line, tok.Position.Column, diag.Diagnostic{
				Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
				Code:     "task_discriminator_multiple",
				Message:  fmt.Sprintf("task declares multiple discriminators: %v — exactly one of {module, apply, include, block, assert} is allowed", found),
				Hint:     "see docs/destiny/tasks.md §2",
				YAMLPath: pathPrefix + "." + found[i],
			}))
		}
	}

	// 3) Discriminator-specific validation.
	if kv, ok := present["module"]; ok {
		out = append(out, validateModuleField(kv, pathPrefix)...)
		// `params:` is required on a module task (even if `{}`). Without it the
		// applier cannot tell "forgot to pass" from "passed `{}`". See
		// docs/destiny/tasks.md §4. The type of `params:` is validated via
		// ModuleTask.Params (map[string]any).
		if _, hasParams := present["params"]; !hasParams {
			tok := kv.Key.GetToken()
			out = append(out, diagAt(tok.Position.Line, tok.Position.Column, diag.Diagnostic{
				Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
				Code:     "missing_required_field",
				Message:  "params: is required on a module task (use params: {} if there are no inputs)",
				Hint:     "module: <ns>.<module>.<state>\\n    params: { ... }  # or {}",
				YAMLPath: pathPrefix + ".params",
			}))
		}
		// Phase that checks params against the module's manifest schema (core modules).
		out = append(out, validateModuleParams(kv, present["params"], pathPrefix)...)
	}
	if kv, ok := present["apply"]; ok {
		out = append(out, validateApplyField(kv, pathPrefix)...)
	}
	if kv, ok := present["assert"]; ok {
		out = append(out, validateAssertField(kv, pathPrefix)...)
	}
	if kv, ok := present["include"]; ok {
		out = append(out, validateIncludeField(kv, pathPrefix)...)
	}
	if kv, ok := present["block"]; ok {
		out = append(out, validateBlockField(kv, pathPrefix)...)
		// `register:` on a block task is semantically meaningless (a block invokes no
		// module), destiny/tasks.md §6.5. Raise a separate code.
		if rkv, hasReg := present["register"]; hasReg {
			tok := rkv.Key.GetToken()
			out = append(out, diagAt(tok.Position.Line, tok.Position.Column, diag.Diagnostic{
				Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
				Code:     "register_on_block_invalid",
				Message:  "register: is not allowed on a block task (block does not invoke a module)",
				Hint:     "place register: on the inner module-task that produces the value",
				YAMLPath: pathPrefix + ".register",
			}))
		}
		out = append(out, validateBlockForbiddenKeys(present, pathPrefix)...)
	}

	// 4) Scenario delta: on / serial / run_once. `where:` is already checked in block
	// 1a (the common string-field check); CEL parsing deferred to M1.3.
	if kv, ok := present["on"]; ok {
		out = append(out, validateOnField(kv, pathPrefix)...)
	}
	if kv, ok := present["serial"]; ok {
		out = append(out, validateSerialField(kv, pathPrefix)...)
	}
	// serial: and run_once: are mutually exclusive (orchestration.md §2.2.2 "`run_once`").
	_, hasSerial := present["serial"]
	_, hasRunOnce := present["run_once"]
	if hasSerial && hasRunOnce {
		// Diagnostic on run_once (the second key's position), as for the discriminator.
		kv := present["run_once"]
		tok := kv.Key.GetToken()
		out = append(out, diagAt(tok.Position.Line, tok.Position.Column, diag.Diagnostic{
			Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
			Code:     "serial_run_once_conflict",
			Message:  "serial: and run_once: are mutually exclusive (different width strategies)",
			Hint:     "use serial: for rolling waves of N hosts; run_once: for a single deterministic host",
			YAMLPath: pathPrefix + ".run_once",
		}))
	}

	// 4a) Requisite fields onchanges/onfail/require — type. onchanges/onfail are
	// strictly list-of-string; require is list-of-string OR the scalar "all". Without
	// this check a scalar instead of a list (`onchanges: redis_conf`) passed silently:
	// the cross-ref in validateTaskRefs looks only at the sequence form and skips a
	// scalar, so a form typo was caught nowhere.
	for _, k := range []string{"onchanges", "onfail"} {
		if kv, ok := present[k]; ok {
			out = append(out, validateRequisiteListField(kv, k, pathPrefix)...)
		}
	}
	if kv, ok := present["require"]; ok {
		out = append(out, validateRequireField(kv, pathPrefix)...)
	}

	// 5) Universal fields: loop, register, retry, timeout.
	if kv, ok := present["loop"]; ok {
		out = append(out, validateLoopField(kv, present, pathPrefix)...)
	}
	if kv, ok := present["register"]; ok {
		out = append(out, validateRegisterField(kv, pathPrefix)...)
	}
	if kv, ok := present["id"]; ok {
		out = append(out, validateIDField(kv, present, pathPrefix)...)
	}
	if kv, ok := present["retry"]; ok {
		out = append(out, validateRetryField(kv, pathPrefix)...)
	}
	if kv, ok := present["timeout"]; ok {
		// The type check (string) is already done in block 1a; don't re-emit
		// type_mismatch, only the format.
		if _, isStr := kv.Value.(*ast.StringNode); isStr {
			out = append(out, validateDurationField(kv, pathPrefix+".timeout")...)
		}
	}

	return out
}

// validateModuleField — `module: <ns>.<module>.<state>` string + an optional
// `params:` neighbour (params is validated against the module schema at M1.5; for now
// only the map type is checked implicitly via UnmarshalYAML).
func validateModuleField(kv *ast.MappingValueNode, pathPrefix string) []diag.Diagnostic {
	sn, ok := kv.Value.(*ast.StringNode)
	if !ok {
		tok := kv.Value.GetToken()
		return []diag.Diagnostic{diagAt(tok.Position.Line, tok.Position.Column, diag.Diagnostic{
			Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
			Code:     "type_mismatch",
			Message:  "module: must be a string in form <namespace>.<module>.<state>",
			YAMLPath: pathPrefix + ".module",
		})}
	}
	if !reModuleAddress.MatchString(sn.Value) {
		tok := sn.GetToken()
		return []diag.Diagnostic{diagAt(tok.Position.Line, tok.Position.Column, diag.Diagnostic{
			Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
			Code:     "module_format_invalid",
			Message:  fmt.Sprintf("module %q does not match <namespace>.<module>.<state>", sn.Value),
			Hint:     "three-level kebab-case address; e.g. core.pkg.installed, core.file.rendered",
			YAMLPath: pathPrefix + ".module",
		})}
	}
	return nil
}

// validateApplyField — apply: task: `destiny:` + an `input:` neighbour.
func validateApplyField(kv *ast.MappingValueNode, pathPrefix string) []diag.Diagnostic {
	mm, ok := kv.Value.(*ast.MappingNode)
	if !ok {
		tok := kv.Value.GetToken()
		return []diag.Diagnostic{diagAt(tok.Position.Line, tok.Position.Column, diag.Diagnostic{
			Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
			Code:     "type_mismatch",
			Message:  "apply: must be a mapping with destiny: + input:",
			YAMLPath: pathPrefix + ".apply",
		})}
	}
	var out []diag.Diagnostic
	var hasDestiny bool
	for _, sub := range mm.Values {
		tok := sub.Key.GetToken()
		if tok == nil {
			continue
		}
		switch tok.Value {
		case "destiny":
			hasDestiny = true
			sn, isStr := sub.Value.(*ast.StringNode)
			if !isStr {
				vt := sub.Value.GetToken()
				out = append(out, diagAt(vt.Position.Line, vt.Position.Column, diag.Diagnostic{
					Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
					Code:     "type_mismatch",
					Message:  "apply.destiny must be a string",
					YAMLPath: pathPrefix + ".apply.destiny",
				}))
				continue
			}
			if !reDestinyName.MatchString(sn.Value) {
				vt := sn.GetToken()
				out = append(out, diagAt(vt.Position.Line, vt.Position.Column, diag.Diagnostic{
					Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
					Code:     "name_invalid_format",
					Message:  fmt.Sprintf("apply.destiny %q does not match %s", sn.Value, reDestinyName),
					Hint:     "kebab-case destiny name from service.yml → destiny[]",
					YAMLPath: pathPrefix + ".apply.destiny",
				}))
			}
		case "input":
			if _, isMap := sub.Value.(*ast.MappingNode); !isMap {
				// `input: null` is allowed (no input). `input: <scalar>` —
				// type_mismatch. For null goccy creates a nil node; skip it.
				if _, isNull := sub.Value.(*ast.NullNode); !isNull {
					vt := sub.Value.GetToken()
					out = append(out, diagAt(vt.Position.Line, vt.Position.Column, diag.Diagnostic{
						Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
						Code:     "type_mismatch",
						Message:  "apply.input must be a mapping",
						YAMLPath: pathPrefix + ".apply.input",
					}))
				}
			}
		default:
			out = append(out, diagAt(tok.Position.Line, tok.Position.Column, diag.Diagnostic{
				Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
				Code:     "unknown_key",
				Message:  `unknown field "` + tok.Value + `" in apply:`,
				Hint:     "apply: accepts only destiny: + input:",
				YAMLPath: pathPrefix + ".apply." + tok.Value,
			}))
		}
	}
	if !hasDestiny {
		tok := kv.Key.GetToken()
		out = append(out, diagAt(tok.Position.Line, tok.Position.Column, diag.Diagnostic{
			Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
			Code:     "missing_required_field",
			Message:  "apply.destiny is required",
			Hint:     "apply: { destiny: <name>, input: { ... } }",
			YAMLPath: pathPrefix + ".apply.destiny",
		}))
	}
	return out
}

// validateAssertField — assert task (render-time precondition, ADR-009 amendment
// 2026-06-23): `assert: { that: [<CEL-bool>…], message?: <str> }`.
//
// `that` is required, a non-empty list of strings (each whole string = CEL-bool, CEL
// parsing deferred to the render phase — like `where:`). `message` is optional, a
// string (default message if omitted). Other keys inside assert: — unknown_key
// (fail-closed).
func validateAssertField(kv *ast.MappingValueNode, pathPrefix string) []diag.Diagnostic {
	mm, ok := kv.Value.(*ast.MappingNode)
	if !ok {
		tok := kv.Value.GetToken()
		return []diag.Diagnostic{diagAt(tok.Position.Line, tok.Position.Column, diag.Diagnostic{
			Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
			Code:     "type_mismatch",
			Message:  "assert: must be a mapping with that: + optional message:",
			YAMLPath: pathPrefix + ".assert",
		})}
	}
	var out []diag.Diagnostic
	var hasThat bool
	for _, sub := range mm.Values {
		tok := sub.Key.GetToken()
		if tok == nil {
			continue
		}
		switch tok.Value {
		case "that":
			hasThat = true
			seq, isSeq := sub.Value.(*ast.SequenceNode)
			if !isSeq {
				vt := sub.Value.GetToken()
				out = append(out, diagAt(vt.Position.Line, vt.Position.Column, diag.Diagnostic{
					Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
					Code:     "type_mismatch",
					Message:  "assert.that must be a non-empty list of CEL-bool predicate strings",
					YAMLPath: pathPrefix + ".assert.that",
				}))
				continue
			}
			if len(seq.Values) == 0 {
				vt := sub.Value.GetToken()
				out = append(out, diagAt(vt.Position.Line, vt.Position.Column, diag.Diagnostic{
					Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
					Code:     "missing_required_field",
					Message:  "assert.that must not be empty (at least one CEL-bool predicate)",
					Hint:     "assert: { that: [ \"<cel>\" ], message: \"...\" }",
					YAMLPath: pathPrefix + ".assert.that",
				}))
				continue
			}
			for j, item := range seq.Values {
				if _, isStr := item.(*ast.StringNode); isStr {
					continue
				}
				it := item.GetToken()
				line, col := 0, 0
				if it != nil {
					line, col = it.Position.Line, it.Position.Column
				}
				out = append(out, diagAt(line, col, diag.Diagnostic{
					Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
					Code:     "type_mismatch",
					Message:  fmt.Sprintf("assert.that[%d]: must be a string (CEL-bool predicate)", j),
					YAMLPath: fmt.Sprintf("%s.assert.that[%d]", pathPrefix, j),
				}))
			}
		case "message":
			if _, isStr := sub.Value.(*ast.StringNode); !isStr {
				if _, isNull := sub.Value.(*ast.NullNode); !isNull {
					vt := sub.Value.GetToken()
					out = append(out, diagAt(vt.Position.Line, vt.Position.Column, diag.Diagnostic{
						Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
						Code:     "type_mismatch",
						Message:  "assert.message must be a string",
						YAMLPath: pathPrefix + ".assert.message",
					}))
				}
			}
		default:
			out = append(out, diagAt(tok.Position.Line, tok.Position.Column, diag.Diagnostic{
				Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
				Code:     "unknown_key",
				Message:  `unknown field "` + tok.Value + `" in assert:`,
				Hint:     "assert: accepts only that: + message:",
				YAMLPath: pathPrefix + ".assert." + tok.Value,
			}))
		}
	}
	if !hasThat {
		tok := kv.Key.GetToken()
		out = append(out, diagAt(tok.Position.Line, tok.Position.Column, diag.Diagnostic{
			Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
			Code:     "missing_required_field",
			Message:  "assert.that is required",
			Hint:     "assert: { that: [ \"<cel>\" ], message: \"...\" }",
			YAMLPath: pathPrefix + ".assert.that",
		}))
	}
	return out
}

// validateIncludeField — `include: <file>.yml`.
func validateIncludeField(kv *ast.MappingValueNode, pathPrefix string) []diag.Diagnostic {
	sn, ok := kv.Value.(*ast.StringNode)
	if !ok {
		tok := kv.Value.GetToken()
		return []diag.Diagnostic{diagAt(tok.Position.Line, tok.Position.Column, diag.Diagnostic{
			Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
			Code:     "type_mismatch",
			Message:  "include: must be a string (file name)",
			YAMLPath: pathPrefix + ".include",
		})}
	}
	if !reIncludeFile.MatchString(sn.Value) {
		tok := sn.GetToken()
		return []diag.Diagnostic{diagAt(tok.Position.Line, tok.Position.Column, diag.Diagnostic{
			Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
			Code:     "name_invalid_format",
			Message:  fmt.Sprintf("include %q must be a sibling file name ending in .yml (no slashes, no ..)", sn.Value),
			Hint:     "two-level resolve is done by the engine; authors never write ../ — see orchestration.md §6",
			YAMLPath: pathPrefix + ".include",
		})}
	}
	return nil
}

// validateBlockField — `block:` — an array of nested tasks, validated recursively.
func validateBlockField(kv *ast.MappingValueNode, pathPrefix string) []diag.Diagnostic {
	seq, ok := kv.Value.(*ast.SequenceNode)
	if !ok {
		tok := kv.Value.GetToken()
		return []diag.Diagnostic{diagAt(tok.Position.Line, tok.Position.Column, diag.Diagnostic{
			Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
			Code:     "type_mismatch",
			Message:  "block: must be a sequence of tasks",
			YAMLPath: pathPrefix + ".block",
		})}
	}
	var out []diag.Diagnostic
	for i, item := range seq.Values {
		out = append(out, validateTaskNode(item, fmt.Sprintf("%s.block[%d]", pathPrefix, i))...)
	}
	return out
}

// blockForbiddenKeys — module-specific keys not allowed at the BLOCK level (fail-
// closed; destiny/tasks.md §6.5 does not mention them on a block). A block invokes no
// module, so a module-result override (`changed_when`/`failed_when`), one call's
// retry/timeout/output/no_log, and `params:` (module arguments) are meaningless on
// it. Each key is rejected with code `<key>_on_block_invalid` (symmetric to
// register_on_block_invalid). `register:` is already rejected separately above.
//
// `parallel:` is also outside the pilot block (parallel on a block is a later slice)
// — rejected by the same mechanism, code parallel_on_block_invalid.
//
// Keys inherited by a block (`when`/`where`/`vars`/`onchanges`/`onfail`/`require`/
// `on`/`serial`/`run_once`/`name`/`loop`) are NOT in the list: §6.5 explicitly allows
// them on a block.
var blockForbiddenKeys = []string{
	"changed_when",
	"failed_when",
	"retry",
	"timeout",
	"output",
	"no_log",
	"params",
	"parallel",
}

// validateBlockForbiddenKeys raises `<key>_on_block_invalid` for each present module-
// specific key on a block task (fail-closed, §6.5). Called only when the
// discriminator is block.
func validateBlockForbiddenKeys(present map[string]*ast.MappingValueNode, pathPrefix string) []diag.Diagnostic {
	var out []diag.Diagnostic
	for _, key := range blockForbiddenKeys {
		kv, ok := present[key]
		if !ok {
			continue
		}
		tok := kv.Key.GetToken()
		out = append(out, diagAt(tok.Position.Line, tok.Position.Column, diag.Diagnostic{
			Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
			Code:     key + "_on_block_invalid",
			Message:  fmt.Sprintf("%s: is not allowed on a block task (block does not invoke a module — see docs/destiny/tasks.md §6.5)", key),
			Hint:     "place module-specific keys on the inner module-task; block: only carries when/where/vars/requisites/serial/run_once",
			YAMLPath: pathPrefix + "." + key,
		}))
	}
	return out
}

// validateOnField — `on:` literal `keeper` or a sequence of strings (coven-ids).
func validateOnField(kv *ast.MappingValueNode, pathPrefix string) []diag.Diagnostic {
	switch v := kv.Value.(type) {
	case *ast.StringNode:
		// `on: keeper` — the only allowed string form (orchestration.md §3).
		if v.Value != "keeper" {
			tok := v.GetToken()
			return []diag.Diagnostic{diagAt(tok.Position.Line, tok.Position.Column, diag.Diagnostic{
				Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
				Code:     "enum_invalid",
				Message:  fmt.Sprintf("on: %q — only 'keeper' is allowed as scalar; use a sequence of coven-ids otherwise", v.Value),
				Hint:     "on: keeper | on: [coven-a, coven-b]",
				YAMLPath: pathPrefix + ".on",
			})}
		}
		return nil
	case *ast.SequenceNode:
		var out []diag.Diagnostic
		for i, item := range v.Values {
			itemPath := fmt.Sprintf("%s.on[%d]", pathPrefix, i)
			sn, ok := item.(*ast.StringNode)
			if !ok {
				tok := item.GetToken()
				out = append(out, diagAt(tok.Position.Line, tok.Position.Column, diag.Diagnostic{
					Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
					Code:     "type_mismatch",
					Message:  "on[]: must be strings (coven-ids)",
					YAMLPath: itemPath,
				}))
				continue
			}
			// Skip a CEL wrapper `${ ... }` — it is a valid coven resolver
			// (e.g. `${ incarnation.name }`). Apply the regex form only to bare
			// names.
			if !isCELWrapped(sn.Value) {
				if !reCovenName.MatchString(sn.Value) {
					tok := sn.GetToken()
					out = append(out, diagAt(tok.Position.Line, tok.Position.Column, diag.Diagnostic{
						Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
						Code:     "name_invalid_format",
						Message:  fmt.Sprintf("on[%d] %q is not a valid coven-id (kebab-case)", i, sn.Value),
						Hint:     "kebab-case literal or CEL ${ ... } expression",
						YAMLPath: itemPath,
					}))
				} else if err := covenLabelValidator().Validate(sn.Value); err != nil {
					// Optional hook on top of the format: a pluggable
					// CovenLabelValidator (Q1b registry, ADR-008-amend). In the
					// pilot — a no-op; the real implementation is injected via
					// SetCovenLabelValidator at client startup.
					tok := sn.GetToken()
					out = append(out, diagAt(tok.Position.Line, tok.Position.Column, diag.Diagnostic{
						Level: diag.LevelError, Phase: diag.PhaseSemanticValidate,
						Code:     "coven_label_unknown",
						Message:  fmt.Sprintf("on[%d] %q is not a recognized coven label: %v", i, sn.Value, err),
						Hint:     "check the spelling against the coven registry; until the registry exists, only kebab-case format is enforced",
						YAMLPath: itemPath,
					}))
				}
			}
		}
		return out
	default:
		tok := kv.Value.GetToken()
		return []diag.Diagnostic{diagAt(tok.Position.Line, tok.Position.Column, diag.Diagnostic{
			Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
			Code:     "type_mismatch",
			Message:  "on: must be the string 'keeper' or a sequence of coven-id strings",
			YAMLPath: pathPrefix + ".on",
		})}
	}
}

// validateSerialField — int >= 1 or a percent string `"<N>%"`.
func validateSerialField(kv *ast.MappingValueNode, pathPrefix string) []diag.Diagnostic {
	switch v := kv.Value.(type) {
	case *ast.IntegerNode:
		// goccy types integers as int64/uint64 via GetValue. Use the token's
		// string representation for portability.
		tok := v.GetToken()
		// A coarse but sufficient check: the token must not be "0" or
		// negative. For negatives goccy parses the sign as part of the token.
		if tok.Value == "0" || (len(tok.Value) > 0 && tok.Value[0] == '-') {
			return []diag.Diagnostic{diagAt(tok.Position.Line, tok.Position.Column, diag.Diagnostic{
				Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
				Code:     "value_out_of_range",
				Message:  fmt.Sprintf("serial: %s must be >= 1", tok.Value),
				YAMLPath: pathPrefix + ".serial",
			})}
		}
		return nil
	case *ast.StringNode:
		if _, ok := ParseSerialPercent(v.Value); !ok {
			tok := v.GetToken()
			return []diag.Diagnostic{diagAt(tok.Position.Line, tok.Position.Column, diag.Diagnostic{
				Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
				Code:     "value_out_of_range",
				Message:  fmt.Sprintf("serial: %q must match int >= 1 OR percent-form \"<N>%%\" (1..99)", v.Value),
				Hint:     "examples: serial: 1, serial: 3, serial: \"25%\"",
				YAMLPath: pathPrefix + ".serial",
			})}
		}
		return nil
	default:
		tok := kv.Value.GetToken()
		return []diag.Diagnostic{diagAt(tok.Position.Line, tok.Position.Column, diag.Diagnostic{
			Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
			Code:     "type_mismatch",
			Message:  "serial: must be int >= 1 or percent-form \"<N>%\"",
			YAMLPath: pathPrefix + ".serial",
		})}
	}
}

// validateRegisterField — identifier. The type check (string) is done in the
// common block 1a of validateTaskNode; here — only the format.
func validateRegisterField(kv *ast.MappingValueNode, pathPrefix string) []diag.Diagnostic {
	sn, ok := kv.Value.(*ast.StringNode)
	if !ok {
		return nil
	}
	if !reRegisterID.MatchString(sn.Value) {
		tok := sn.GetToken()
		return []diag.Diagnostic{diagAt(tok.Position.Line, tok.Position.Column, diag.Diagnostic{
			Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
			Code:     "register_identifier_invalid",
			Message:  fmt.Sprintf("register %q does not match %s", sn.Value, reRegisterID),
			Hint:     "snake_case identifier: starts with a-z; only [a-z0-9_]",
			YAMLPath: pathPrefix + ".register",
		})}
	}
	return nil
}

// validateIDField — `id:` — a stable task address for subscribing to "task X
// changed" alerts (ADR-009-amend, ADR-052 §h). Optional. The type check
// (string) is done in common block 1a of validateTaskNode; here — format +
// mutual exclusions.
//
// Format — the register format (same reRegisterID): id and register share ONE
// subscription address space.
//
// Mutual exclusions:
//   - `id` ⊕ `register`: a task with register is already addressable by it → id
//     is redundant and makes the address ambiguous. Error id_register_conflict.
//   - `id` only on a module task (pilot): block/include have no changed signal of
//     their own. id is forbidden on them for now (error id_unsupported_target);
//     lifting the restriction is a separate change on request.
func validateIDField(kv *ast.MappingValueNode, present map[string]*ast.MappingValueNode, pathPrefix string) []diag.Diagnostic {
	sn, ok := kv.Value.(*ast.StringNode)
	if !ok {
		return nil
	}

	var out []diag.Diagnostic

	if !reRegisterID.MatchString(sn.Value) {
		tok := sn.GetToken()
		out = append(out, diagAt(tok.Position.Line, tok.Position.Column, diag.Diagnostic{
			Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
			Code:     "id_identifier_invalid",
			Message:  fmt.Sprintf("id %q does not match %s", sn.Value, reRegisterID),
			Hint:     "snake_case identifier: starts with a-z; only [a-z0-9_]",
			YAMLPath: pathPrefix + ".id",
		}))
	}

	// id ⊕ register: a task with register already has an address.
	if _, hasReg := present["register"]; hasReg {
		tok := kv.Key.GetToken()
		out = append(out, diagAt(tok.Position.Line, tok.Position.Column, diag.Diagnostic{
			Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
			Code:     "id_register_conflict",
			Message:  "id: and register: cannot both be set on one task — a task with register is already addressable; use id only on tasks without register",
			Hint:     "keep only register: (subscribe to alerts by its name), or drop register: and use id:",
			YAMLPath: pathPrefix + ".id",
		}))
	}

	// pilot: id only on a module task.
	if _, isModule := present["module"]; !isModule {
		tok := kv.Key.GetToken()
		out = append(out, diagAt(tok.Position.Line, tok.Position.Column, diag.Diagnostic{
			Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
			Code:     "id_unsupported_target",
			Message:  "id: on a block/include task is not supported yet — in the pilot id is allowed only on a module task (it has its own changed signal)",
			Hint:     "put id: on a specific module task; extending to block/include is a separate change",
			YAMLPath: pathPrefix + ".id",
		}))
	}

	return out
}

// validateLoopField — `loop: { items: <required>, as?, index_as?, when? }`
// (destiny/tasks.md §7).
//
// Slice E1: `loop:` is supported only on a module task (render-time fan-out,
// see orchestration.md §2.2). On include:/apply:/block: — rejected with
// code loop_unsupported_target (loop expansion for those kinds is deferred).
//
// items: is required; the type/value (CEL/template-expr) is not checked — parsing
// happens in the render phase. as/index_as — valid snake_case identifiers (if
// set) and not from the reserved context. when: — a string (CEL predicate).
func validateLoopField(kv *ast.MappingValueNode, present map[string]*ast.MappingValueNode, pathPrefix string) []diag.Diagnostic {
	// Slice E1: loop is legitimate only on a module task.
	if _, isModule := present["module"]; !isModule {
		tok := kv.Key.GetToken()
		return []diag.Diagnostic{diagAt(tok.Position.Line, tok.Position.Column, diag.Diagnostic{
			Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
			Code:     "loop_unsupported_target",
			Message:  "loop: is supported only on a module task (loop on include/apply/block is not yet implemented)",
			Hint:     "wrap the iterated work in a module task, or split into separate tasks",
			YAMLPath: pathPrefix + ".loop",
		})}
	}

	mm, ok := kv.Value.(*ast.MappingNode)
	if !ok {
		tok := kv.Value.GetToken()
		return []diag.Diagnostic{diagAt(tok.Position.Line, tok.Position.Column, diag.Diagnostic{
			Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
			Code:     "type_mismatch",
			Message:  "loop: must be a mapping with items: + optional as:/index_as:/when:",
			YAMLPath: pathPrefix + ".loop",
		})}
	}

	var out []diag.Diagnostic
	knownLoopKeys := map[string]bool{"items": true, "as": true, "index_as": true, "when": true}
	var hasItems bool
	var asNode, indexAsNode *ast.MappingValueNode
	for _, sub := range mm.Values {
		tok := sub.Key.GetToken()
		if tok == nil {
			continue
		}
		k := tok.Value
		if !knownLoopKeys[k] {
			out = append(out, diagAt(tok.Position.Line, tok.Position.Column, diag.Diagnostic{
				Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
				Code:     "unknown_key",
				Message:  `unknown field "` + k + `" in loop:`,
				Hint:     "loop: accepts only items, as, index_as, when",
				YAMLPath: pathPrefix + ".loop." + k,
			}))
			continue
		}
		switch k {
		case "items":
			hasItems = true
			// Type not checked: items is a CEL/template-expr (a `${ … }` string)
			// or an inline literal (seq/map), parsed in the render phase.
		case "as":
			asNode = sub
			out = append(out, validateLoopVar(sub, k, pathPrefix)...)
		case "index_as":
			indexAsNode = sub
			out = append(out, validateLoopVar(sub, k, pathPrefix)...)
		case "when":
			if _, isStr := sub.Value.(*ast.StringNode); !isStr {
				if _, isNull := sub.Value.(*ast.NullNode); !isNull {
					vt := sub.Value.GetToken()
					out = append(out, diagAt(vt.Position.Line, vt.Position.Column, diag.Diagnostic{
						Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
						Code:     "type_mismatch",
						Message:  "loop.when must be a string (CEL predicate)",
						YAMLPath: pathPrefix + ".loop.when",
					}))
				}
			}
		}
	}
	if !hasItems {
		tok := kv.Key.GetToken()
		out = append(out, diagAt(tok.Position.Line, tok.Position.Column, diag.Diagnostic{
			Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
			Code:     "missing_required_field",
			Message:  "loop.items is required",
			Hint:     "loop: { items: ${ input.<x> }, as: <name> }",
			YAMLPath: pathPrefix + ".loop.items",
		}))
	}
	// as: and index_as: must differ: in the render context they go into a
	// shared loop-variable map, and an identical name would silently overwrite the
	// element with the index (`as=x, index_as=x` → the host gets the index, not the element).
	if asNode != nil && indexAsNode != nil {
		asStr, asOK := asNode.Value.(*ast.StringNode)
		ixStr, ixOK := indexAsNode.Value.(*ast.StringNode)
		if asOK && ixOK && asStr.Value == ixStr.Value {
			tok := ixStr.GetToken()
			out = append(out, diagAt(tok.Position.Line, tok.Position.Column, diag.Diagnostic{
				Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
				Code:     "loop_var_conflict",
				Message:  fmt.Sprintf("loop.as and loop.index_as are both %q; they must differ", asStr.Value),
				Hint:     "use distinct names, e.g. as: item, index_as: i",
				YAMLPath: pathPrefix + ".loop.index_as",
			}))
		}
	}
	return out
}

// validateLoopVar — `loop.as:`/`loop.index_as:` — a snake_case identifier,
// not from the reserved CEL context (it becomes a bare variable).
func validateLoopVar(sub *ast.MappingValueNode, key, pathPrefix string) []diag.Diagnostic {
	sn, isStr := sub.Value.(*ast.StringNode)
	if !isStr {
		vt := sub.Value.GetToken()
		return []diag.Diagnostic{diagAt(vt.Position.Line, vt.Position.Column, diag.Diagnostic{
			Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
			Code:     "type_mismatch",
			Message:  fmt.Sprintf("loop.%s must be a string identifier", key),
			YAMLPath: pathPrefix + ".loop." + key,
		})}
	}
	if !reLoopVar.MatchString(sn.Value) {
		tok := sn.GetToken()
		return []diag.Diagnostic{diagAt(tok.Position.Line, tok.Position.Column, diag.Diagnostic{
			Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
			Code:     "loop_var_invalid",
			Message:  fmt.Sprintf("loop.%s %q does not match %s", key, sn.Value, reLoopVar),
			Hint:     "snake_case identifier: starts with a-z; only [a-z0-9_]",
			YAMLPath: pathPrefix + ".loop." + key,
		})}
	}
	if loopReservedNames[sn.Value] {
		tok := sn.GetToken()
		return []diag.Diagnostic{diagAt(tok.Position.Line, tok.Position.Column, diag.Diagnostic{
			Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
			Code:     "loop_var_reserved",
			Message:  fmt.Sprintf("loop.%s %q shadows a reserved CEL context name", key, sn.Value),
			Hint:     "reserved: input, register, incarnation, soulprint, essence, vars",
			YAMLPath: pathPrefix + ".loop." + key,
		})}
	}
	for _, prefix := range loopReservedPrefixes {
		if strings.HasPrefix(sn.Value, prefix) {
			tok := sn.GetToken()
			return []diag.Diagnostic{diagAt(tok.Position.Line, tok.Position.Column, diag.Diagnostic{
				Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
				Code:     "loop_var_reserved",
				Message:  fmt.Sprintf("loop.%s %q uses reserved prefix %q (engine filter iter-variable)", key, sn.Value, prefix),
				Hint:     "the __host* prefix is reserved for the soulprint.hosts.where(...) filter; choose another name",
				YAMLPath: pathPrefix + ".loop." + key,
			})}
		}
	}
	return nil
}

// validateRequisiteListField — `onchanges:`/`onfail:` strictly list-of-string
// (destiny/tasks.md §"onchanges"/§"onfail"). scalar/map/int → type_mismatch;
// a non-string list element → type_mismatch on the element. null = "unset".
// Register names are checked by the cross-ref phase (validateTaskRefs) — here only
// the form.
func validateRequisiteListField(kv *ast.MappingValueNode, key, pathPrefix string) []diag.Diagnostic {
	if _, isNull := kv.Value.(*ast.NullNode); isNull {
		return nil
	}
	seq, ok := kv.Value.(*ast.SequenceNode)
	if !ok {
		tok := kv.Value.GetToken()
		return []diag.Diagnostic{diagAt(tok.Position.Line, tok.Position.Column, diag.Diagnostic{
			Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
			Code:     "type_mismatch",
			Message:  fmt.Sprintf("%s: must be a list of register names", key),
			Hint:     fmt.Sprintf("%s: [register_a, register_b]", key),
			YAMLPath: pathPrefix + "." + key,
		})}
	}
	return requisiteListElems(seq, key, pathPrefix)
}

// validateRequireField — `require:` allows two forms: a list of register names
// OR the scalar "all" (orchestration/destiny tasks). A scalar != "all" → error;
// a non-string list element → type_mismatch. null = "unset".
func validateRequireField(kv *ast.MappingValueNode, pathPrefix string) []diag.Diagnostic {
	switch v := kv.Value.(type) {
	case *ast.NullNode:
		return nil
	case *ast.SequenceNode:
		return requisiteListElems(v, "require", pathPrefix)
	case *ast.StringNode:
		if v.Value == "all" {
			return nil
		}
		tok := v.GetToken()
		return []diag.Diagnostic{diagAt(tok.Position.Line, tok.Position.Column, diag.Diagnostic{
			Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
			Code:     "type_mismatch",
			Message:  fmt.Sprintf("require: scalar %q is invalid — use a list of register names or the literal \"all\"", v.Value),
			Hint:     "require: [register_a] | require: all",
			YAMLPath: pathPrefix + ".require",
		})}
	default:
		tok := kv.Value.GetToken()
		return []diag.Diagnostic{diagAt(tok.Position.Line, tok.Position.Column, diag.Diagnostic{
			Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
			Code:     "type_mismatch",
			Message:  "require: must be a list of register names or the literal \"all\"",
			Hint:     "require: [register_a] | require: all",
			YAMLPath: pathPrefix + ".require",
		})}
	}
}

// requisiteListElems — each requisite-list element must be a string
// (a register name or a CEL wrapper). int/bool/seq/map in an element → type_mismatch.
func requisiteListElems(seq *ast.SequenceNode, key, pathPrefix string) []diag.Diagnostic {
	var out []diag.Diagnostic
	for j, item := range seq.Values {
		if _, isStr := item.(*ast.StringNode); isStr {
			continue
		}
		tok := item.GetToken()
		line, col := 0, 0
		if tok != nil {
			line, col = tok.Position.Line, tok.Position.Column
		}
		out = append(out, diagAt(line, col, diag.Diagnostic{
			Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
			Code:     "type_mismatch",
			Message:  fmt.Sprintf("%s[%d]: must be a string (register name)", key, j),
			YAMLPath: fmt.Sprintf("%s.%s[%d]", pathPrefix, key, j),
		}))
	}
	return out
}

// validateRetryField — `retry: { count: >=1, delay: duration, until: string }`.
func validateRetryField(kv *ast.MappingValueNode, pathPrefix string) []diag.Diagnostic {
	mm, ok := kv.Value.(*ast.MappingNode)
	if !ok {
		tok := kv.Value.GetToken()
		return []diag.Diagnostic{diagAt(tok.Position.Line, tok.Position.Column, diag.Diagnostic{
			Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
			Code:     "type_mismatch",
			Message:  "retry: must be a mapping",
			YAMLPath: pathPrefix + ".retry",
		})}
	}
	var out []diag.Diagnostic
	knownRetryKeys := map[string]bool{"count": true, "delay": true, "until": true}
	var hasCount bool
	for _, sub := range mm.Values {
		tok := sub.Key.GetToken()
		if tok == nil {
			continue
		}
		k := tok.Value
		if !knownRetryKeys[k] {
			out = append(out, diagAt(tok.Position.Line, tok.Position.Column, diag.Diagnostic{
				Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
				Code:     "unknown_key",
				Message:  `unknown field "` + k + `" in retry:`,
				Hint:     "retry: accepts only count, delay, until",
				YAMLPath: pathPrefix + ".retry." + k,
			}))
			continue
		}
		switch k {
		case "count":
			in, isInt := sub.Value.(*ast.IntegerNode)
			if !isInt {
				vt := sub.Value.GetToken()
				out = append(out, diagAt(vt.Position.Line, vt.Position.Column, diag.Diagnostic{
					Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
					Code:     "type_mismatch",
					Message:  "retry.count must be an integer",
					YAMLPath: pathPrefix + ".retry.count",
				}))
				continue
			}
			hasCount = true
			it := in.GetToken()
			if it.Value == "0" || (len(it.Value) > 0 && it.Value[0] == '-') {
				out = append(out, diagAt(it.Position.Line, it.Position.Column, diag.Diagnostic{
					Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
					Code:     "value_out_of_range",
					Message:  fmt.Sprintf("retry.count must be >= 1, got %s", it.Value),
					YAMLPath: pathPrefix + ".retry.count",
				}))
			}
		case "delay":
			out = append(out, validateDurationField(sub, pathPrefix+".retry.delay")...)
		case "until":
			if _, isStr := sub.Value.(*ast.StringNode); !isStr {
				vt := sub.Value.GetToken()
				out = append(out, diagAt(vt.Position.Line, vt.Position.Column, diag.Diagnostic{
					Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
					Code:     "type_mismatch",
					Message:  "retry.until must be a string (CEL predicate)",
					YAMLPath: pathPrefix + ".retry.until",
				}))
			}
		}
	}
	if !hasCount {
		tok := kv.Key.GetToken()
		out = append(out, diagAt(tok.Position.Line, tok.Position.Column, diag.Diagnostic{
			Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
			Code:     "missing_required_field",
			Message:  "retry.count is required",
			Hint:     "retry: { count: <int>, delay: <duration>, until: <expr> }",
			YAMLPath: pathPrefix + ".retry.count",
		}))
	}
	return out
}

// validateDurationField validates a string against Soul Stack's `duration` convention
// (config.ParseDuration): Go time.ParseDuration (`30s`/`5m`/`1h30m`) plus
// the `<N>d` suffix for days (`30d`). One convention with keeper.yml validation,
// Reaper and core.url — see docs/keeper/config.md → "Type conventions".
// Applies to all destiny duration fields (task.timeout, retry.delay, etc.).
func validateDurationField(kv *ast.MappingValueNode, yamlPath string) []diag.Diagnostic {
	sn, ok := kv.Value.(*ast.StringNode)
	if !ok {
		tok := kv.Value.GetToken()
		return []diag.Diagnostic{diagAt(tok.Position.Line, tok.Position.Column, diag.Diagnostic{
			Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
			Code:     "type_mismatch",
			Message:  "duration must be a string (Go duration syntax or <N>d for days)",
			YAMLPath: yamlPath,
		})}
	}
	if _, err := ParseDuration(sn.Value); err != nil {
		tok := sn.GetToken()
		return []diag.Diagnostic{diagAt(tok.Position.Line, tok.Position.Column, diag.Diagnostic{
			Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
			Code:     "duration_invalid",
			Message:  fmt.Sprintf("invalid duration %q: %v", sn.Value, err),
			Hint:     "examples: 30s, 5m, 1h30m, 30d",
			YAMLPath: yamlPath,
		})}
	}
	return nil
}

// isCELWrapped — true if the whole string is a single `${ … }` wrapper. At M1.2.c
// we do not parse CEL, but a coarse recognition is needed: `${ incarnation.name }` in
// `on:` literals must not be caught by the kebab-case regex.
func isCELWrapped(s string) bool {
	if len(s) < 4 {
		return false
	}
	if s[0] != '$' || s[1] != '{' {
		return false
	}
	return s[len(s)-1] == '}'
}
