package render

import (
	"maps"
	"slices"

	"github.com/souls-guild/soul-stack/keeper/internal/topology"
	"github.com/souls-guild/soul-stack/shared/cel"
	"github.com/souls-guild/soul-stack/shared/config"
)

// The views of `vars.*` a render builds ([ADR-0082] amendment 2026-10-15, points 5
// to 8). A view is lazy: a definition is evaluated when a cell reads its name, once
// per view, and every definition reads `vars.<other>` through the view of the cell
// that asked — so a service var derived from a name a task overrides follows the
// override.
//
//	scenario pass:  service view (RenderInput.ServiceVars, one per run)
//	                  └ scenario `vars:` rung + block/task rung   per task, host, iteration
//	destiny pass:   vars.yml view (RenderInput.destinyVars, one per host)
//	                  └ block/task rung                          per task, host, iteration
//
// The scenario rung is a rung of each task view and never of a shared parent: a view
// stores a value in its parent's memo when the value reads nothing the child
// defines, and a scenario definition is evaluated with the READER's run-level
// values — its `incarnation.host_count` differs between a host task and a keeper
// task.
//
// [ADR-0082]: ../../../docs/adr/0082-service-vars.md

// ErrVarCycle — the stable marker of a var→var cycle ([cel.VarCycleError]); an L0
// case's expect_render_error matches its text, `var_cycle`.
var ErrVarCycle = cel.ErrVarCycle

// Origins name a rung in a [cel.VarError].
const (
	scenarioRungOrigin = "scenario vars:"
	taskRungOrigin     = "task vars:"
	destinyRungOrigin  = "vars.yml"
)

// taskView is the view a task's cells read on one host: its params:, where:,
// apply: input:, flow_context and render_context values, and its own `vars:`.
// reader is the context of the cell — host, loop iteration, scopes; its Vars and
// View are not read. With no rung of its own a task reads the view below unchanged
// and shares its memo.
func (p *Pipeline) taskView(in RenderInput, task config.Task, host *topology.HostFacts, reader cel.Vars) (*cel.VarView, error) {
	if in.destinyIsolated {
		base := in.destinyBase(host)
		if len(task.Vars) == 0 {
			return base, nil
		}
		return p.newView(base, p.taskRung(task, reader)), nil
	}
	layers, err := p.scenarioRung(in, reader)
	if err != nil {
		return nil, err
	}
	if len(task.Vars) > 0 {
		layers = append(layers, p.taskRung(task, reader))
	}
	if len(layers) == 0 {
		return in.ServiceVars, nil
	}
	return p.newView(in.ServiceVars, layers...), nil
}

// newView is [cel.NewVarView], reported to onView.
func (p *Pipeline) newView(parent *cel.VarView, layers ...cel.VarLayer) *cel.VarView {
	view := cel.NewVarView(parent, layers...)
	if p.onView != nil {
		p.onView(view)
	}
	return view
}

// materialize evaluates every name of view: strict mode's check (point 12b), run once
// the context's own render has succeeded, so a case that fails in production fails
// with the same error. Through [cel.VarView.Check], which records no trial coverage
// and fills no memo: what production never evaluates is no branch of the scenario.
// nil is a context with no `vars.*`.
func materialize(view *cel.VarView) error {
	if view == nil {
		return nil
	}
	return view.Check()
}

// withTaskView is reader reading `vars` through the task's view.
func (p *Pipeline) withTaskView(in RenderInput, task config.Task, host *topology.HostFacts, reader cel.Vars) (cel.Vars, error) {
	view, err := p.taskView(in, task, host, reader)
	reader.View = view
	return reader, err
}

// runLevelView is the scenario pass's view without a task rung — the service view
// under the scenario `vars:` rung — for the cells that belong to no task's own
// `vars:`: the loop axis, an include `when:`, a `template:` path (point 5). The
// scenario definitions are evaluated with reader's values, its compute scope
// included, so one reading `compute.*` is refused on the loop axis and read
// elsewhere (point 6). nil in a destiny pass: none of those cells reads `vars.yml`
// but an include `when:`, which takes [RenderInput.destinyBase].
func (p *Pipeline) runLevelView(in RenderInput, reader cel.Vars) (*cel.VarView, error) {
	if in.destinyIsolated {
		return nil, nil
	}
	layers, err := p.scenarioRung(in, reader)
	if err != nil {
		return nil, err
	}
	if len(layers) == 0 {
		return in.ServiceVars, nil
	}
	return p.newView(in.ServiceVars, layers...), nil
}

// runLevelVarLayers is the raw stack of [Pipeline.runLevelView]: what an include
// `when:` reads of `vars` in a scenario pass. nil when the view cannot be built or
// is empty.
func (p *Pipeline) runLevelVarLayers(in RenderInput) []cel.VarLayer {
	view, err := p.runLevelView(in, cel.Vars{})
	if err != nil || view == nil {
		return nil
	}
	return view.Layers()
}

// viewClosure is the set of a view's names whose definitions read what hit accepts,
// directly or through a name already in the set (ADR-0082 amendment 2026-10-15,
// point 16): a read of `vars.<x>` counts as a read of everything the definitions of x
// read. Conservative: every layer's definition of a name counts, the winning one or
// not, and every string of it at any depth, each parsed with its own layer's engine
// (fallback for a layer without one). Raw text only; nothing is evaluated.
//
// roots must include `vars`, which hit reads against closed. A fixpoint, because a
// view has no declaration order: `b: ${ vars.a }` may be visited before `a`.
func viewClosure(fallback *cel.Engine, layers []cel.VarLayer, roots []string, hit func(reads map[string]cel.RootReads, closed map[string]bool) bool) map[string]bool {
	type definition struct {
		name  string
		reads map[string]cel.RootReads
	}
	var defs []definition
	for _, l := range layers {
		engine := l.Engine
		if engine == nil {
			engine = fallback
		}
		for name, raw := range l.Defs {
			defs = append(defs, definition{name: name, reads: rawReads(engine, raw, roots)})
		}
	}
	closed := map[string]bool{}
	for grew := true; grew; {
		grew = false
		for _, d := range defs {
			if !closed[d.name] && hit(d.reads, closed) {
				closed[d.name], grew = true, true
			}
		}
	}
	return closed
}

// rawReads is what the strings of a raw value, at any depth, read off roots: the
// union of [cel.Engine.InterpolationReads] over every one of them.
func rawReads(engine *cel.Engine, v any, roots []string) map[string]cel.RootReads {
	out := make(map[string]cel.RootReads, len(roots))
	for _, root := range roots {
		out[root] = cel.RootReads{Fields: map[string]bool{}}
	}
	var walk func(v any)
	walk = func(v any) {
		switch t := v.(type) {
		case string:
			for root, r := range engine.InterpolationReads(t, roots) {
				u := out[root]
				u.Whole = u.Whole || r.Whole
				for field := range r.Fields {
					u.Fields[field] = true
				}
				out[root] = u
			}
		case []any:
			for _, el := range t {
				walk(el)
			}
		case map[string]any:
			for _, el := range t {
				walk(el)
			}
		}
	}
	walk(v)
	return out
}

// scenarioRung is the scenario's top-level `vars:` as a layer of a view read with
// reader's run-level values (point 6), or none for a scenario without `vars:`.
func (p *Pipeline) scenarioRung(in RenderInput, reader cel.Vars) ([]cel.VarLayer, error) {
	if in.Scenario == nil || len(in.Scenario.Vars) == 0 {
		return nil, nil
	}
	if p.scenarioVarsErr != nil {
		return nil, p.scenarioVarsErr
	}
	return append(make([]cel.VarLayer, 0, 2), cel.VarLayer{
		Rung:   cel.RungScenario,
		Origin: scenarioRungOrigin,
		Defs:   in.Scenario.Vars,
		Engine: p.scenarioVars,
		Base:   scenarioRungBase(reader),
	}), nil
}

// scenarioRungBase is what a scenario `vars:` definition is evaluated with: the
// reader's run data, never its loop binding, and the reader's scopes. Its engine
// declares no `soulprint` and no `register`, so it needs neither.
func scenarioRungBase(reader cel.Vars) cel.Vars {
	return cel.Vars{
		Input:       reader.Input,
		Incarnation: reader.Incarnation,
		Compute:     reader.Compute,
		Ctx:         reader.Ctx,
		// A scenario rung is stacked per task view of one pass, so the reader's window
		// is the pass's; the service rung, kept across passes, has no function at all.
		InMaintenanceWindow: reader.InMaintenanceWindow,
		ComputeScope:        reader.ComputeScope,
		HostScope:           reader.HostScope,
		HostVariantInputs:   reader.HostVariantInputs,
		HostVariantVars:     reader.HostVariantVars,
	}
}

// taskRung is a block's and the task's own `vars:` — already merged into task.Vars
// by mergeBlockInheritance, shallow — as ONE layer: two layers would deep-merge a
// map the block and the task both define. Evaluated in the reader's context, loop
// binding and host scope included.
func (p *Pipeline) taskRung(task config.Task, reader cel.Vars) cel.VarLayer {
	return cel.VarLayer{Rung: cel.RungTask, Origin: taskRungOrigin, Defs: task.Vars, Engine: p.cel, Base: reader}
}

// destinyBaseViews builds the destiny pass's per-host `vars.yml` views, one per
// targeted host plus the synthetic empty host "" a task whose where: filtered out
// everyone renders on. A definition is evaluated in the destiny env over its own
// host's values (hostVars of the isolated pass) — never the caller's scope, and
// never a loop binding. nil for a destiny without `vars.yml`.
func (p *Pipeline) destinyBaseViews(destinyIn RenderInput, defs map[string]any, targeted []*topology.HostFacts) map[string]*cel.VarView {
	if len(defs) == 0 {
		return nil
	}
	views := make(map[string]*cel.VarView, len(targeted)+1)
	add := func(host *topology.HostFacts) {
		views[host.SID] = p.newView(nil, cel.VarLayer{
			Rung:   cel.RungDestinyFile,
			Origin: destinyRungOrigin,
			Defs:   defs,
			Engine: p.cel,
			Base:   hostVars(destinyIn, host, len(targeted)),
		})
	}
	for _, host := range targeted {
		add(host)
	}
	if _, ok := views[""]; !ok {
		add(&topology.HostFacts{})
	}
	return views
}

// destinyBase is host's `vars.yml` view in a destiny pass, the synthetic empty
// host's for no host; nil on a scenario pass, for a destiny without `vars.yml`, and
// for a host the pass did not target — another host's view would answer for it with
// that host's values, the NIM-908 substitution.
func (in RenderInput) destinyBase(host *topology.HostFacts) *cel.VarView {
	var sid string
	if host != nil {
		sid = host.SID
	}
	return in.destinyVars[sid]
}

// lookupVars evaluates names in view, in the order given; an absent name is left
// out. The values are the view's memo and must not be mutated.
func lookupVars(view *cel.VarView, names []string) (map[string]any, error) {
	out := make(map[string]any, len(names))
	if view == nil {
		return out, nil
	}
	for _, name := range names {
		val, found, err := view.Lookup(name)
		if err != nil {
			return nil, err
		}
		if found {
			out[name] = val
		}
	}
	return out, nil
}

// renderContextVarNames is the set of `vars.*` names render_context carries for a
// template (point 17): the names of the service, scenario and `vars.yml` rungs the
// template spells as `.vars.<name>`, and on a whole read of `.vars` every name of
// the service and scenario rungs too — never a `vars.yml` name it does not spell,
// whose plumbing would cross the wire to the host for nothing. A task-rung name
// reaches the template through params.vars. Sorted.
func renderContextVarNames(view *cel.VarView, keys map[string]bool, whole bool) []string {
	if view == nil {
		return nil
	}
	set := map[string]struct{}{}
	for _, l := range view.Layers() {
		if l.Rung == cel.RungTask {
			continue
		}
		wide := whole && l.Rung != cel.RungDestinyFile
		for name := range l.Defs {
			if wide || keys[name] {
				set[name] = struct{}{}
			}
		}
	}
	return slices.Sorted(maps.Keys(set))
}
