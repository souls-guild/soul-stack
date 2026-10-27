package render

import (
	"fmt"
	"slices"
	"strings"

	"github.com/souls-guild/soul-stack/keeper/internal/topology"
	"github.com/souls-guild/soul-stack/shared/config"
)

// Rescue units in render (ADR-0091 amendment NIM-993, rule 1): each task carries the
// key of the outermost unit holding it, and a linear run learns where each scenario
// unit can reach.

// unitScope is what a pass keys its tasks' rescue units by.
type unitScope struct {
	// units — the units of the plan the pass renders: the scenario's, or in a destiny
	// pass the destiny's own.
	units []config.RescueUnit
	// prefix — "<applier position>@" in a destiny pass.
	prefix string
	// outer — a destiny pass whose applier sits in a scenario unit: that unit's key.
	// Applied from a unit, every task of the destiny belongs to it (§6.5 unions the
	// applier's `onfail:` into each), the destiny's own units included.
	outer string
}

// keyAt is the key of the outermost unit holding the task at plan position path.
func (s unitScope) keyAt(path string) string {
	if s.outer != "" {
		return s.outer
	}
	// Pre-order: an enclosing construct's unit comes before every unit inside it.
	for _, u := range s.units {
		if u.Holds(path) {
			return s.prefix + u.Path
		}
	}
	return ""
}

// scenarioUnits is the scope of a scenario pass.
func scenarioUnits(tasks []config.Task) unitScope {
	return unitScope{units: config.RescueUnits(tasks)}
}

// destinyUnits is the scope of the destiny pass of the applier at parent.path.
func destinyUnits(parent RenderInput, tasks []config.Task) unitScope {
	return unitScope{
		units:  config.RescueUnits(tasks),
		prefix: parent.path + "@",
		outer:  parent.units.keyAt(parent.path),
	}
}

// stampRescueUnit keys every RenderedTask added for the task at plan position path
// (tasks[from:]). Called where a task's own output is appended, never over a range
// holding an applier's terminal: [Pipeline.renderApplyDestiny] stamps its own.
func stampRescueUnit(in RenderInput, tasks []*RenderedTask, from int, path string) {
	key := in.units.keyAt(path)
	if key == "" {
		return
	}
	for _, rt := range tasks[from:] {
		rt.RescueUnit = key
	}
}

// RescueCover is where one rescue unit of a linear scenario can reach, its `where:`
// and `run_once:` aside.
type RescueCover struct {
	// Path — the unit's construct ([config.StepPath]).
	Path string
	// Sources — the registers it rescues, in the order its `onfail:` names them.
	Sources []string
	// First, Last — its steps ([config.StepSpan]).
	First, Last int
	// RescueUnit — the key of the outermost unit holding it, which decides its
	// sources (rule 1) and whose key its tasks carry ([RenderedTask.RescueUnit]): Path
	// itself unless it sits inside a unit of another source.
	RescueUnit string
	// DecidedAt — the last step of the RescueUnit unit, at whose barrier it is decided.
	DecidedAt int
	// Covens — the labels a host must carry ([HostCovered]): the unit's own `on:` and
	// every enclosing construct's, ANDed.
	Covens []string
}

// HostCovered reports whether h carries every label of covens.
func HostCovered(h *topology.HostFacts, covens []string) bool {
	return hostHasAllCovens(h.Coven, covens)
}

// RescueCovers is every rescue unit of in's scenario as [RescueCover], in plan order.
// A unit inside an include group the pass drops is no unit of the run. nil without
// Steps: staged dispatch has no step to reckon a unit by.
func (p *Pipeline) RescueCovers(in RenderInput) ([]RescueCover, error) {
	if in.Scenario == nil || in.Steps == nil {
		return nil, nil
	}
	if len(in.Steps) != len(in.Scenario.Tasks) {
		return nil, fmt.Errorf("render: the step shape has %d nodes for %d tasks - it was built from another plan", len(in.Steps), len(in.Scenario.Tasks))
	}
	units := config.RescueUnits(in.Scenario.Tasks)
	if len(units) == 0 {
		return nil, nil
	}
	sources := map[string][]string{}
	// Every unit's position and each position above it: the subtrees the walk enters.
	toward := map[string]bool{}
	for _, u := range units {
		sources[u.Path] = append(sources[u.Path], u.Source)
		for path := u.Path; path != ""; path = parentPath(path) {
			toward[path] = true
		}
	}
	w := coverWalk{p: p, in: in, units: unitScope{units: units}, sources: sources, toward: toward, includes: includeGroupCache{}}
	if err := w.walk(in.Scenario.Tasks, "", nil); err != nil {
		return nil, err
	}
	return w.out, nil
}

type coverWalk struct {
	p        *Pipeline
	in       RenderInput
	units    unitScope
	sources  map[string][]string
	toward   map[string]bool
	includes includeGroupCache
	out      []RescueCover
}

func (w *coverWalk) walk(tasks []config.Task, parent string, covens []string) error {
	for i := range tasks {
		task := tasks[i]
		path := config.StepPath(parent, i)
		if !w.toward[path] {
			continue
		}
		keep, err := w.p.keepIncludeGroup(w.in, task, w.includes)
		if err != nil {
			return err
		}
		if !keep {
			continue
		}
		own, err := resolveOn(w.p.cel, w.in, task.On)
		if err != nil {
			return fmt.Errorf("render: task %q: %w", task.Name, err)
		}
		chain := slices.Clone(covens)
		for _, c := range own {
			if !slices.Contains(chain, c) {
				chain = append(chain, c)
			}
		}
		if srcs, ok := w.sources[path]; ok {
			first, last, ok := config.StepSpan(w.in.Steps, path)
			key := w.units.keyAt(path)
			_, decidedAt, keyOK := config.StepSpan(w.in.Steps, key)
			if !ok || !keyOK {
				return fmt.Errorf("render: rescue unit %q at %s is outside the step shape", task.Name, path)
			}
			w.out = append(w.out, RescueCover{
				Path: path, Sources: srcs, First: first, Last: last,
				RescueUnit: key, DecidedAt: decidedAt, Covens: chain,
			})
		}
		if task.Block != nil {
			if err := w.walk(task.Block.Block, path, chain); err != nil {
				return err
			}
		}
	}
	return nil
}

// parentPath is the position of the construct enclosing path, "" at the top level.
func parentPath(path string) string {
	if i := strings.LastIndexByte(path, '/'); i >= 0 {
		return path[:i]
	}
	return ""
}
