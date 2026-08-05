package config

// Where a scenario's `assert:` can actually be answered (ADR-009 amendment
// 2026-06-23 form A, as narrowed by NIM-235 and extended by NIM-270).
//
// An `assert:` is evaluated at two points from one source — pre-flight on the
// request path, and render as a fail-safe. Which of the two answers a given
// assert is NOT a property of the assert alone; it depends on what the
// predicate reads and on how the run was started:
//
//   - an assert over `input.` / `vars.` / `incarnation.` is answered
//     pre-flight on BOTH paths — 422, nothing mutated;
//   - an assert that reads the roster (`soulprint.*`) is answered pre-flight
//     ONLY where the roster in front of the gate is the one the assert is about:
//     an existing incarnation whose plan consumes its roster, i.e. an explicit
//     run. On the create path there is no incarnation row yet and therefore no
//     membership (NIM-124), so it is deferred to render, and a mismatch reaches
//     the operator as `error_locked` rather than 422.
//
// A service author has no way to see that split from the DSL — both asserts
// look identical — so a size-guard in a create scenario silently behaves
// differently from what the ADR promises. This file is the static half of the
// answer: the linter names the split at authoring time.
//
// Scope is the scenario's own top-level task list, matching
// render.Pipeline.EvalAsserts, which likewise evaluates only top-level assert
// tasks. An assert spliced in from an `include:` branch is not seen here
// (includes expand later, at render), so a dispatcher scenario keeping its
// guards in branch files gets no warning — a known gap, not a silent claim of
// correctness.

import (
	"fmt"

	"github.com/goccy/go-yaml/ast"

	"github.com/souls-guild/soul-stack/shared/diag"
)

// validateAssertReachability warns about a roster-reading `assert:` in a create
// scenario, where the pre-flight gate cannot answer it.
//
// WARNING, not ERROR, and deliberately so: the construct is legal, it does
// exactly what it says, and on an explicit run of the same scenario it IS
// answered pre-flight. What is wrong is only the author's likely expectation of
// a 422 on `POST /v1/incarnations`. Failing the build over that would be
// telling authors to stop writing topology guards, which is the opposite of
// what we want.
//
// tasksNode carries the AST positions; tasks is the parsed list. They are
// walked in lockstep and the check is skipped entirely if they disagree in
// length — a parse error elsewhere already reported itself, and guessing at the
// pairing would attach the warning to the wrong line.
func validateAssertReachability(tasksNode *ast.SequenceNode, tasks []Task, create *bool) []diag.Diagnostic {
	if create == nil || !*create || tasksNode == nil {
		return nil
	}
	if len(tasksNode.Values) != len(tasks) {
		return nil
	}
	var out []diag.Diagnostic
	for i := range tasks {
		if tasks[i].Assert == nil || !AssertReadsRoster(&tasks[i]) {
			continue
		}
		d := diag.Diagnostic{
			Level: diag.LevelWarning, Phase: diag.PhaseSemanticValidate,
			Code: "assert_roster_deferred_on_create",
			Message: "this assert reads the run roster (soulprint.*), which does not exist on the create path: " +
				"POST /v1/incarnations answers before the incarnation row is written, so the predicate is deferred " +
				"to render and a mismatch reaches the operator as error_locked, not 422",
			Hint: "if the invariant is expressible over input.* alone, use the top-level validate: — it answers 422 on " +
				"both paths; if it genuinely needs the roster, this is the intended behaviour, and the same scenario " +
				"started as an EXPLICIT run over an already-bound roster does get the 422",
			YAMLPath: fmt.Sprintf("$.tasks[%d].assert", i),
		}
		if tok := tasksNode.Values[i].GetToken(); tok != nil {
			d = diagAt(tok.Position.Line, tok.Position.Column, d)
		}
		out = append(out, d)
	}
	return out
}
