package servicevars

import (
	"errors"
	"fmt"
	"io/fs"
	"path"
	"regexp"
	"slices"
	"strings"

	"github.com/souls-guild/soul-stack/shared/cel"
	"github.com/souls-guild/soul-stack/shared/config"
)

// ParseStack parses the contents of a `vars/_stack.yaml`: its shape
// ([config.ParseServiceVarsStack]) and the one rule the shape cannot state — a
// selector (`when:`, `foreach:`, `file:`) reads no `vars.*` (ADR-0082 amendment
// 2026-10-15, point 14). A step chooses files, never values: a value read while
// the steps are still choosing is read before the final merge, so
// `when: "vars.tls"` would leave a TLS layer out while the merged result says
// `tls: true`.
//
// keeper and soul-lint both parse through this, so the offline answer and the
// runtime one cannot disagree. Every error wraps [config.ErrStackStepInvalid].
func ParseStack(data []byte) (*config.ServiceVarsStack, error) {
	engine, err := serviceVarsEngine()
	if err != nil {
		return nil, fmt.Errorf("servicevars: cel engine: %w", err)
	}
	return parseStack(engine, data)
}

func parseStack(engine *cel.Engine, data []byte) (*config.ServiceVarsStack, error) {
	doc, err := config.ParseServiceVarsStack(data)
	if err != nil {
		return nil, err
	}
	for i, step := range doc.Stack {
		if err := selectorsReadNoVars(engine, step); err != nil {
			return nil, fmt.Errorf("%s step %d: %w", stackFile, i+1, err)
		}
	}
	return doc, nil
}

// unparsed is a root no CEL identifier can spell. PredicateReads and
// InterpolationReads report EVERY root read whole when the text does not parse,
// so this one being read is how a syntax error is told from a read of `vars`. An
// unparsable selector is left to the evaluation, which names the syntax error.
const unparsed = "\x00"

// dottedVars is `vars` spelled with a leading dot, which CEL resolves to the same
// root. The parser keeps the dot in the identifier, so a check asking about `vars`
// alone reads `has(.vars.tls)` as no read at all. The selector engine would refuse
// it at compile too, but only this check names the step and the read.
const dottedVars = ".vars"

var selectorRoots = []string{"vars", dottedVars, unparsed}

func selectorsReadNoVars(engine *cel.Engine, step config.ServiceVarsStackStep) error {
	for _, sel := range []struct {
		key, text string
		bare      bool
	}{
		{"when", step.When, true},
		{"foreach", step.Foreach, false},
		{"file", step.File, false},
	} {
		if sel.text == "" {
			continue
		}
		var reads map[string]cel.RootReads
		if sel.bare {
			reads = engine.PredicateReads(sel.text, selectorRoots)
		} else {
			reads = engine.InterpolationReads(sel.text, selectorRoots)
		}
		if reads[unparsed].Whole {
			continue
		}
		r := reads["vars"]
		dotted := reads[dottedVars]
		r.Whole = r.Whole || dotted.Whole
		for field := range dotted.Fields {
			r.Fields[field] = true
		}
		if r.Whole || len(r.Fields) > 0 {
			return fmt.Errorf("%w: %s: reads %s — a selector sees incarnation.* and the foreach binding only; "+
				"a value read before the final merge is early binding, so derive it in a file under vars/",
				config.ErrStackStepInvalid, sel.key, varsReadText(r))
		}
	}
	return nil
}

func varsReadText(r cel.RootReads) string {
	if r.Whole {
		return "vars"
	}
	names := make([]string, 0, len(r.Fields))
	for field := range r.Fields {
		names = append(names, "vars."+field)
	}
	slices.Sort(names)
	return strings.Join(names, ", ")
}

// readsIncarnation reports whether a `foreach:` or `file:` selector may read
// `incarnation.*`: one that does not renders the same for every incarnation. Text
// that does not parse is taken to read it.
func (a *assembly) readsIncarnation(text string) bool { return a.readsRoot(text, "incarnation") }

func (a *assembly) readsRoot(text, root string) bool {
	for _, r := range a.selector.InterpolationReads(text, []string{root, "." + root, unparsed}) {
		if r.Whole || len(r.Fields) > 0 {
			return true
		}
	}
	return false
}

// perIncarnationPath reports whether step's `file:` names a path that depends on
// the incarnation: it is composed, and reads `incarnation.*` itself or through the
// binding of a `foreach:` that reads it. A literal one names one file for every
// incarnation.
func (a *assembly) perIncarnationPath(step config.ServiceVarsStackStep) bool {
	if _, literal := stepLiteral(step.File); literal {
		return false
	}
	if a.readsIncarnation(step.File) {
		return true
	}
	return step.Foreach != "" && a.readsIncarnation(step.Foreach) && a.readsRoot(step.File, step.As)
}

// StepFileMissingError is a file a `_stack.yaml` step names and the snapshot lacks,
// the step not `optional:`. Step counts from 1, as the step errors do; File is
// relative to `vars/`.
type StepFileMissingError struct {
	Step int
	File string
}

func (e *StepFileMissingError) Error() string {
	return fmt.Sprintf("file %q not found (add optional: true to tolerate it)", e.File)
}

// readStack reads `vars/_stack.yaml`. Missing file → (nil, nil): the caller falls
// back to lexical order over the directory.
func (a *assembly) readStack() (*config.ServiceVarsStack, error) {
	data, err := fs.ReadFile(a.fsys, path.Join(varsDir, stackFile))
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("servicevars: read %s: %w", stackFile, err)
	}
	doc, err := parseStack(a.engine, data)
	if err != nil {
		return nil, fmt.Errorf("servicevars: %w", err)
	}
	return doc, nil
}

// stackLayers runs the declarative pipeline: one layer per selected step and
// `foreach:` binding, in step order.
//
// A selector is evaluated over `incarnation.*` and its binding, and over nothing
// else: no `vars` (refused at parse, [ParseStack], and undeclared in the selector
// engine) and no `soulprint.self` — the service view is built once per run and
// read by every host, so a step keyed on one host's facts would hand that host's
// answer to the whole roster. The binding
// stays with the selectors: a layer's Base carries no Loop, because its
// definitions are evaluated after the merge, on request, where no step is in
// scope.
func (a *assembly) stackLayers(doc *config.ServiceVarsStack) ([]cel.VarLayer, error) {
	var layers []cel.VarLayer
	for i, step := range doc.Stack {
		stepErr := func(err error) error {
			return fmt.Errorf("servicevars: %s step %d: %w", stackFile, i+1, err)
		}
		binds, err := a.stepBindings(step)
		if err != nil {
			return nil, stepErr(err)
		}
		for _, bind := range binds {
			sel := a.base
			sel.Loop = bind
			if step.When != "" {
				on, err := evalBool(a.selector, step.When, sel)
				if err != nil {
					return nil, stepErr(fmt.Errorf("when: %w", err))
				}
				if !on {
					continue
				}
			}
			rel, err := a.stepFile(step, sel)
			if err != nil {
				return nil, stepErr(err)
			}
			full := path.Join(varsDir, rel)
			raw, found, err := a.readLayer(full)
			if err != nil {
				return nil, stepErr(err)
			}
			if !found {
				if step.Optional || (a.stub && a.perIncarnationPath(step)) {
					continue
				}
				// Absence is the interesting case here: unlike the lexical scan,
				// where the listing named the file, this path is composed from an
				// expression and a typo in it has no other symptom.
				return nil, stepErr(&StepFileMissingError{Step: i + 1, File: rel})
			}
			layer, err := a.layer(fmt.Sprintf("%s/%s step %d: %s", varsDir, stackFile, i+1, full), raw, step.Strategy)
			if err != nil {
				return nil, stepErr(err)
			}
			layers = append(layers, layer)
		}
	}
	return layers, nil
}

// stepBindings expands `foreach:` into one binding per element. A step without
// `foreach:` runs exactly once, with no binding. An empty list runs it zero times
// — the same outcome as a false `when:`, and not an error.
func (a *assembly) stepBindings(step config.ServiceVarsStackStep) ([]map[string]any, error) {
	if step.Foreach == "" {
		return []map[string]any{nil}, nil
	}
	raw, err := a.selector.EvalInterpolation(step.Foreach, a.base)
	if err != nil {
		return nil, fmt.Errorf("foreach: %w", err)
	}
	items, err := toList(raw)
	if err != nil {
		return nil, fmt.Errorf("%w: foreach: %v", config.ErrStackStepInvalid, err)
	}
	out := make([]map[string]any, 0, len(items))
	for _, item := range items {
		out = append(out, map[string]any{step.As: item})
	}
	return out, nil
}

// stepFile renders `file:`, relative to `vars/`.
func (a *assembly) stepFile(step config.ServiceVarsStackStep, sel cel.Vars) (string, error) {
	rendered, err := a.selector.EvalInterpolation(step.File, sel)
	if err != nil {
		return "", fmt.Errorf("file: %w", err)
	}
	rel, ok := rendered.(string)
	if !ok {
		return "", fmt.Errorf("%w: file: must render to a string, got %T", config.ErrStackStepInvalid, rendered)
	}
	if rel == "" {
		return "", fmt.Errorf("%w: file: rendered empty", config.ErrStackStepInvalid)
	}
	// `file:` is documented relative to `vars/`, and a composed path that climbs
	// out of it stays inside the SNAPSHOT — the caller's FS contains a path at the
	// service root, not at vars/. So `file: "../service.yml"` would succeed and
	// merge the manifest into the vars namespace. Refuse it: the step said vars/,
	// and reading something else under that name is a scope surprise nothing
	// would report.
	if leavesVarsDir(rel) {
		return "", fmt.Errorf("%w: file: %q leaves the vars/ directory", config.ErrStackStepInvalid, rel)
	}
	return rel, nil
}

func leavesVarsDir(rel string) bool {
	cleaned := path.Clean(rel)
	return cleaned == ".." || strings.HasPrefix(cleaned, "../") || path.IsAbs(cleaned)
}

// blockMark stands for a `${ … }` block in a step's skeleton: no file name holds it.
const blockMark = "\x00"

// stepSkeleton is a step's `file:` as [path.Clean] leaves it, as the Keeper's
// path.Join leaves the rendered path, with each block as [blockMark]. ok is false
// when a block never closes.
func stepSkeleton(file string) (skeleton string, blocks int, ok bool) {
	segs, ok := SplitInterpolation(file)
	if !ok {
		return "", 0, false
	}
	var b strings.Builder
	for _, s := range segs {
		if s.Expr {
			b.WriteString(blockMark)
			blocks++
			continue
		}
		b.WriteString(s.Text)
	}
	return path.Clean(b.String()), blocks, true
}

// stepLiteral is the one path, relative to `vars/`, a `file:` without `${ … }` names.
func stepLiteral(file string) (string, bool) {
	skeleton, blocks, ok := stepSkeleton(file)
	return skeleton, ok && blocks == 0
}

// StepFileMatcher reports which paths, relative to `vars/`, a `_stack.yaml` step's
// `file:` can name: a literal one its cleaned path, a composed one any path its
// cleaned skeleton matches with each `${ … }` block standing for text inside ONE
// path segment — which value a block renders is per incarnation, and a block that
// crossed a directory would claim every nested file for a step meant for one level.
func StepFileMatcher(file string) func(rel string) bool {
	skeleton, blocks, ok := stepSkeleton(file)
	switch {
	case !ok:
		return func(string) bool { return false }
	case blocks == 0:
		return func(rel string) bool { return path.Clean(rel) == skeleton }
	}
	parts := strings.Split(skeleton, blockMark)
	for i, p := range parts {
		parts[i] = regexp.QuoteMeta(p)
	}
	re := regexp.MustCompile("^" + strings.Join(parts, "[^/]*") + "$")
	return re.MatchString
}

// evalBool evaluates a bare CEL predicate. A non-bool result is an error, not a
// truthiness guess.
func evalBool(engine *cel.Engine, expr string, vars cel.Vars) (bool, error) {
	val, err := engine.EvalExpression(expr, vars)
	if err != nil {
		return false, err
	}
	b, ok := val.Value().(bool)
	if !ok {
		return false, fmt.Errorf("%w: when: must be a bool, got %T", config.ErrStackStepInvalid, val.Value())
	}
	return b, nil
}

// toList normalizes a CEL list result to []any. A string is REFUSED rather than
// treated as a one-element list: `foreach: "${ incarnation.id }"` is a mistake
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
