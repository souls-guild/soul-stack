package coremanifest

import "sort"

// verbShellModules is the closed set of core modules whose declared input IS an
// arbitrary command line: `core.cmd.shell` (a shell string through `sh -c`) and
// `core.exec.run` (argv without a shell). Both run as the Soul daemon's user,
// typically root.
//
// This is the SINGLE source of the fact for the whole repo (NIM-197). Two
// consumers derive from it and must not keep private copies:
//
//   - the Soul-side Errand runner's hardcoded allow-list (ADR-033 §2,
//     soul/internal/runtime/errandrunner) — "may an Errand call this module at all";
//   - the Keeper-side console gate (ADR-0074 amendment, keeper/internal/shellgate)
//     — "does reaching this module additionally require `soul.console`".
//
// Keeping the list in one place is what makes the second gate real: it lives on
// the Keeper, the allow-list lives on the Soul, and a divergence would leave the
// gate authorizing a set of modules the runner no longer matches.
//
// Placement in `shared/coremanifest` follows the package's existing role — a
// neutral static fact about core modules that both `soul` and `keeper` import
// without importing each other (ADR-011).
//
// Keys are the FULL address `<namespace>.<name>.<state>`: the state matters
// (`core.cmd.shell` carries a command line, a hypothetical `core.cmd.foo` is a
// different contract and is not covered).
var verbShellModules = map[string]struct{}{
	"core.cmd.shell": {},
	"core.exec.run":  {},
}

// IsVerbShell reports whether fullName (`<namespace>.<name>.<state>`) addresses a
// module that carries an arbitrary command line. Exact match, no prefix logic.
func IsVerbShell(fullName string) bool {
	_, ok := verbShellModules[fullName]
	return ok
}

// VerbShellModules returns the full addresses of the verb-shell modules in
// deterministic (lexicographic) order — for diagnostics, docs and consistency
// tests. The returned slice is a fresh copy; mutating it does not affect the set.
func VerbShellModules() []string {
	out := make([]string, 0, len(verbShellModules))
	for k := range verbShellModules {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
