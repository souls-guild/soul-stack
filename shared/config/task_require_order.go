package config

import (
	"fmt"

	"github.com/goccy/go-yaml/ast"

	"github.com/souls-guild/soul-stack/shared/diag"
)

// validateRequireOrder raises `require_forward_reference` for every `require:`
// element naming a source that is NOT earlier in the plan than the task awaiting
// it (ADR-0075(b.1), destiny/tasks.md §8).
//
// Why a forward barrier is an error rather than a slow path: a Soul runner
// resolves what a task waits for IN THE MAIN FLOW, at the awaiting task's plan
// position (soul/internal/runtime.asyncFlows.barriers). Only flows LAUNCHED by
// then can be in the set, so a `require:` naming a source that starts later
// resolves to nothing — the runner does not deadlock, it does not fail, it
// simply does not wait. The ordering the author wrote into the plan silently
// never happens, and the first symptom is a task reading a half-written file on
// a machine that happened to be slow. Every other outcome of a mistyped barrier
// is already an error (an unknown name → unknown_register_reference, a source in
// a later Passage → cross-passage reject at render); a barrier that resolves to
// a no-op is the one gap left, and this closes it.
//
// The rule also makes a `require:` CYCLE unrepresentable rather than merely
// unlikely: a cycle needs at least one edge pointing forward, so rejecting
// forward edges rejects every cycle — including the degenerate self-cycle, a
// task naming its own `register:`.
//
// Position is the in-order walk of the task list, descending into `block:` — the
// same order render lays out indices in (a block fans out at its own position,
// children in order). An `include:` splices tasks in place and cannot reorder two
// tasks of one file, so checking within a file is sound before expansion. A name
// no task in this file declares is skipped: it is either an unknown register
// (already diagnosed) or a cross-file source, which the flat-plan check owns.
//
// A source that IS earlier is not flagged, whether or not it is `async:`: on an
// ordinary task the barrier is redundant but true (destiny/tasks.md §8 — "in a
// linear flow without async: the requirement is redundant"), and a plan that
// gains an `async:` on that source later keeps working without an edit.
//
// tasksSeq — the `tasks:` node (scenario) or the root sequence (destiny); nil → nil.
func validateRequireOrder(tasksSeq *ast.SequenceNode, pathPrefix string) []diag.Diagnostic {
	if tasksSeq == nil {
		return nil
	}
	// Last-wins on a repeated name, mirroring the keeper-side registerIndex a
	// barrier actually resolves through. A duplicate is its own error
	// (duplicate_task_address); agreeing with render here keeps the two
	// diagnostics from contradicting each other on the same file.
	order := map[string]int{}
	pos := 0
	collectRegisterOrder(tasksSeq, order, &pos)

	var out []diag.Diagnostic
	pos = 0
	checkRequireOrder(tasksSeq, pathPrefix, order, &pos, &out)
	return out
}

// collectRegisterOrder numbers every task in the walk and records the position of
// each declared `register:`. Both passes number identically: one increment per
// task before its keys are read, recursion on the `block:` key — so a block gets
// its own position and its children the ones straight after it.
func collectRegisterOrder(seq *ast.SequenceNode, order map[string]int, pos *int) {
	for _, item := range seq.Values {
		self := *pos
		*pos++
		mm, ok := item.(*ast.MappingNode)
		if !ok {
			continue
		}
		for _, kv := range mm.Values {
			tok := kv.Key.GetToken()
			if tok == nil {
				continue
			}
			switch tok.Value {
			case "register":
				if sn, isStr := kv.Value.(*ast.StringNode); isStr && sn.Value != "" {
					order[sn.Value] = self
				}
			case "block":
				if bseq, isSeq := kv.Value.(*ast.SequenceNode); isSeq {
					collectRegisterOrder(bseq, order, pos)
				}
			}
		}
	}
}

// checkRequireOrder walks the same order as collectRegisterOrder and diagnoses
// each `require:` element whose source is not strictly earlier. The scalar form
// (`require: all`) carries no names and is positional by definition ("every async
// task started earlier in this run"), so it cannot point forward and is skipped.
func checkRequireOrder(seq *ast.SequenceNode, pathPrefix string, order map[string]int, pos *int, out *[]diag.Diagnostic) {
	for i, item := range seq.Values {
		self := *pos
		*pos++
		taskPath := fmt.Sprintf("%s[%d]", pathPrefix, i)
		mm, ok := item.(*ast.MappingNode)
		if !ok {
			continue
		}
		for _, kv := range mm.Values {
			tok := kv.Key.GetToken()
			if tok == nil {
				continue
			}
			switch tok.Value {
			case "require":
				rseq, isSeq := kv.Value.(*ast.SequenceNode)
				if !isSeq {
					continue // `require: all`, or a shape validateRequireField rejects.
				}
				*out = append(*out, requireOrderDiags(rseq, order, self, taskPath)...)
			case "block":
				if bseq, isSeq := kv.Value.(*ast.SequenceNode); isSeq {
					checkRequireOrder(bseq, taskPath+".block", order, pos, out)
				}
			}
		}
	}
}

// requireOrderDiags checks one `require:` list against the declaration order.
// Non-string and CEL-wrapped elements are skipped (the name is not statically
// known), as is a name no task declares — that is unknown_register_reference's,
// and raising both on one element would just double the noise.
func requireOrderDiags(rseq *ast.SequenceNode, order map[string]int, self int, taskPath string) []diag.Diagnostic {
	var out []diag.Diagnostic
	for j, item := range rseq.Values {
		sn, isStr := item.(*ast.StringNode)
		if !isStr || isCELWrapped(sn.Value) {
			continue
		}
		srcPos, known := order[sn.Value]
		if !known || srcPos < self {
			continue
		}
		message := fmt.Sprintf("require[%d] names register %q, which is declared later in the plan", j, sn.Value)
		hint := "a barrier resolves its targets at the awaiting task's plan position, so a source that starts later is never in the set — the wait silently never happens; move the source above this task, or drop the require:"
		if srcPos == self {
			message = fmt.Sprintf("require[%d] names register %q, which this task declares itself", j, sn.Value)
			hint = "a task cannot wait for itself — it is not launched when its own barrier is resolved, so the wait silently never happens; name the source task's register instead"
		}
		rt := sn.GetToken()
		line, col := 0, 0
		if rt != nil {
			line, col = rt.Position.Line, rt.Position.Column
		}
		out = append(out, diagAt(line, col, diag.Diagnostic{
			Level: diag.LevelError, Phase: diag.PhaseSemanticValidate,
			Code:     "require_forward_reference",
			Message:  message,
			Hint:     hint,
			YAMLPath: fmt.Sprintf("%s.require[%d]", taskPath, j),
		}))
	}
	return out
}
