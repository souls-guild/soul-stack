package config

import (
	"fmt"

	"github.com/goccy/go-yaml/ast"

	"github.com/souls-guild/soul-stack/shared/diag"
)

// TaskFailedModule is the author address of the module that always fails its task
// (ADR-0091 amendment NIM-993, rule 8): inside a rescue unit the explicit "error
// not handled", elsewhere Ansible's `fail`.
const TaskFailedModule = "core.task.failed"

// taskFailedForbiddenKeys are refused on a [TaskFailedModule] task, code
// `<key>_on_task_failed_invalid`. The task fails on purpose. A scenario step of its
// own is to be settled by the Keeper without a message (not built: the Soul runs it
// today), where such a key would mean something different or nothing at all, so a key
// accepted now would make one scenario behave two ways. `when:` stays — it is how a
// unit decides whether the error is handled, and how any other task fails only
// sometimes.
var taskFailedForbiddenKeys = []struct{ key, hint string }{
	{"failed_when", "drop failed_when:; the task always fails — gate it with when: instead"},
	{"changed_when", "drop changed_when:; the task changes nothing and always fails"},
	{"retry", "drop retry:; a deliberate failure fails the same on every attempt"},
	{"timeout", "drop timeout:; the task fails at once, there is nothing for a timeout to bound"},
}

// validateTaskFailedForbiddenKeys raises `<key>_on_task_failed_invalid` for each
// present key of [taskFailedForbiddenKeys] on a [TaskFailedModule] task. Raised on
// presence, like `<key>_on_apply_invalid`.
//
// `settings.task_timeout` written on the task is the long form of `timeout:`
// ([Task.OwnSettings]) and is refused as `timeout:`. One the task inherits from
// the scenario or an enclosing block is that level's and stays legal.
func validateTaskFailedForbiddenKeys(present map[string]*ast.MappingValueNode, pathPrefix string) []diag.Diagnostic {
	if taskModuleAddr(present) != TaskFailedModule {
		return nil
	}
	var out []diag.Diagnostic
	refuse := func(kv *ast.MappingValueNode, key, code, hint string) {
		out = append(out, diagAtKV(kv, diag.Diagnostic{
			Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
			Code: code,
			Message: fmt.Sprintf("%s: is not allowed on %s — the task fails on purpose, and a step of its own is to be settled by the Keeper, where the key would mean something else or nothing; to fail only sometimes, gate it with when:",
				key, TaskFailedModule),
			Hint:     hint,
			YAMLPath: pathPrefix + "." + key,
		}))
	}
	for _, f := range taskFailedForbiddenKeys {
		if kv, ok := present[f.key]; ok {
			refuse(kv, f.key, f.key+"_on_task_failed_invalid", f.hint)
		}
	}
	if s, ok := present["settings"]; ok {
		if m, isMap := s.Value.(*ast.MappingNode); isMap {
			for _, kv := range m.Values {
				if tok := kv.Key.GetToken(); tok != nil && tok.Value == "task_timeout" {
					refuse(kv, "settings.task_timeout", "timeout_on_task_failed_invalid",
						"drop settings.task_timeout:; the task fails at once, there is nothing for a timeout to bound")
				}
			}
		}
	}
	return out
}

// hasTaskFailedMessage reports whether a [TaskFailedModule] task carries a
// non-empty `message:`. An empty string fails with the generic text, so it is no
// message for the rule that requires one.
func hasTaskFailedMessage(t *Task) bool {
	if t.Module == nil {
		return false
	}
	switch v := t.Module.Params["message"].(type) {
	case nil:
		return false
	case string:
		return v != ""
	default:
		return true
	}
}
