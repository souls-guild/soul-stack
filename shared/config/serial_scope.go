package config

import (
	"fmt"
	"strings"

	"github.com/souls-guild/soul-stack/shared/diag"
)

// CodeGroupChildKeyUnread — a warning: `serial:` or a barrier-kind setting on a
// descendant of a block rolled as one group, which linear dispatch does not read
// (settings.md §3 rule 4). A nested block's `serial:` there is read by the staged
// rollback.
const CodeGroupChildKeyUnread = "group_child_key_unread"

// serialFrame is what a task inherits from the levels above it.
type serialFrame struct {
	scope string
	// serial: this level or one above it carries `serial:`.
	serial bool
}

func scenarioSerialFrame(s *ScenarioSettings) serialFrame {
	f := serialFrame{scope: SerialScopeTask}
	if s != nil && s.SerialScope != nil {
		f.scope = *s.SerialScope
	}
	return f
}

func (f serialFrame) enter(t *Task) serialFrame {
	if t.Settings != nil && t.Settings.SerialScope != nil {
		f.scope = *t.Settings.SerialScope
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

// GroupChildKeysUnread returns [CodeGroupChildKeyUnread] for every key a group's
// descendant writes in vain. scenario is the scenario-level `settings:`; tasks is
// the plan after include expansion. soul-lint warns; the Keeper accepts the file.
func GroupChildKeysUnread(scenario *ScenarioSettings, tasks []Task) []diag.Diagnostic {
	var out []diag.Diagnostic
	root := scenarioSerialFrame(scenario)
	for i := range tasks {
		walkGroups(&tasks[i], root, &out)
	}
	return out
}

func walkGroups(t *Task, up serialFrame, out *[]diag.Diagnostic) {
	if t.Block == nil {
		return
	}
	f := up.enter(t)
	if f.group() {
		collectGroupChildKeys(t, t.Block.Block, out)
		return
	}
	for i := range t.Block.Block {
		walkGroups(&t.Block.Block[i], f, out)
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
