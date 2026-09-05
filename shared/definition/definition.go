// Package definition loads the task definition of a scenario or a destiny for a
// tool that intends to report a verdict on it.
//
// # Why this is a package and not four call sites
//
// The `params:` of a plugin module are checked against the module's manifest by
// [config.ValidateOptions.ModuleManifests] — one shared check, in shared/config,
// since NIM-228. What was never shared is the WIRING around it, and the wiring is
// three separate things a caller has to get right:
//
//  1. parse the entry file with the manifest resolver bound;
//  2. expand its `include:` bodies with the SAME resolver, so a step one file deep
//     is checked by the same contract as a step in the entry file;
//  3. keep the non-error diagnostics, because the finding that says "this could
//     not be checked" is a HINT and an error-only filter deletes exactly it.
//
// Four tools attached that wiring one at a time, and three of them were shipped
// missing a piece: `soul-lint validate-scenario` missed (2) and (3) for an
// included body (NIM-779), `validate-destiny` never reached the task file at all
// (NIM-783), `soul-trial` missed all three (NIM-790), and the keeper still misses
// them by an open decision (NIM-785). One of those gaps carried a real defect to
// production: NIM-778 shipped `tls: "true"` — a string where the manifest declares
// a bool — into the WB redis service, past a lint that printed `OK:`.
//
// Four in a row is a property of the shape, not four separate lapses. So the
// wiring lives here, once, and a fifth tool attaches to it by calling one of the
// two functions below instead of assembling the sequence again.
//
// # The rule this package exists to hold
//
// A check that could not run says so. There are three outcomes, never two:
//
//	checked, clean       -> no diagnostic
//	checked, contradicts -> an ERROR ([config] unknown_param, missing_required_param, ...)
//	could not check      -> a HINT ([config.DiagPluginParamsUnchecked])
//
// Silence is the worst of the three because it is indistinguishable from the
// first, and the reader cannot act on it. The severity split is deliberate and
// must not be flattened: `*_unchecked` means something was missing from the CALL
// (nobody bound a manifest; the author of a definition usually cannot produce
// somebody else's plugin's manifest, so failing them would punish the wrong
// party). An ERROR means the definition contradicts a manifest that WAS read.
//
// # What a caller still owns
//
// The include RESOLVER, because the two tools that resolve differently are right
// to: soul-lint reads a service tree on disk, the keeper reads a snapshot, and the
// trial harness reads a fixture directory. This package does not guess a layout.
//
// And, for a scenario, the entry file's own parse — [ExpandScenario] takes an
// already-parsed task list because the manifest carries `input:`/`validate:`/
// `extends:` that each caller resolves its own way (covenant merge, for one). The
// entry file's `params:` are therefore checked only if the caller passed
// `ModuleManifests` to its own parse call, and nothing in the type system enforces
// that.
//
// What stands in for enforcement is a fixture rather than a type:
// [deftest] holds one definition carrying a plugin step on BOTH paths — inline and
// behind an `include:` — and each tool's own test asserts its verdict over it, in
// counts (`×2`, not `≥1`) that separate the two paths. The tools live in three Go
// modules and cannot meet in one test process, so this is agreement by shared
// input and pinned expectation, not by a single test calling all three. A tool that
// stops threading the manifests into its entry parse loses one of the two findings
// and fails its own count.
package definition

import (
	"github.com/souls-guild/soul-stack/shared/config"
	"github.com/souls-guild/soul-stack/shared/diag"
)

// ExpandScenario expands a scenario's `include:` bodies with the plugin manifests
// bound, and returns the flat plan together with EVERY diagnostic the expansion
// produced — not the errors alone.
//
// entry is the path of the file whose include list assembled the plan
// (`scenario/<name>/main.yml`). Cross-file findings carry no file of their own —
// expansion has erased AST positions by the time it compares addresses across the
// flattened plan — and are attributed to it.
//
// modules nil is the ordinary case and is NOT silence: each plugin step in each
// body then yields [config.DiagPluginParamsUnchecked]. It is written as an
// explicit argument, never defaulted, so that a caller that has a resolver and
// does not pass it is visible in a diff.
//
// The tasks come back even when the diagnostics contain errors: a failed
// expansion leaves a TRUNCATED list (the broken branch is dropped), and whether
// that is worth walking is a decision only the caller can make — soul-lint stops
// before stratifying it, the trial harness refuses the case outright. Deciding it
// here would take that judgement away from both.
func ExpandScenario(entry string, tasks []config.Task, resolve config.IncludeResolver, modules config.ModuleManifestResolver) ([]config.Task, []diag.Diagnostic) {
	expanded, diags := config.ExpandIncludesWithModules(tasks, resolve, modules)
	return expanded, attribute(entry, diags)
}

// LoadDestinyTasks parses a destiny's `tasks/main.yml` and expands the bodies it
// includes, both with the plugin manifests bound, and returns the flat plan
// together with EVERY diagnostic.
//
// entry is the path of that task file; src is its bytes. Reading it is the
// caller's, because the two tools disagree about what an absent file MEANS: for
// soul-lint a `destiny.yml` linted away from an artifact root has no task file and
// that is a hint, while for the trial harness a destiny without one cannot be
// rendered and the case aborts. Nothing here should decide that.
//
// [config.ValidateOptions.DestinyTasks] is set on the parse and reaches every
// included body: a destiny is Soul-side by construction, so a keeper-side module
// address anywhere in it can never execute (`keeper_module_in_destiny`, NIM-749).
// The keeper's own loader sets the same flag on the same call — the two have to
// agree about what this file is.
//
// A nil task list is the parse's own signal for parse-fatal (its return
// contract): there is no list to expand, and an include tree derived from one
// would be a plan that never existed. The condition is deliberately NOT
// [diag.HasErrors] — a schema error leaves the list intact, so bailing on severity
// would make the STRICTEST invocation report the LEAST: bind manifests, get
// `unknown_param`, and watch the unresolvable `include:` two lines below it drop
// out of the report it was in before the binding.
func LoadDestinyTasks(entry string, src []byte, resolve config.IncludeResolver, modules config.ModuleManifestResolver) ([]config.Task, []diag.Diagnostic) {
	tasks, diags, _ := config.LoadDestinyTasksFromBytes(entry, src, config.ValidateOptions{
		DestinyTasks:    true,
		ModuleManifests: modules,
	})
	if tasks == nil {
		return nil, attribute(entry, diags)
	}
	expanded, expandDiags := config.ExpandIncludesInDestinyWithModules(tasks, resolve, modules)
	return expanded, append(attribute(entry, diags), attribute(entry, expandDiags)...)
}

// attribute gives every fileless diagnostic the entry point's path, so that a
// finding about the assembled plan is reported somewhere a reader can open. A
// diagnostic that already names a file keeps it: a body's own error belongs at the
// body's own coordinates, wherever the definition was loaded from (NIM-716).
func attribute(entry string, diags []diag.Diagnostic) []diag.Diagnostic {
	for i := range diags {
		if diags[i].File == "" {
			diags[i].File = entry
		}
	}
	return diags
}
