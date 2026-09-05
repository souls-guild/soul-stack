package trial

// What a trial RUN carries beyond the cases themselves, and what it brings back
// besides pass/fail.
//
// Both halves exist for NIM-790. L0 loaded every definition with empty
// [config.ValidateOptions] and expanded every `include:` with no manifest
// resolver, then filtered the diagnostics of both to ERRORS before doing anything
// with them (`hasErrors`, `formatDiags`). So a case with a plugin step went green
// over `params:` nobody had checked, and the `plugin_params_unchecked` hint that
// exists to say exactly that was produced, discarded, and never printed.
//
// That is the fourth tool of this shape — after `validate-scenario` (NIM-779),
// `validate-destiny` (NIM-783) and the keeper (NIM-785, still open). The wiring
// now comes from [definition], shared by all of them; what stays here is the
// channel a hint travels to the report on, because a harness that reports
// pass/fail had nowhere to put a finding that is neither.

import (
	"github.com/souls-guild/soul-stack/shared/config"
	"github.com/souls-guild/soul-stack/shared/diag"
)

// Options are the run-wide inputs of a trial, held apart from the case files: a
// case describes a definition, these describe what the runner was given to check
// it WITH.
type Options struct {
	// Modules resolves the manifest of a plugin module, from the
	// `--modules <alias>=<path>` bindings of this run ([definition.LoadSchemas]).
	//
	// nil is the ordinary case and is not silence: every plugin step then reports
	// [config.DiagPluginParamsUnchecked] as a notice on its case. The keeper
	// resolves the same interface from its Sigil grants, which an offline runner
	// does not have — that asymmetry is the whole reason the flag exists.
	Modules config.ModuleManifestResolver
}

// notices collects the non-error diagnostics of loading a case's definition.
//
// A trial Result is pass/fail, and these are neither: `plugin_params_unchecked`
// does not make a case wrong, it says a part of it was never judged. Dropping them
// for that reason is what NIM-790 is, so they travel to the report as themselves,
// at their own level and file, and the report prints them under a PASS.
//
// Deduplicated by whole-diagnostic identity, not by code or by file. One body
// included twice, or one destiny resolved by two `apply:destiny` steps, is loaded
// twice and produces the same finding twice; two findings that differ in ANY field
// are two things to look at and both are kept.
type notices struct {
	seen map[diag.Diagnostic]bool
	out  []diag.Diagnostic
}

// keep takes the non-error half of a definition's verdict. Errors are the
// caller's: in L0 they abort the case with a message, which is a louder channel
// than this one and must not be duplicated on it.
func (n *notices) keep(diags []diag.Diagnostic) {
	for _, d := range diags {
		if d.Level == diag.LevelError {
			continue
		}
		if n.seen == nil {
			n.seen = map[diag.Diagnostic]bool{}
		}
		if n.seen[d] {
			continue
		}
		n.seen[d] = true
		n.out = append(n.out, d)
	}
}

// list is the collected notices in the order they were first produced.
func (n *notices) list() []diag.Diagnostic { return n.out }
