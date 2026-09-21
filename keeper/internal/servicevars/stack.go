package servicevars

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"strings"

	securejoin "github.com/cyphar/filepath-securejoin"
	"github.com/souls-guild/soul-stack/shared/cel"
	"github.com/souls-guild/soul-stack/shared/config"
)

// Aliases onto the schema in shared/config. The shape of `_stack.yaml` is shared
// with soul-lint, which reports the same class offline as `stack_step_invalid`;
// what stays here is the execution — CEL evaluation, the layer merge, and path
// containment.
type (
	stackDoc  = config.ServiceVarsStack
	stackStep = config.ServiceVarsStackStep
)

const (
	strategyDeep    = config.StrategyDeep
	strategyReplace = config.StrategyReplace
)

// ErrStackStepInvalid — re-exported so callers in this package keep matching one
// sentinel regardless of which side raised it.
var ErrStackStepInvalid = config.ErrStackStepInvalid

// readStack reads `<serviceDir>/vars/_stack.yaml`. Missing file → (nil, nil):
// the caller falls back to lexical order over the directory.
func (r *Resolver) readStack(serviceDir string) (*stackDoc, error) {
	full, err := securejoin.SecureJoin(serviceDir, varsDir+"/"+stackFile)
	if err != nil {
		return nil, fmt.Errorf("servicevars: unsafe stack path: %w", err)
	}
	data, err := os.ReadFile(full)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("servicevars: read %s: %w", stackFile, err)
	}
	doc, err := config.ParseServiceVarsStack(data)
	if err != nil {
		return nil, fmt.Errorf("servicevars: %w", err)
	}
	return doc, nil
}

// resolveStack runs the declarative pipeline. Each step sees the vars ACCUMULATED
// so far under `vars.*`, so a later step can branch on what an earlier one
// loaded — which is the whole reason `_stack.yaml` exists rather than a sorted
// directory.
//
// The step env is its own, NOT the scenario's CEL context: `incarnation.*` (the
// row's own fields, covens included) plus `vars.*` plus the `foreach:` binding.
// There is deliberately no `soulprint.self` — a service's vars are resolved ONCE
// per run and handed to every host, so a step keyed on one host's facts would
// silently apply that host's answer to the whole roster. Per-host values are the
// job of task `vars:` and `apply: input:`, which are rendered per host.
func (r *Resolver) resolveStack(in ResolveInput, doc *stackDoc) (map[string]any, error) {
	engine, err := cel.NewServiceVars()
	if err != nil {
		return nil, fmt.Errorf("servicevars: cel engine: %w", err)
	}

	incVars := in.Incarnation.celMap()
	result := make(map[string]any)
	for i, step := range doc.Stack {
		binds, err := r.stepBindings(engine, incVars, result, step)
		if err != nil {
			return nil, fmt.Errorf("servicevars: %s step %d: %w", stackFile, i+1, err)
		}
		for _, bind := range binds {
			vars := cel.Vars{Incarnation: incVars, Vars: result, Loop: bind}

			if step.When != "" {
				on, err := evalBool(engine, step.When, vars)
				if err != nil {
					return nil, fmt.Errorf("servicevars: %s step %d: when: %w", stackFile, i+1, err)
				}
				if !on {
					continue
				}
			}

			layer, err := r.stepLayer(engine, in, step, vars)
			if err != nil {
				return nil, fmt.Errorf("servicevars: %s step %d: %w", stackFile, i+1, err)
			}
			result, err = applyLayer(result, layer, step.Strategy)
			if err != nil {
				return nil, fmt.Errorf("servicevars: %s step %d: %w", stackFile, i+1, err)
			}
		}
	}
	return result, nil
}

// stepBindings expands `foreach:` into one binding per element. A step without
// `foreach:` runs exactly once, with no binding. An empty list runs it zero
// times — the same outcome as a false `when:`, and not an error.
func (r *Resolver) stepBindings(engine *cel.Engine, incVars, acc map[string]any, step stackStep) ([]map[string]any, error) {
	if step.Foreach == "" {
		return []map[string]any{nil}, nil
	}
	raw, err := engine.EvalInterpolation(step.Foreach, cel.Vars{Incarnation: incVars, Vars: acc})
	if err != nil {
		return nil, fmt.Errorf("foreach: %w", err)
	}
	items, err := toList(raw)
	if err != nil {
		return nil, fmt.Errorf("%w: foreach: %v", ErrStackStepInvalid, err)
	}
	out := make([]map[string]any, 0, len(items))
	for _, item := range items {
		out = append(out, map[string]any{step.As: item})
	}
	return out, nil
}

// stepLayer produces the step's layer: an interpolated inline map, or the YAML
// file named by an interpolated path. A missing file is an error unless the step
// says `optional: true` — the default is loud, because a path built from an
// expression is exactly where a typo hides.
func (r *Resolver) stepLayer(engine *cel.Engine, in ResolveInput, step stackStep, vars cel.Vars) (map[string]any, error) {
	if step.Inline != nil {
		return renderInline(engine, step.Inline, vars)
	}

	rendered, err := engine.EvalInterpolation(step.File, vars)
	if err != nil {
		return nil, fmt.Errorf("file: %w", err)
	}
	rel, ok := rendered.(string)
	if !ok {
		return nil, fmt.Errorf("%w: file: must render to a string, got %T", ErrStackStepInvalid, rendered)
	}
	if rel == "" {
		return nil, fmt.Errorf("%w: file: rendered empty", ErrStackStepInvalid)
	}

	// `file:` is documented relative to `vars/`, and a composed path that climbs
	// out of it stays inside the SNAPSHOT — securejoin clamps at the service root,
	// not at vars/. So `file: "../service.yml"` succeeds and merges the manifest
	// into the vars namespace. Refuse it: the step said vars/, and reading
	// something else under that name is a scope surprise nothing would report.
	if cleaned := path.Clean(rel); cleaned == ".." || strings.HasPrefix(cleaned, "../") || path.IsAbs(cleaned) {
		return nil, fmt.Errorf("%w: file: %q leaves the vars/ directory", ErrStackStepInvalid, rel)
	}

	layer, found, err := r.readLayer(in.ServiceDir, varsDir+"/"+rel)
	if err != nil {
		return nil, err
	}
	if !found && !step.Optional {
		// Absence is the interesting case here: unlike the lexical scan, where
		// ReadDir listed the file, this path is composed from an expression and a
		// typo in it has no other symptom. A file that EXISTS and parses to
		// nothing is not this case and is left alone.
		return nil, fmt.Errorf("servicevars: file %q not found (add optional: true to tolerate it)", rel)
	}
	return layer, nil
}

// renderInline interpolates the string values of an inline layer.
//
// Only TOP-LEVEL string values, the same limitation destiny `vars.yml` states —
// but where that document merely warns, this REFUSES. An author who has just
// watched `acl_path: "${ vars.conf_dir }/users.acl"` work has no reason to expect
// the next indent level to differ, and the consequence of passing it through is
// the literal text `${ vars.mirror }/deb` reaching an apt sources line or a
// systemd unit, never re-evaluated downstream. A marker one level in is a mistake
// worth an error naming the key.
func renderInline(engine *cel.Engine, inline map[string]any, vars cel.Vars) (map[string]any, error) {
	out := make(map[string]any, len(inline))
	for k, v := range inline {
		s, ok := v.(string)
		if !ok {
			if nested := nestedMarkerPath(k, v); nested != "" {
				return nil, fmt.Errorf("%w: inline: %s carries a nested ${ … } that would NOT be interpolated; "+
					"lift it to a top-level key", ErrStackStepInvalid, nested)
			}
			out[k] = v
			continue
		}
		rendered, err := engine.EvalInterpolation(s, vars)
		if err != nil {
			return nil, fmt.Errorf("inline: %s: %w", k, err)
		}
		out[k] = rendered
	}
	return out, nil
}

// nestedMarkerPath walks a non-string inline value and returns the path of the
// first string containing an interpolation marker, or "" if there is none.
func nestedMarkerPath(prefix string, v any) string {
	switch t := v.(type) {
	case string:
		if strings.Contains(t, "${") {
			return prefix
		}
	case map[string]any:
		for k, nested := range t {
			if p := nestedMarkerPath(prefix+"."+k, nested); p != "" {
				return p
			}
		}
	case []any:
		for i, nested := range t {
			if p := nestedMarkerPath(fmt.Sprintf("%s[%d]", prefix, i), nested); p != "" {
				return p
			}
		}
	}
	return ""
}

// applyLayer merges a layer into the accumulator, per key, under the most
// specific strategy that applies to it: the file's own `_strategy` for that key,
// else the file's whole-file `_strategy`, else the step's.
//
// The file gets the last word because that is where the intent lives — a
// `10-mirror.yaml` knows it is an override — and because in lexical mode, with no
// `_stack.yaml` to carry a step, it is the only voice there is.
func applyLayer(acc, layer map[string]any, stepStrategy string) (map[string]any, error) {
	layer, perKey, fileWide, err := takeLayerStrategy(layer)
	if err != nil {
		return nil, err
	}
	if acc == nil {
		acc = make(map[string]any, len(layer))
	}
	for k, v := range layer {
		strategy := stepStrategy
		if fileWide != "" {
			strategy = fileWide
		}
		if s, ok := perKey[k]; ok {
			strategy = s
		}
		if strategy == strategyReplace {
			acc[k] = v
			continue
		}
		acc = mergeInto(acc, map[string]any{k: v})
	}
	return acc, nil
}

// takeLayerStrategy lifts the reserved `_strategy` key out of a layer and returns
// the layer without it. A malformed declaration is refused rather than ignored:
// `_strategy: shallow` silently deep-merging is the fail-open this package's error
// type exists to prevent, and a stray `_strategy` left in the map would surface
// later as a var nobody declared.
func takeLayerStrategy(layer map[string]any) (rest map[string]any, perKey map[string]string, fileWide string, err error) {
	raw, present := layer[strategyKey]
	if !present {
		return layer, nil, "", nil
	}
	rest = make(map[string]any, len(layer)-1)
	for k, v := range layer {
		if k != strategyKey {
			rest[k] = v
		}
	}
	switch t := raw.(type) {
	case string:
		if !validStrategy(t) {
			return nil, nil, "", fmt.Errorf("%w: %s: %q is not deep or replace", ErrStackStepInvalid, strategyKey, t)
		}
		return rest, nil, t, nil
	case map[string]any:
		perKey = make(map[string]string, len(t))
		for k, v := range t {
			s, ok := v.(string)
			if !ok || !validStrategy(s) {
				return nil, nil, "", fmt.Errorf("%w: %s.%s: %v is not deep or replace", ErrStackStepInvalid, strategyKey, k, v)
			}
			perKey[k] = s
		}
		return rest, perKey, "", nil
	default:
		return nil, nil, "", fmt.Errorf("%w: %s must be a string or a map of key→strategy, got %T", ErrStackStepInvalid, strategyKey, raw)
	}
}

func validStrategy(s string) bool { return s == strategyDeep || s == strategyReplace }

// evalBool evaluates a bare CEL predicate. A non-bool result is an error, not a
// truthiness guess.
func evalBool(engine *cel.Engine, expr string, vars cel.Vars) (bool, error) {
	val, err := engine.EvalExpression(expr, vars)
	if err != nil {
		return false, err
	}
	b, ok := val.Value().(bool)
	if !ok {
		return false, fmt.Errorf("%w: when: must be a bool, got %T", ErrStackStepInvalid, val.Value())
	}
	return b, nil
}

// toList normalizes a CEL list result to []any. A string is REFUSED rather than
// treated as a one-element list: `foreach: "${ incarnation.name }"` is a mistake
// worth an error, not a loop of one.
func toList(v any) ([]any, error) {
	switch t := v.(type) {
	case []any:
		return t, nil
	case []string:
		out := make([]any, len(t))
		for i, s := range t {
			out[i] = s
		}
		return out, nil
	case nil:
		return nil, nil
	default:
		return nil, fmt.Errorf("must be a list, got %T", v)
	}
}
