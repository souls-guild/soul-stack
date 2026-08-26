package config

import (
	"regexp"
	"sort"
	"strings"
)

// Ordering guards around a `core.state.<verb>` capture step ([ADR-0084], "The
// ordering guard"). Both read the EXPANDED plan (after [ExpandIncludes]) and
// [Stratify]'s Passage assignment, which is why they live beside the other plan
// predicates and are emitted from soul-lint's stage pass instead of the per-file
// task rules: a capture and the task it has to be ordered against routinely land
// in different included files, and a per-file rule would silently not compare
// them.
const (
	// CodeStateStoreAfterUse — a task consumes a register BEFORE the
	// `core.state.<verb>` step that captures the same register. Generate → store
	// → use is crash-safe at every point: crash before the capture and nothing
	// was applied, crash after it and the value is in Postgres/Vault where the
	// operator can reach it. Generate → use → store leaves, in the window between
	// the two, a live host configured with a value that exists nowhere else —
	// the failure this ADR is written against. Fail-closed reject offline.
	CodeStateStoreAfterUse = "state_store_after_use"
	// CodeStateStaleRead — a task reads `${ incarnation.state.<field> }` that a
	// capture EARLIER IN THE SAME PASSAGE writes. An interpolated read refreshes
	// only at a Passage boundary, so it renders the pre-capture value, while the
	// verb engine — which always reads live — would have written the new one.
	// Rejecting the pair keeps the two mechanisms from disagreeing in production,
	// and is also what keeps L0 honest: the trial harness threads state
	// task-by-task, so it cannot reproduce the difference.
	CodeStateStaleRead = "state_stale_same_passage_read"
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
// `true`. Unlike the two ordering guards this needs no [Passage] — the defect is
// inside one task — but it is emitted from the same stage pass, because a capture
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
	CaptureName    string // name of the `core.state.<verb>` task
	CaptureVerb    string // the verb, i.e. the module state
	CapturePassage int
	OtherName      string // name of the task on the wrong side of it
	OtherPassage   int
	Ref            string // register name (store-after-use) / state field (stale read)
}

// StoreAfterUse detects the one broken capture order: a task consumes a register
// that a `core.state.<verb>` step captures, and consumes it FIRST.
//
// Only a non-capture task counts as the consumer — two captures reading the same
// register are two stores, and their relative order carries no crash-safety
// meaning. `passage` is [Stratify]'s result for the same expanded tasks; a
// single-Passage plan is checked too (there the plan index alone is the order),
// unlike the cross-passage guards which are vacuous at Count==1.
func StoreAfterUse(tasks []Task, passage Passage) (info StateOrderInfo, ok bool) {
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
				if !runsBefore(passage, nodes[ui].idx, nodes[ci].idx) {
					continue
				}
				return StateOrderInfo{
					CaptureName:    taskDisplayName(nodes[ci].task),
					CaptureVerb:    verb,
					CapturePassage: passageOf(passage, nodes[ci].idx),
					OtherName:      taskDisplayName(nodes[ui].task),
					OtherPassage:   passageOf(passage, nodes[ui].idx),
					Ref:            ref,
				}, true
			}
		}
	}
	return StateOrderInfo{}, false
}

// StaleStateRead detects an interpolated `${ incarnation.state.<field> }` read
// that runs after a capture of the SAME field in the SAME Passage.
//
// A reader in a LATER Passage is correct by construction (the runner re-reads
// state at the boundary), and a reader BEFORE the capture legitimately renders
// the pre-capture value. Only the same-Passage after-case makes the interpolated
// read and the verb engine disagree.
//
// A capture whose `field:` is itself an expression is skipped: which field it
// writes is not knowable offline.
func StaleStateRead(tasks []Task, passage Passage) (info StateOrderInfo, ok bool) {
	nodes := flattenOrdered(tasks)
	fieldReads := make([][]string, len(nodes))
	for i := range nodes {
		fieldReads[i] = taskStateFieldReads(nodes[i].task)
	}
	for ci := range nodes {
		verb, isCapture := stateCaptureVerb(nodes[ci].task)
		if !isCapture {
			continue
		}
		field, known := captureField(nodes[ci].task)
		if !known {
			continue
		}
		capturePassage := passageOf(passage, nodes[ci].idx)
		for ui := range nodes {
			if ui == ci || passageOf(passage, nodes[ui].idx) != capturePassage {
				continue
			}
			if !runsBefore(passage, nodes[ci].idx, nodes[ui].idx) {
				continue
			}
			for _, f := range fieldReads[ui] {
				if f != field {
					continue
				}
				return StateOrderInfo{
					CaptureName:    taskDisplayName(nodes[ci].task),
					CaptureVerb:    verb,
					CapturePassage: capturePassage,
					OtherName:      taskDisplayName(nodes[ui].task),
					OtherPassage:   capturePassage,
					Ref:            field,
				}, true
			}
		}
	}
	return StateOrderInfo{}, false
}

// orderedTask is one node of the plan paired with the TOP-LEVEL index that
// decides when it runs. A block child carries its parent's index: a block is
// atomic per Passage, so two nodes of the same block are unordered with respect
// to each other and neither guard fires between them.
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

// runsBefore reports whether the top-level task at index i executes before the
// one at index j: Passage first, plan index within it.
func runsBefore(passage Passage, i, j int) bool {
	pi, pj := passageOf(passage, i), passageOf(passage, j)
	if pi != pj {
		return pi < pj
	}
	return i < j
}

// passageOf is the Passage of a top-level index, 0 when the plan has no entry
// for it (an unstratified or mismatched plan degrades to plain index order
// rather than to a wrong answer).
func passageOf(passage Passage, i int) int {
	if i < 0 || i >= len(passage.TaskPassage) {
		return 0
	}
	return passage.TaskPassage[i]
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

// reIncarnationStateField extracts the field name of an `incarnation.state`
// read, in both CEL forms: dotted access and a string index.
var reIncarnationStateField = regexp.MustCompile(`incarnation\.state\.([A-Za-z_][A-Za-z0-9_]*)|incarnation\.state\[\s*['"]([^'"]+)['"]\s*\]`)

// taskStateFieldReads collects the `incarnation.state.<field>` fields a single
// node interpolates. The zones are the ones [taskOwnReads] walks — the same
// render-time interpolation surface, and deliberately NOT the flow-control keys:
// those are evaluated Soul-side in their own sandbox, where incarnation.state is
// not declared at all.
func taskStateFieldReads(t *Task) []string {
	seen := map[string]bool{}
	addStateFieldRefs := func(v any) { walkStrings(v, func(s string) { collectStateFields(s, seen) }) }
	addStateFieldRefs(t.Where)
	if t.Loop != nil {
		addStateFieldRefs(t.Loop.When)
		addStateFieldRefs(t.Loop.Items)
	}
	addStateFieldRefs(t.Vars)
	addStateFieldRefs(t.Output)
	if t.Module != nil {
		addStateFieldRefs(t.Module.Params)
	}
	if t.Apply != nil {
		addStateFieldRefs(t.Apply.Input)
	}
	return sortedKeys(seen)
}

func collectStateFields(expr string, seen map[string]bool) {
	for _, m := range reIncarnationStateField.FindAllStringSubmatch(expr, -1) {
		switch {
		case m[1] != "":
			seen[m[1]] = true
		case m[2] != "":
			seen[m[2]] = true
		}
	}
}

// walkStrings applies fn to every string inside a value (string / map / seq).
// map[string]any is handled directly so the same walk serves both a params map
// and a bare CEL key.
func walkStrings(v any, fn func(string)) {
	switch t := v.(type) {
	case string:
		fn(t)
	case map[string]any:
		for _, sub := range t {
			walkStrings(sub, fn)
		}
	case []any:
		for _, sub := range t {
			walkStrings(sub, fn)
		}
	}
}

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
