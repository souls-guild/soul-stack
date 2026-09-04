package validate

// Offline check of the retired CEL root `incarnation.name` ([ADR-0085], NIM-730).
//
// The identifier of a registry entity is spelled `id` since NIM-729, and the CEL
// root followed. `incarnation.name` is read for a compatibility window — the
// engine puts both keys in the activation (shared/cel.Vars.incarnationRoot), so
// a scenario on the old spelling runs unchanged — and this rule is the only
// thing that says so. It is a WARNING and not an error on purpose: an error here
// would close the window it exists to keep open.
//
// It is load-bearing rather than a courtesy. `incarnation` is `cel.DynType`, so
// the drop at the end of the window is a `no such key` at EVALUATION, not a
// compile error — and one of the three environments is flow-control, evaluated
// on the HOST, so a stale `when: incarnation.name == …` fails mid-run after
// earlier tasks have applied ([ADR-0085] §"The stale CEL root is a runtime
// failure, not a compile error"). Nothing else on either side of the wire
// catches it statically.
//
// The walk deliberately mirrors computeChecker (compute_scope.go): the same task
// recursion, the same split between an interpolated cell and a whole-string
// expression key, and the same AST-based question asked of shared/cel rather
// than a regex — `incarnation.name` in prose and inside a CEL string literal are
// not references, and a regex would disagree with the engine in exactly those
// places.
//
// Each finding carries a LINE, resolved from the scenario Document: an address
// is what makes the warning actionable across a service repository with dozens
// of scenarios, and the YAMLPath alone is not one an author can jump to. When
// the exact cell's path does not resolve, the nearest enclosing path that does
// is used — an address one level out beats no address.
//
// `include:` bodies ARE covered, unlike the compute rules next door. That is not
// symmetry for its own sake: the keeper renders the EXPANDED task list, and in a
// service written the way services actually are — a thin `main.yml` over bodies in
// `_shared/` — the bodies hold most of the `incarnation.*` references there are. A
// catcher that stopped at `main.yml` would be silent on the majority of the sites
// it exists for. Each body is walked as its own file, so a finding cites that
// file's own path and its own line rather than an index into a list that only
// exists after expansion.
//
// NOT covered: the isolated destiny pass (a destiny's own file never reaches this
// entry point). The service-vars surface — `vars/_stack.yaml`, the third CEL
// environment carrying this root — is covered separately, from the SERVICE entry
// point, in service_vars.go: it is not part of a scenario and this function never
// sees it.
//
// [ADR-0085]: ../../../docs/adr/0085-entity-id-and-label.md

import (
	"fmt"
	"sort"
	"strings"

	"github.com/souls-guild/soul-stack/shared/cel"
	"github.com/souls-guild/soul-stack/shared/config"
	"github.com/souls-guild/soul-stack/shared/diag"
)

// legacyRootDiagnostics walks a parsed scenario — its own file and every
// `include:` body the engine would reach — for reads of the retired root.
//
// A nil engine (construction failed) or a nil manifest disables the rule rather
// than failing the lint: this is an extra check over a scenario the other rules
// have already read. doc may be nil; findings from the scenario's own file then
// carry no line.
func legacyRootDiagnostics(scenarioPath string, scn *config.ScenarioManifest, doc *config.Document) []diag.Diagnostic {
	eng := computeScopeEngine()
	if eng == nil || scn == nil {
		return nil
	}
	c := &legacyRootChecker{eng: eng, path: scenarioPath, position: documentPosition(doc)}
	c.computeBlock(scn.Compute)
	c.tasks(scn.Tasks, "$.tasks")
	out := c.out
	for _, body := range includedTaskBodies(scenarioPath, scn) {
		out = append(out, legacyRootInIncludedBody(eng, body)...)
	}
	return out
}

// legacyRootInIncludedBody walks ONE included file, as its own document. The body
// is a bare task list, so its paths start `$[i]` — a task's address inside the
// file it is written in, not its index in the expanded run list, which is what an
// author needs to open.
func legacyRootInIncludedBody(eng *cel.Engine, body includedBody) []diag.Diagnostic {
	// The body's own DIAGNOSTICS are discarded, and deliberately not used as a
	// gate. Loaded on its own it is missing the context the expander gives it —
	// [config.ValidateOptions.OuterRegisters], the registers its INCLUDER declares
	// — so a body that legally reads `register.<name>` from the file that includes
	// it comes back carrying error-level `unknown_register_reference`. Skipping on
	// that would drop the whole body from this walk and put back the exact false
	// green this rule exists to close, on the shape services are actually written
	// in. The expander raises those same diagnostics WITH the right context, so
	// nothing here is lost by ignoring them.
	//
	// Only a body that yielded no tasks at all is skipped: it did not parse, the
	// parse error is reported by whoever loaded it with a position, and there is
	// nothing to walk.
	tasks, _, err := config.LoadDestinyTasksFromBytes(body.path, body.data, config.ValidateOptions{})
	if err != nil || len(tasks) == 0 {
		return nil
	}
	c := &legacyRootChecker{
		eng:  eng,
		path: body.path,
		position: func(yamlPath string) (int, int, bool) {
			return config.PositionIn(body.data, yamlPath)
		},
	}
	c.tasks(tasks, "$")
	return c.out
}

// legacyRootChecker carries the walk's state. position resolves a YAML path to a
// place in whichever file this walk is over; nil means findings carry no address.
type legacyRootChecker struct {
	eng      *cel.Engine
	path     string
	position func(yamlPath string) (line, column int, ok bool)
	out      []diag.Diagnostic
}

// documentPosition adapts a loaded [config.Document] to the checker's resolver.
// A nil document yields a nil resolver rather than one that always fails, so
// "no address available" is one state and not two.
func documentPosition(doc *config.Document) func(string) (int, int, bool) {
	if doc == nil {
		return nil
	}
	return doc.PositionOf
}

// tasks recurses over the task list, including block: children. Every cell the
// keeper renders and every predicate the Soul evaluates is visited: the root is
// declared in all three CEL environments, so unlike `compute` there is no
// context here where the reference would be out of scope rather than retired.
func (c *legacyRootChecker) tasks(tasks []config.Task, prefix string) {
	for i := range tasks {
		t := &tasks[i]
		where := fmt.Sprintf("%s[%d]", prefix, i)

		if elems, ok := onListElements(t.On); ok {
			for j, s := range elems {
				c.interpolation(fmt.Sprintf("%s.on[%d]", where, j), s)
			}
		}

		if t.Loop != nil {
			c.value(t.Loop.Items, where+".loop.items")
			c.expression(where+".loop.when", t.Loop.When)
		}

		c.expression(where+".when", t.When)
		c.expression(where+".changed_when", t.ChangedWhen)
		c.expression(where+".failed_when", t.FailedWhen)
		if t.Retry != nil {
			c.expression(where+".retry.until", t.Retry.Until)
		}

		c.expression(where+".where", t.Where)
		if t.Assert != nil {
			for j, that := range t.Assert.That {
				c.expression(fmt.Sprintf("%s.assert.that[%d]", where, j), that)
			}
		}
		c.value(t.Vars, where+".vars")
		if t.Module != nil {
			c.value(t.Module.Params, where+".params")
		}
		if t.Apply != nil {
			c.value(t.Apply.Input, where+".apply.input")
		}

		if t.Block != nil {
			c.tasks(t.Block.Block, where+".block")
		}
	}
}

// computeBlock checks the scenario's `compute:` entries. They resolve in the
// run-level context, which carries the incarnation like every other.
func (c *legacyRootChecker) computeBlock(block config.ComputeBlock) {
	for _, cv := range block {
		s, ok := cv.Value.(string)
		if !ok {
			continue // a literal passes through unrendered
		}
		c.interpolation("$.compute."+cv.Name, s)
	}
}

// value walks a decoded YAML value (params:/vars:/apply.input:/loop.items:) and
// tests every string it contains. Map keys are visited in sorted order so a
// scenario always produces its diagnostics in the same order — a map's range
// order would otherwise shuffle the report between runs.
func (c *legacyRootChecker) value(v any, where string) {
	switch val := v.(type) {
	case string:
		c.interpolation(where, val)
	case map[string]any:
		keys := make([]string, 0, len(val))
		for k := range val {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			c.value(val[k], where+"."+k)
		}
	case []any:
		for i, e := range val {
			c.value(e, fmt.Sprintf("%s[%d]", where, i))
		}
	case []string:
		for i, s := range val {
			c.interpolation(fmt.Sprintf("%s[%d]", where, i), s)
		}
	}
}

func (c *legacyRootChecker) interpolation(where, raw string) {
	if raw == "" || !c.eng.InterpolationReadsLegacyIncarnationID(raw) {
		return
	}
	c.out = append(c.out, c.diag(where, raw))
}

func (c *legacyRootChecker) expression(where, expr string) {
	if expr == "" || !c.eng.ExpressionReadsLegacyIncarnationID(expr) {
		return
	}
	c.out = append(c.out, c.diag(where, expr))
}

// diag builds the finding. One per CELL, not per occurrence: an author fixes a
// cell, and `${ incarnation.name }-${ incarnation.name }` is one edit.
func (c *legacyRootChecker) diag(where, raw string) diag.Diagnostic {
	d := diag.Diagnostic{
		Level: diag.LevelWarning,
		Phase: diag.PhaseSemanticValidate,
		File:  c.path,
		Code:  "incarnation_name_legacy_root",
		Message: fmt.Sprintf(
			"%q reads incarnation.name, the pre-ADR-0085 spelling of incarnation.id -- it still evaluates, for a compatibility window, and then stops",
			raw),
		Hint:     "replace incarnation.name with incarnation.id in this expression",
		YAMLPath: where,
	}
	if line, col, ok := c.address(where); ok {
		d.Line, d.Column = line, col
	}
	return d
}

// address resolves the cell's YAMLPath against the source, walking OUT to the
// nearest enclosing path when the exact one does not resolve. A params key
// holding a dot, or any other shape outside the path subset config parses, would
// otherwise cost the finding its address entirely; `$.tasks[3]` is a worse
// address than `$.tasks[3].params.retries` and a far better one than none.
func (c *legacyRootChecker) address(yamlPath string) (line, column int, ok bool) {
	if c.position == nil {
		return 0, 0, false
	}
	for p := yamlPath; p != "$" && p != ""; p = trimLastPathSegment(p) {
		if line, column, ok = c.position(p); ok {
			return line, column, true
		}
	}
	return 0, 0, false
}

// trimLastPathSegment drops the final `.key` or `[i]` of a YAML path, so a
// caller can walk toward the root. `$` (and anything with no segment left)
// returns `$`, which terminates the walk.
func trimLastPathSegment(p string) string {
	dot, bracket := strings.LastIndexByte(p, '.'), strings.LastIndexByte(p, '[')
	cut := dot
	if bracket > cut {
		cut = bracket
	}
	if cut <= 0 {
		return "$"
	}
	return p[:cut]
}

// includedBody — one file an `include:` reaches, as the resolver handed it over.
type includedBody struct {
	path string
	data []byte
}

// includedTaskBodies collects every file the scenario's `include:` chain reaches,
// deduplicated by display path and in a stable order.
//
// It drives [config.ExpandIncludes] purely to make the resolver walk the chain —
// the flattened list it returns is deliberately DISCARDED. That list has no
// positions (expansion erases them, include_expand.go) and its indices address a
// run, not a file; walking each body on its own is what buys the finding a real
// address in the file its author would open. The sibling own-namespace rule uses
// the same resolver and takes the flat list instead, because it reports names
// rather than places.
//
// A body that cannot be resolved contributes nothing here: the expander itself
// raises `include_not_found`/`include_cycle` with the address, and a second
// complaint from this rule would say less about the same file.
func includedTaskBodies(scenarioPath string, scn *config.ScenarioManifest) []includedBody {
	// Both levels, resolved exactly as stageDiagnostics resolves them — including
	// the loose-file case, where the second level is unavailable and the resolver
	// reads the local one alone. A loose `main.yml` beside its own body still has
	// a body to walk, and stopping here would have made this rule quieter than
	// the sibling that expands the same chain.
	serviceDir := scenarioServiceLevelDir(scenarioPath)
	dir := scenarioLocalLevelDir(scenarioPath, serviceDir)
	var root string
	if serviceDir != "" {
		root = scenarioServiceRoot(scenarioPath)
	}
	resolve := scenarioIncludeResolver(root, dir, serviceDir)

	var (
		bodies []includedBody
		seen   = map[string]bool{}
	)
	recording := func(name string) ([]byte, string, error) {
		data, display, err := resolve(name)
		if err == nil && !seen[display] {
			seen[display] = true
			bodies = append(bodies, includedBody{path: display, data: data})
		}
		return data, display, err
	}
	_, _ = config.ExpandIncludes(scn.Tasks, recording)
	return bodies
}
