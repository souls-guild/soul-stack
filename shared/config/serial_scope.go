package config

import (
	"fmt"
	"strings"

	"github.com/souls-guild/soul-stack/shared/diag"
)

// The serial_scope checks run on the flat plan, after include expansion, and are
// called by soul-lint and by the Keeper's run path alike: service repositories are
// not linted, so a check only the linter made would leave the change silent there
// (docs/scenario/settings.md §3.1).
const (
	// CodeSerialScopeImplicit — a block reached by `serial:` (its own or an
	// enclosing block's) over two or more host tasks, and no line the author wrote
	// sets its `serial_scope`. Under linear dispatch such a block is an overlay by
	// default, so its children stop travelling in one wave.
	CodeSerialScopeImplicit = "serial_scope_implicit"
	// CodeSerialScopeImplicitAcrossTasks — the transition rule: a `serial:` sharing
	// an ADR-056 Passage with another host step, whose `serial_scope` no line the
	// author wrote sets. Today one wave loop carries the whole Passage at its
	// narrowest width; under linear dispatch each step rolls on its own. It reads
	// the derivation, so it goes when the derivation goes.
	CodeSerialScopeImplicitAcrossTasks = "serial_scope_implicit_across_tasks"
	// CodeGroupChildKeyUnread — a warning: `serial:` or a barrier-kind setting on a
	// descendant of a block rolled as one group, which linear dispatch does not read
	// (settings.md §3 rule 4). A nested block's `serial:` there is read today.
	CodeGroupChildKeyUnread = "group_child_key_unread"
)

// serialFrame is what a task inherits from the levels above it.
type serialFrame struct {
	scope        string
	scopeWritten bool
	// serial: this level or one above it carries `serial:`.
	serial bool
}

func scenarioSerialFrame(s *ScenarioSettings) serialFrame {
	f := serialFrame{scope: SerialScopeTask}
	if s != nil && s.SerialScope != nil {
		f.scope, f.scopeWritten = *s.SerialScope, true
	}
	return f
}

func (f serialFrame) enter(t *Task) serialFrame {
	if t.Settings != nil && t.Settings.SerialScope != nil {
		f.scope, f.scopeWritten = *t.Settings.SerialScope, true
	}
	if t.Serial != nil {
		f.serial = true
	}
	return f
}

// group: `serial_scope` is read only on a block that `serial:` reaches.
func (f serialFrame) group() bool {
	return f.serial && f.scope == SerialScopeBlock
}

// isHostTask reports whether t is dispatched to hosts. A block is not counted —
// its leaves are — and an `assert:` fails the render rather than a host.
func isHostTask(t *Task) bool {
	if t.Module == nil && t.Apply == nil {
		return false
	}
	return !IsKeeperSideTask(*t)
}

func hostTasksBeneath(children []Task) int {
	n := 0
	for i := range children {
		if children[i].Block != nil {
			n += hostTasksBeneath(children[i].Block.Block)
		} else if isHostTask(&children[i]) {
			n++
		}
	}
	return n
}

// ImplicitSerialScope returns [CodeSerialScopeImplicit] for every outermost block
// that needs a written `serial_scope` and lacks one, and [CodeGroupChildKeyUnread]
// for every key a group's descendant writes in vain. scenario is the scenario-level
// `settings:`; tasks is the plan after include expansion.
func ImplicitSerialScope(scenario *ScenarioSettings, tasks []Task) []diag.Diagnostic {
	var out []diag.Diagnostic
	root := scenarioSerialFrame(scenario)
	for i := range tasks {
		walkImplicitSerialScope(&tasks[i], root, &out)
	}
	return out
}

func walkImplicitSerialScope(t *Task, up serialFrame, out *[]diag.Diagnostic) {
	if t.Block == nil {
		return
	}
	f := up.enter(t)
	if f.group() {
		collectGroupChildKeys(t, t.Block.Block, out)
		return
	}
	// Reported at the outermost such block only: a line written there settles
	// every block beneath it, so a block reached by an enclosing serial: is never
	// the one reported.
	if f.serial && !f.scopeWritten {
		if n := hostTasksBeneath(t.Block.Block); n >= 2 {
			*out = append(*out, implicitSerialScopeDiag(t, n))
			return
		}
	}
	for i := range t.Block.Block {
		walkImplicitSerialScope(&t.Block.Block[i], f, out)
	}
}

func implicitSerialScopeDiag(block *Task, hostTasks int) diag.Diagnostic {
	return diag.Diagnostic{
		Level: diag.LevelError, Phase: diag.PhaseSemanticValidate,
		Code: CodeSerialScopeImplicit,
		Message: fmt.Sprintf("block %q carries serial: %v over %d host tasks, and no line the author wrote sets its serial_scope -- "+
			"under linear dispatch a block is an overlay by default, so each child rolls over every host "+
			"before the next child starts; a canary needs serial_scope: block", taskLabel(block), block.Serial, hostTasks),
		Hint: "write settings: {serial_scope: block} on the block (or on a level above it) to keep one wave carrying " +
			"the whole block -- a canary; write serial_scope: task if the children are independent steps",
	}
}

func collectGroupChildKeys(group *Task, children []Task, out *[]diag.Diagnostic) {
	for i := range children {
		c := &children[i]
		var keys []string
		if c.Serial != nil {
			keys = append(keys, "serial:")
		}
		if c.Settings != nil {
			for _, k := range settingKeys {
				if k.kind == settingBarrier && k.written(*c.Settings) {
					keys = append(keys, "settings."+k.name)
				}
			}
		}
		for _, key := range keys {
			*out = append(*out, diag.Diagnostic{
				Level: diag.LevelWarning, Phase: diag.PhaseSemanticValidate,
				Code: CodeGroupChildKeyUnread,
				Message: fmt.Sprintf("%s on %q is not read under linear dispatch: block %q rolls as one group (serial_scope: block), "+
					"so its descendants share its one message, barrier and width", key, taskLabel(c), taskLabel(group)),
				Hint: "write the key on the group block itself, or set serial_scope: task on it to make each child its own step",
			})
		}
		if c.Block != nil {
			collectGroupChildKeys(group, c.Block.Block, out)
		}
	}
}

// serialUnit is a part of a top-level task that rolls on its own under linear
// dispatch.
type serialUnit struct {
	task          *Task
	carriesSerial bool
	scopeWritten  bool
}

// ImplicitSerialScopeAcrossTasks returns [CodeSerialScopeImplicitAcrossTasks] for
// every unit carrying `serial:` that shares a Passage of passage with another host
// unit and has no written `serial_scope` at or above it. Either value opts out:
// neither stretches a wave over the next task.
func ImplicitSerialScopeAcrossTasks(scenario *ScenarioSettings, tasks []Task, passage Passage) []diag.Diagnostic {
	byPassage := make([][]serialUnit, passage.Count)
	root := scenarioSerialFrame(scenario)
	for i := range tasks {
		if i >= len(passage.TaskPassage) {
			break
		}
		p := passage.TaskPassage[i]
		if p < 0 || p >= passage.Count {
			continue
		}
		collectSerialUnits(&tasks[i], root, true, &byPassage[p])
	}
	var out []diag.Diagnostic
	for p, units := range byPassage {
		if len(units) < 2 {
			continue
		}
		for k, u := range units {
			if !u.carriesSerial || u.scopeWritten {
				continue
			}
			partner := units[len(units)-1]
			if k+1 < len(units) {
				partner = units[k+1]
			} else if k > 0 {
				partner = units[k-1]
			}
			out = append(out, diag.Diagnostic{
				Level: diag.LevelError, Phase: diag.PhaseSemanticValidate,
				Code: CodeSerialScopeImplicitAcrossTasks,
				Message: fmt.Sprintf("%q carries serial: %v and shares Passage %d of the derived plan with host task %q, "+
					"and no line the author wrote sets its serial_scope -- today one wave loop carries the whole Passage "+
					"at its narrowest serial:; under linear dispatch each step rolls on its own, so %q no longer travels in the waves of %q",
					taskLabel(u.task), u.task.Serial, p, taskLabel(partner.task), taskLabel(partner.task), taskLabel(u.task)),
				Hint: fmt.Sprintf("to keep the steps in one wave (a canary), put them in one block with the serial: and "+
					"settings: {serial_scope: block}; if they are independent, write settings: {serial_scope: task} on %q "+
					"or in the scenario's settings: to say so", taskLabel(u.task)),
			})
		}
	}
	return out
}

// collectSerialUnits splits t into the units linear dispatch rolls apart. A block
// with no `serial:` of its own is an overlay whatever its `serial_scope`, so its
// children are units; a block with `serial:` is one unit (whether it rolls as a
// group or not is [ImplicitSerialScope]'s question). carriesSerial follows what the
// Passage width reads today ([rollsToday]): a module or applier child's own
// `serial:` is not read inside a block, so only a top-level leaf's counts.
func collectSerialUnits(t *Task, up serialFrame, topLevel bool, units *[]serialUnit) {
	f := up.enter(t)
	if t.Block != nil {
		if t.Serial == nil {
			for i := range t.Block.Block {
				collectSerialUnits(&t.Block.Block[i], f, false, units)
			}
			return
		}
		if hostTasksBeneath(t.Block.Block) > 0 {
			*units = append(*units, serialUnit{task: t, carriesSerial: rollsToday(t), scopeWritten: f.scopeWritten})
		}
		return
	}
	if !isHostTask(t) {
		return
	}
	*units = append(*units, serialUnit{task: t, carriesSerial: topLevel && t.Serial != nil, scopeWritten: f.scopeWritten})
}

// rollsToday reports whether some host task under block t gets a positive width
// from today's render: a block stamps its own `serial:` on its module and applier
// children only, and a nested block rolls at its own `serial:`, none when it has
// none (keeper/internal/render/block.go renderBlockTask).
func rollsToday(t *Task) bool {
	for i := range t.Block.Block {
		c := &t.Block.Block[i]
		if c.Block != nil {
			if rollsToday(c) {
				return true
			}
		} else if t.Serial != nil && isHostTask(c) {
			return true
		}
	}
	return false
}

func taskLabel(t *Task) string {
	if name := strings.TrimSpace(t.Name); name != "" {
		return name
	}
	if t.Module != nil {
		return t.Module.Module
	}
	if t.Block != nil {
		return "unnamed block"
	}
	return "unnamed task"
}
