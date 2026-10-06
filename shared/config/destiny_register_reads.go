package config

import (
	"fmt"
	"sort"

	"github.com/souls-guild/soul-stack/shared/diag"
)

// CodeWithinDestinyRegisterDependency — a destiny task reads, in a key the Keeper
// renders before dispatch, a register that a task of the SAME destiny declares: a
// sibling's or its own. The destiny counterpart of
// [CodeWithinBlockRegisterDependency] (NIM-939).
//
// The Keeper renders a whole destiny in one pass against an empty `register` root
// (keeper/internal/render.renderApplyDestiny builds the destiny's RenderInput with no
// register channel of the caller's) and ships every task in one message, so no task of
// the destiny has run when such a key is rendered. A plain read fails the run at render
// as `no such key`; a `has()`-guarded one renders without error and takes its fallback
// on every run.
const CodeWithinDestinyRegisterDependency = "within_destiny_register_dependency"

// validateFlatDestinyRegisterReads raises [CodeWithinDestinyRegisterDependency] for
// every (task, register) pair of a destiny's FLAT plan where a Keeper-rendered key
// reads a register declared anywhere in that plan.
//
// Flat, after include expansion, because the declaring task may live in another file:
// an included body is linted with its includer's registers in scope, so the per-file
// pass sees a known name and stays quiet. The keys are [taskOwnReads] — the set that
// defines a Passage in a scenario — plus `on:`, which a scenario does not order by but
// the Keeper still renders. Flow control (`when:`/`changed_when:`/`failed_when:`/
// `retry.until`) is outside it and stays legal: the Soul evaluates it inside the
// message. A name no task declares is left to `unknown_register_reference`, which
// covers every key here except `on:`.
//
// Not covered, and silent under `has()`: a destiny's `vars.yml`, which is loaded beside
// the plan rather than in it, an undeclared name in `on:`, and `register.self`, which
// [ExtractRegisterRefs] drops.
func validateFlatDestinyRegisterReads(tasks []Task) []diag.Diagnostic {
	declared := map[string]string{}
	collectDestinyDeclaredRegisters(tasks, declared)
	if len(declared) == 0 {
		return nil
	}
	var out []diag.Diagnostic
	collectDestinyRegisterReads(tasks, declared, &out)
	return out
}

// collectDestinyDeclaredRegisters maps every register name declared in tasks (block
// children included) to the name of the task declaring it.
func collectDestinyDeclaredRegisters(tasks []Task, out map[string]string) {
	for i := range tasks {
		if tasks[i].Register != "" {
			out[tasks[i].Register] = taskDisplayName(&tasks[i])
		}
		if tasks[i].Block != nil {
			collectDestinyDeclaredRegisters(tasks[i].Block.Block, out)
		}
	}
}

// collectDestinyRegisterReads reports each task's own Keeper-rendered reads, node by
// node, so a finding names the block child that reads rather than its container.
// A block's own `vars:` is merged into its children at render and is reported against
// the block.
func collectDestinyRegisterReads(tasks []Task, declared map[string]string, out *[]diag.Diagnostic) {
	for i := range tasks {
		t := &tasks[i]
		seen := map[string]bool{}
		taskOwnReads(t, seen)
		addValueRefs(t.On, seen)
		names := make([]string, 0, len(seen))
		for name := range seen {
			names = append(names, name)
		}
		sort.Strings(names)
		for _, name := range names {
			declarer, ok := declared[name]
			if !ok {
				continue
			}
			var msg string
			if t.Register == name {
				msg = fmt.Sprintf("destiny task %q reads its own register %q in a key the Keeper renders before dispatch (where/vars/params/loop/on)", taskDisplayName(t), name)
			} else {
				msg = fmt.Sprintf("destiny task %q reads register %q, declared by task %q of the same destiny, in a key the Keeper renders before dispatch (where/vars/params/loop/on)", taskDisplayName(t), name, declarer)
			}
			*out = append(*out, diag.Diagnostic{
				Level: diag.LevelError, Phase: diag.PhaseSemanticValidate,
				Code:    CodeWithinDestinyRegisterDependency,
				Message: msg + " — the whole destiny is rendered before any of its tasks runs, so the value cannot exist yet",
				Hint:    "read an earlier task's register in when:/changed_when:/failed_when:/retry.until, which the Soul evaluates inside the message (a task's own result is register.self in changed_when:/failed_when:/retry.until), or produce the value in the scenario before the applier and pass it in through apply.input",
			})
		}
		if t.Block != nil {
			collectDestinyRegisterReads(t.Block.Block, declared, out)
		}
	}
}
