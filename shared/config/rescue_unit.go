package config

import (
	"fmt"
	"strings"

	"github.com/souls-guild/soul-stack/shared/diag"
)

// Rescue units — ADR-0091 amendment NIM-993 (docs/adr/0091-scenario-linear-barrier.md,
// "a rescue that succeeds handles the failure"), rule 1.

// CodeOnfailNamesRescue — an `onfail:` naming a register set inside a rescue unit
// of the same plan. A unit's failure is not a failure of its own: it means the
// unit's source is not handled, so a rescue of it would give one host two
// interleaved verdicts.
const CodeOnfailNamesRescue = "onfail_names_rescue"

// CodeGroupMultipleRescueUnits — a block rolled as one group holding more than
// one rescue unit for sources outside it. The Keeper strips a satisfied cross-step
// `onfail:` from the wire, so on the Soul such units are ordinary tasks under the
// message's fail-stop: one unit behaves as a unit, a second would run after the
// first succeeded or never get its chance after the first failed.
const CodeGroupMultipleRescueUnits = "group_multiple_rescue_units"

// RescueUnit is one rescue unit of a plan: for Source, the outermost task,
// `block:` or `apply:` whose own written `onfail:` names it, with everything below
// it. A descendant that names Source too belongs to this unit, because requisites
// merge as a union (docs/destiny/tasks.md §6.5).
type RescueUnit struct {
	// Source is the register whose failure the unit rescues.
	Source string
	// Path is the construct's plan position in [StepPath] form.
	Path string
	// Task is the construct.
	Task *Task
	// Registers are the `register:` names set inside the unit, the construct's own
	// included, in plan order. An applier's destiny is another plan, so its tasks'
	// registers are not here.
	Registers []string
}

// Holds reports whether the task at plan position path ([StepPath] form) is
// inside the unit.
func (u RescueUnit) Holds(path string) bool {
	return path == u.Path || strings.HasPrefix(path, u.Path+"/")
}

// RescueUnits returns the rescue units of ONE plan — a scenario's task tree or a
// destiny's, with or without its includes spliced — in plan order: by the
// construct's pre-order position, then in the order its `onfail:` names the
// sources. A source named twice on one construct is one unit.
//
// One plan is enough because a destiny's tasks name only the destiny's registers
// and a scenario's only the scenario's, so every unit of a source is in the plan
// that sets it. An applier is a unit of the plan it is written in; the tasks of
// its destiny are not walked.
//
// A keeper-side task is no rescue (rule 9): the Keeper evaluates no requisite of
// its own tasks, so its `onfail:` makes no unit.
func RescueUnits(tasks []Task) []RescueUnit {
	units, _ := walkRescueUnits(tasks)
	return units
}

// rescueVisit is one task of the plan, at its position, with whether some unit
// holds it.
type rescueVisit struct {
	task   *Task
	path   string
	inUnit bool
}

func walkRescueUnits(tasks []Task) ([]RescueUnit, []rescueVisit) {
	w := &rescueWalk{}
	w.walk(tasks, "", nil, nil)
	return w.units, w.visits
}

type rescueWalk struct {
	units  []RescueUnit
	visits []rescueVisit
}

// walk visits tasks below parent. claimed holds the sources an enclosing construct
// already made a unit of; open indexes w.units for every unit enclosing this level.
func (w *rescueWalk) walk(tasks []Task, parent string, claimed map[string]bool, open []int) {
	for i := range tasks {
		t := &tasks[i]
		path := StepPath(parent, i)
		holding, inner := open, claimed
		if !IsKeeperSideTask(*t) {
			copied := false
			for _, src := range t.OnFail {
				if inner[src] {
					continue
				}
				if !copied {
					inner = make(map[string]bool, len(claimed)+len(t.OnFail))
					for s := range claimed {
						inner[s] = true
					}
					copied = true
				}
				inner[src] = true
				holding = append(holding[:len(holding):len(holding)], len(w.units))
				w.units = append(w.units, RescueUnit{Source: src, Path: path, Task: t})
			}
		}
		if t.Register != "" {
			for _, u := range holding {
				w.units[u].Registers = append(w.units[u].Registers, t.Register)
			}
		}
		w.visits = append(w.visits, rescueVisit{task: t, path: path, inUnit: len(holding) > 0})
		if t.Block != nil {
			w.walk(t.Block.Block, path, inner, holding)
		}
	}
}

// validateFlatRescueUnits raises the two refusals that read a plan's units:
// [CodeOnfailNamesRescue], and `missing_required_field` for a `core.task.failed`
// outside every unit with no `message:` (rule 8). Runs on the FLAT plan, after
// include expansion: a scenario's included body is part of the scenario, and a
// unit may hold an `include:` the body's own file cannot see. A destiny is
// expanded and judged on its own, so applying it from a unit does not put its
// tasks inside one.
//
// No line/column: expansion erased the positions, as for
// [validateFlatTaskAddresses]; the diagnostic names the task.
func validateFlatRescueUnits(tasks []Task) []diag.Diagnostic {
	units, visits := walkRescueUnits(tasks)
	setIn := map[string]RescueUnit{}
	for _, u := range units {
		for _, r := range u.Registers {
			if _, seen := setIn[r]; !seen {
				setIn[r] = u
			}
		}
	}

	var out []diag.Diagnostic
	for _, v := range visits {
		named := map[string]bool{}
		for _, src := range v.task.OnFail {
			u, ok := setIn[src]
			if !ok || named[src] {
				continue
			}
			named[src] = true
			out = append(out, diag.Diagnostic{
				Level: diag.LevelError, Phase: diag.PhaseSemanticValidate,
				Code: CodeOnfailNamesRescue,
				Message: fmt.Sprintf("onfail: on %q names register %q, which is set inside the rescue unit %q of %q — a unit's failure is not a failure of its own, it means %q is not handled",
					taskLabel(v.task), src, taskLabel(u.Task), u.Source, u.Source),
				Hint: "rescue the source instead: name " + u.Source + " in onfail:, or decide inside the unit with a when: on register." + src + " (a when: reading it stays legal)",
			})
		}
		if !v.inUnit && v.task.Module != nil && v.task.Module.Module == TaskFailedModule && !hasTaskFailedMessage(v.task) {
			out = append(out, diag.Diagnostic{
				Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
				Code: "missing_required_field",
				Message: fmt.Sprintf("params.message is required on %s %q outside a rescue unit — there is no original error to fall back to",
					TaskFailedModule, taskLabel(v.task)),
				Hint: "give the failure a message:, or put the task inside a rescue unit (its own onfail: or an enclosing block's), where it defaults to the original error",
			})
		}
	}
	return out
}

// GroupMultipleRescueUnits raises [CodeGroupMultipleRescueUnits] for every block
// rolled as one group ([BlockRollsAsGroup]) that holds more than one rescue unit
// for sources outside it. A unit counts when it holds some task of the group:
// the group itself, a construct inside it, or one enclosing it. The units that
// hold the whole group — its own and its enclosing constructs' — count as one:
// inside the message each holds the same plan, as one `onfail:` naming all their
// sources would. A construct naming several outside sources is one unit; a source
// set inside the group is an in-message pair the Soul decides itself and is not
// counted.
//
// scenario is the scenario level of `settings:`; tasks is the plan after include
// expansion, the plan the group's message is built from.
func GroupMultipleRescueUnits(scenario *ScenarioSettings, tasks []Task) []diag.Diagnostic {
	units := RescueUnits(tasks)
	var out []diag.Diagnostic
	for i := range tasks {
		groupRescueUnits(scenario, tasks[i], StepPath("", i), units, &out)
	}
	return out
}

func groupRescueUnits(scenario *ScenarioSettings, t Task, path string, units []RescueUnit, out *[]diag.Diagnostic) {
	if t.Block == nil {
		return
	}
	if !BlockRollsAsGroup(scenario, t) {
		for j := range t.Block.Block {
			groupRescueUnits(scenario, LinearBlockChild(t, t.Block.Block[j]), StepPath(path, j), units, out)
		}
		return
	}
	inside := map[string]bool{}
	collectDeclaredRegisters(t.Block.Block, inside)
	var held []string
	whole := -1
	seen := map[string]bool{}
	group := RescueUnit{Path: path}
	for _, u := range units {
		holdsAll := u.Holds(path)
		if inside[u.Source] || seen[u.Path] || !(holdsAll || (u.Path != path && group.Holds(u.Path))) {
			continue
		}
		seen[u.Path] = true
		label := fmt.Sprintf("%q (onfail: %s)", taskLabel(u.Task), u.Source)
		switch {
		case holdsAll && whole >= 0:
			held[whole] += " with " + label
		case holdsAll:
			whole = len(held)
			held = append(held, label)
		default:
			held = append(held, label)
		}
	}
	if len(held) < 2 {
		return
	}
	*out = append(*out, diag.Diagnostic{
		Level: diag.LevelError, Phase: diag.PhaseSemanticValidate,
		Code: CodeGroupMultipleRescueUnits,
		Message: fmt.Sprintf("block %q rolls as one group (serial_scope: block) and holds %d rescue units for sources outside it: %s — the group travels as one message, where a second unit would run after the first succeeded or never get its chance after the first failed",
			taskLabel(&t), len(held), strings.Join(held, ", ")),
		Hint: "keep one rescue unit for outside sources per group: merge them into one block, move the others out of the group, or set serial_scope: task on it so each child is a step of its own",
	})
}
