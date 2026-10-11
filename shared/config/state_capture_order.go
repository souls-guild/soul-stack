package config

import (
	"sort"
	"strings"
)

// Guards around a `core.state.<verb>` capture step ([ADR-0084], "The ordering
// guard"). They read the EXPANDED plan (after [ExpandIncludes]), which is why they
// live beside the other plan predicates and are emitted from soul-lint's stage
// pass instead of the per-file task rules: a capture and the task it has to be
// ordered against routinely land in different included files, and a per-file rule
// would silently not compare them.
//
// The stale same-Passage read rule (`state_stale_same_passage_read`) retired with
// linear dispatch (ADR-0091): a capture is a step of its own, and the run
// re-reads the state before every later step renders.
const (
	// CodeStateStoreAfterUse — a task consumes a register BEFORE the
	// `core.state.<verb>` step that captures the same register. Generate → store
	// → use is crash-safe at every point: crash before the capture and nothing
	// was applied, crash after it and the value is in Postgres/Vault where the
	// operator can reach it. Generate → use → store leaves, in the window between
	// the two, a live host configured with a value that exists nowhere else —
	// the failure this ADR is written against. Fail-closed reject offline.
	CodeStateStoreAfterUse = "state_store_after_use"
	// CodeStateWideMatch — a `modify`/`remove` capture whose `match:` is absent or
	// constant-true, so it repatches or demolishes the WHOLE collection. WARN, not
	// an error: the wide form is legitimate ("clear every user"), it is just far
	// more often a predicate the author forgot to narrow. Ported from the ADR-057
	// §d fuse — it is the one safeguard of the removed grammar with no equivalent
	// on the module path, because a module manifest can require a param but cannot
	// say "this one is suspicious when it says `true`".
	CodeStateWideMatch = "state_wide_match"
)

// WideStateMatch returns every `modify`/`remove` capture in the expanded plan
// whose `match:` would select the entire collection: absent, empty, or a literal
// `true`. Unlike the ordering guard the defect is inside one task, but it is
// emitted from the same stage pass, because a capture
// routinely arrives through `include:` and the per-file task rules never see it.
//
// A `match:` carrying an interpolation is NOT reported: its value is decided at
// render, and warning on every `${ … }` predicate would make the rule noise.
func WideStateMatch(tasks []Task) []StateOrderInfo {
	var out []StateOrderInfo
	for _, node := range flattenOrdered(tasks) {
		verb, isCapture := stateCaptureVerb(node.task)
		if !isCapture || (verb != "modify" && verb != "remove") {
			continue
		}
		match, _ := node.task.Module.Params["match"].(string)
		if strings.TrimSpace(match) != "" && !isConstTrueMatch(match) {
			continue
		}
		field, _ := captureField(node.task)
		out = append(out, StateOrderInfo{
			CaptureName: node.task.Name,
			CaptureVerb: verb,
			Ref:         field,
		})
	}
	return out
}

// StateOrderInfo holds the coordinates of one ordering defect around a capture
// step (for the linter diagnostic).
type StateOrderInfo struct {
	CaptureName string // name of the `core.state.<verb>` task
	CaptureVerb string // the verb, i.e. the module state
	OtherName   string // name of the task on the wrong side of it
	Ref         string // register name (store-after-use) / state field (wide match)
}

// StoreAfterUse detects the one broken capture order: a task consumes a register
// that a `core.state.<verb>` step captures, and consumes it FIRST.
//
// FIRST is plan order, the order a run executes its steps in (ADR-0091);
// under the derived Passages a consumer could run before a capture written above
// it. Only a non-capture task counts as the consumer — two captures reading the
// same register are two stores, and their relative order carries no crash-safety
// meaning.
func StoreAfterUse(tasks []Task) (info StateOrderInfo, ok bool) {
	nodes := flattenOrdered(tasks)
	reads := make([]map[string]bool, len(nodes))
	for i := range nodes {
		seen := map[string]bool{}
		taskOwnReads(nodes[i].task, seen)
		reads[i] = seen
	}
	for ci := range nodes {
		verb, isCapture := stateCaptureVerb(nodes[ci].task)
		if !isCapture {
			continue
		}
		for _, ref := range sortedKeys(reads[ci]) {
			for ui := range nodes {
				if ui == ci || !reads[ui][ref] {
					continue
				}
				if _, alsoCapture := stateCaptureVerb(nodes[ui].task); alsoCapture {
					continue
				}
				if nodes[ui].idx >= nodes[ci].idx {
					continue
				}
				return StateOrderInfo{
					CaptureName: taskDisplayName(nodes[ci].task),
					CaptureVerb: verb,
					OtherName:   taskDisplayName(nodes[ui].task),
					Ref:         ref,
				}, true
			}
		}
	}
	return StateOrderInfo{}, false
}

// orderedTask is one node of the plan paired with the TOP-LEVEL index that
// orders it against a capture. A block child carries its parent's index: a
// capture cannot sit in a block (block_on_keeper_invalid), so a block runs wholly
// before or wholly after it.
type orderedTask struct {
	idx  int
	task *Task
}

func flattenOrdered(tasks []Task) []orderedTask {
	var out []orderedTask
	var walk func(t *Task, idx int)
	walk = func(t *Task, idx int) {
		out = append(out, orderedTask{idx: idx, task: t})
		if t.Block != nil {
			for i := range t.Block.Block {
				walk(&t.Block.Block[i], idx)
			}
		}
	}
	for i := range tasks {
		walk(&tasks[i], i)
	}
	return out
}

// stateCaptureVerb returns the verb of a `core.state.<verb>` step. The match is
// on the module BASE via [SplitModuleAddr], never on a string prefix, so
// `core.stateful.set` is not a capture.
func stateCaptureVerb(t *Task) (string, bool) {
	if t.Module == nil {
		return "", false
	}
	name, verb, ok := SplitModuleAddr(t.Module.Module)
	if !ok || name != stateModuleAddr {
		return "", false
	}
	return verb, true
}

// StateCaptureVerb is [stateCaptureVerb] for callers outside this package
// (soul-lint's compute-scope walk). Exported rather than re-derived there so the
// module base address stays spelled in exactly one place.
func StateCaptureVerb(t *Task) (string, bool) { return stateCaptureVerb(t) }

// captureField returns the state field a capture writes, and known=false when
// `field:` is absent or is an expression (its value is decided at render).
func captureField(t *Task) (string, bool) {
	if t.Module == nil {
		return "", false
	}
	field, isString := t.Module.Params["field"].(string)
	if !isString || field == "" || strings.Contains(field, "${") {
		return "", false
	}
	return field, true
}

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
