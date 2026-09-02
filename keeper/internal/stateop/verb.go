package stateop

import (
	"fmt"
	"sort"
	"strings"

	"github.com/souls-guild/soul-stack/keeper/internal/render"
	"github.com/souls-guild/soul-stack/shared/config"
	"github.com/souls-guild/soul-stack/shared/coremanifest"
)

// The author form of a capture step ([ADR-0084]): its address, its states and
// its params. This half of the package answers "what does this task ask for",
// [Merge] answers "what does it do to the state" — and both live here so the
// keeper-side module that executes a capture and the offline harness that has to
// predict one build the SAME operation out of the same params. A second parser
// would be a second answer to what a verb means.

// ModuleName is the base module name without the state suffix (Registry key).
// The author form of the task address is `core.state.<verb>`.
//
// Taken from the shared catalog (NIM-749), which is also what routes the step
// keeper-side: this module's dispatch key and the address the engine reads the
// side off cannot be allowed to drift apart.
const ModuleName = coremanifest.StateModuleAddr

// Param names.
const (
	ParamField      = "field"
	ParamValue      = "value"
	ParamKey        = "key"
	ParamMatch      = "match"
	ParamPatch      = "patch"
	ParamOnConflict = "on_conflict"
	ParamExpect     = "expect"
)

// The author-visible state suffixes. `core.state` alone has no verb and is
// refused — the suffix is not decoration here, it IS the operation.
const (
	StateSet     = "set"
	StatePresent = "present"
	StateAdd     = "add"
	StateAppend  = "append"
	StateModify  = "modify"
	StateRemove  = "remove"
	StateUnset   = "unset"
)

// Spec is one author-visible state of the module: which verb it maps to, which
// params it takes, and whether its `value:` is the WHOLE field or ONE element of
// a collection — the two shapes secret resolution has to tell apart.
type Spec struct {
	Verb config.StateVerb
	// TakesValue — the state carries a `value:` param, and that value is what
	// secret resolution runs over. `modify`/`remove`/`unset` mint nothing.
	TakesValue bool
	// Element — `value:` is one element of the collection rather than the whole
	// field, which moves the declared secret one level up in the value.
	Element bool
	// NeedsEval — the verb evaluates a per-element CEL predicate and cannot run
	// without the merge-time evaluators.
	NeedsEval bool
	// Required / Optional are the params BEYOND `field` (and `value`, which
	// TakesValue already covers). A param outside the two lists is refused rather
	// than ignored: `core.state.set` given a `match:` would otherwise look like a
	// filtered write and be a wholesale overwrite.
	Required []string
	Optional []string
}

// States is the address table. The keys are the author-visible suffixes; adding
// one here without adding it to `shared/coremanifest`'s module manifest would
// leave soul-lint rejecting a state the module accepts, and the reverse would
// let soul-lint green-light one the module rejects at runtime — the manifest
// golden test is what holds the two together.
var States = map[string]Spec{
	StateSet:     {Verb: config.VerbSet, TakesValue: true},
	StatePresent: {Verb: config.VerbPresent, TakesValue: true},
	StateAdd:     {Verb: config.VerbAdd, TakesValue: true, Element: true, NeedsEval: true, Optional: []string{ParamKey, ParamMatch, ParamOnConflict}},
	StateAppend:  {Verb: config.VerbAppend, TakesValue: true, Element: true},
	StateModify:  {Verb: config.VerbModify, NeedsEval: true, Required: []string{ParamPatch}, Optional: []string{ParamMatch, ParamExpect}},
	StateRemove:  {Verb: config.VerbRemove, NeedsEval: true, Optional: []string{ParamMatch, ParamExpect}},
	StateUnset:   {Verb: config.VerbUnset},
}

// KnownStates lists the suffixes for a diagnostic, sorted so the message is
// byte-identical for identical input.
func KnownStates() string {
	return strings.Join(SortedKeys(States), "/")
}

// CheckParams reports every param problem of one state at once, so an author
// fixing a task sees the whole list instead of one error per run.
func CheckParams(params map[string]any, spec Spec) []string {
	var errs []string
	if _, err := StringParam(params, ParamField); err != nil {
		errs = append(errs, err.Error())
	}
	if spec.TakesValue {
		if _, ok := params[ParamValue]; !ok {
			errs = append(errs, fmt.Sprintf("param %q: missing", ParamValue))
		}
	}
	for _, name := range spec.Required {
		if _, ok := params[name]; !ok {
			errs = append(errs, fmt.Sprintf("param %q: missing", name))
		}
	}
	allowed := map[string]bool{ParamField: true, ParamValue: spec.TakesValue}
	for _, name := range append(append([]string{}, spec.Required...), spec.Optional...) {
		allowed[name] = true
	}
	for _, name := range SortedKeys(params) {
		if !allowed[name] {
			// Refused, not ignored: a param the verb does not read looks like it
			// narrowed the write and did not.
			errs = append(errs, fmt.Sprintf("param %q: not a param of state %q", name, spec.Verb))
		}
	}
	return errs
}

// BuildOp assembles the merge operation from the state's own params. Enum params
// are checked here rather than passed through: `on_conflict: replce` would
// otherwise fall to the engine's default (skip) and silently keep the old
// element.
//
// The `value:` is deliberately NOT read here. On the module path it is the
// resolved one (secrets minted, references substituted) and the caller assigns it
// after resolution; a harness that cannot mint assigns the rendered one. Reading
// it here would make the caller's assignment look optional.
func BuildOp(spec Spec, field string, params map[string]any) (render.RenderedOp, error) {
	op := render.RenderedOp{Verb: spec.Verb, Field: field}
	if raw, ok := params[ParamKey]; ok {
		key, err := asString(raw, ParamKey)
		if err != nil {
			return op, err
		}
		op.Key = key
	}
	if raw, ok := params[ParamMatch]; ok {
		match, err := asString(raw, ParamMatch)
		if err != nil {
			return op, err
		}
		op.Match = match
	}
	if raw, ok := params[ParamOnConflict]; ok {
		v, err := asEnum(raw, ParamOnConflict, []string{
			string(config.OnConflictSkip), string(config.OnConflictReplace), string(config.OnConflictError)})
		if err != nil {
			return op, err
		}
		op.OnConflict = config.OnConflict(v)
	}
	if raw, ok := params[ParamExpect]; ok {
		v, err := asEnum(raw, ParamExpect, []string{
			string(config.ExpectAny), string(config.ExpectOne), string(config.ExpectAtMostOne)})
		if err != nil {
			return op, err
		}
		op.Expect = config.Expect(v)
	}
	if raw, ok := params[ParamPatch]; ok {
		patch, ok := raw.(map[string]any)
		if !ok {
			return op, fmt.Errorf("param %q: expected an object of path-in-element -> value, got %T", ParamPatch, raw)
		}
		op.Patch = patch
	}
	// key: and match: are the two spellings of element identity and belong to
	// different collection kinds (key: to a map, match: to a list). Which one the
	// engine honours is decided by the field's kind, not by which the author wrote,
	// so a task carrying both means one of them is silently ignored.
	if op.Key != "" && op.Match != "" {
		return op, fmt.Errorf("params %q and %q are mutually exclusive: %q identifies an element of a MAP field, %q an element of a LIST field",
			ParamKey, ParamMatch, ParamKey, ParamMatch)
	}
	return op, nil
}

// OpsFromPlan builds, in plan order, the merge operation of every
// `core.state.<verb>` step of a rendered plan ([ADR-0084]). A task of another
// module is skipped; a `core.state.<something-else>` is an error rather than a
// skip, because the address IS the operation — an unknown suffix names a step
// nobody can execute, and silently dropping it would predict a state the run
// never reaches.
//
// It lives next to [States] and [BuildOp] because two callers have to predict
// what a run does to the state WITHOUT running it: the L0 trial harness, which
// has no incarnation row to write to, and the render->merge unit tests. Each had
// a loop of its own, and the test-side one skipped [CheckParams] — so it accepted
// a param combination the keeper refuses at dispatch, which is exactly the
// green-on-a-plan-the-run-rejects drift a shared [BuildOp] exists to prevent.
//
// The module itself does NOT come through here: it is handed one dispatched task
// at a time and mints secrets between building the op and assigning `value:`.
// What it shares is everything below the loop.
func OpsFromPlan(tasks []*render.RenderedTask) ([]render.RenderedOp, error) {
	var out []render.RenderedOp
	for i := range tasks {
		name, verb, ok := config.SplitModuleAddr(tasks[i].Module)
		if !ok || name != ModuleName {
			continue
		}
		// A skip placeholder keeps the module address of the task it stands for
		// (static when:false, block/loop skip, future-Passage stub) and carries no
		// params, because the render never ran. It writes nothing, so it is not an
		// op. The marker is `Params == nil` — a rendered module task always carries
		// a non-nil Struct, empty params included; there is no Skip flag (same
		// marker the trial's task matching uses).
		if tasks[i].Params == nil {
			continue
		}
		spec, known := States[verb]
		if !known {
			return nil, fmt.Errorf("task %q: unknown state %q of %s (want %s)",
				tasks[i].Name, verb, ModuleName, KnownStates())
		}
		params := tasks[i].Params.AsMap()
		if errs := CheckParams(params, spec); len(errs) > 0 {
			return nil, fmt.Errorf("task %q (%s): %s", tasks[i].Name, tasks[i].Module, strings.Join(errs, "; "))
		}
		field, _ := StringParam(params, ParamField)
		op, err := BuildOp(spec, field, params)
		if err != nil {
			return nil, fmt.Errorf("task %q (%s): %w", tasks[i].Name, tasks[i].Module, err)
		}
		if spec.TakesValue {
			op.Value = params[ParamValue]
		}
		out = append(out, op)
	}
	return out, nil
}

// StringParam reads a required string param out of the decoded params map.
func StringParam(params map[string]any, key string) (string, error) {
	v, ok := params[key]
	if !ok || v == nil {
		return "", fmt.Errorf("param %q: missing", key)
	}
	s, ok := v.(string)
	if !ok {
		return "", fmt.Errorf("param %q: expected string, got %T", key, v)
	}
	if strings.TrimSpace(s) == "" {
		return "", fmt.Errorf("param %q: empty", key)
	}
	return s, nil
}

// asString reads a param that must be a non-empty string.
func asString(raw any, name string) (string, error) {
	s, ok := raw.(string)
	if !ok {
		return "", fmt.Errorf("param %q: expected string, got %T", name, raw)
	}
	if strings.TrimSpace(s) == "" {
		return "", fmt.Errorf("param %q: empty", name)
	}
	return s, nil
}

// asEnum reads a param that must be one of a closed set of strings.
func asEnum(raw any, name string, allowed []string) (string, error) {
	s, err := asString(raw, name)
	if err != nil {
		return "", err
	}
	for _, a := range allowed {
		if s == a {
			return s, nil
		}
	}
	return "", fmt.Errorf("param %q: unknown value %q (want %s)", name, s, strings.Join(allowed, "/"))
}

// SortedKeys returns a map's keys in sorted order — every walk must produce
// byte-identical output for identical input.
func SortedKeys[T any](m map[string]T) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
