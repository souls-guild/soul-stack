package errand

import (
	"errors"
	"fmt"

	"github.com/souls-guild/soul-stack/shared/coremanifest"
)

// Keeper-side admission for the ONE dry-run rejection keeper can make on its own
// (ADR-033 contract row, NIM-489).
//
// The `dry_run` row of ADR-033 has always said a verb module under dry_run is
// refused; what shipped was the Soul-side half — [errandrunner.IsAllowed] answers
// `FAILED errand_dry_run_unsupported` per target. The keeper-side 400 the same row
// promised was never written, so an operator asking for a preview of
// `core.cmd.shell` got HTTP 200 carrying a per-target failure. The Voyage path was
// wrong the other way round — the flag never left keeper (NIM-559, fixed in the same
// change), so the command line ran for real on every host and no dry_run refusal
// appeared at all. Threading the flag is what turns that request into one identical,
// unavoidable failure per resolved host, and this rule answers it at the request
// instead.
//
// The boundary is deliberate and narrow. Keeper may refuse only what keeper KNOWS:
//
//   - the verb-shell set is a keeper-side constant ([coremanifest.VerbShellModules],
//     shared with the console gate) — `core.cmd.shell` and `core.exec.run` take an
//     arbitrary command line and have no pure-read Plan, so dry_run on them cannot
//     succeed on ANY host, in any fleet, at any version. That is a fact about the
//     request, so it is answered at the request.
//   - whether an arbitrary module exists on a given Soul, and whether its Plan is
//     declared pure-read ([sdkmodule.PlanReadSafe]) — keeper does not know and must
//     not: module presence and its markers live on the far side of the isolation
//     boundary (ADR-011), and the answer differs per host. Those stay per-target
//     `failed` / `errand_dry_run_unsupported`, decided by the runner.
//
// The rule is NOT keyed on the module catalogue's `errand_safe` flag. That flag
// describes the APPLY path (ErrandReadSafe) and since NIM-488 marks very nearly the
// COMPLEMENT of what dry_run admits — gating on it would reject `core.file.present`
// and the other twelve PlanReadSafe modules, i.e. exactly the modules dry_run
// exists for, while still admitting the shell.
//
// One decision, two call sites: [Dispatcher.Dispatch] (which covers REST
// `/v1/souls/{sid}/exec` and the MCP tool at once, before any errands row or audit
// event exists) and Voyage `kind=command` creation (which refuses at create time
// instead of fanning out a fleet of identical failures).

// ErrDryRunVerbShell — dry_run was requested for a verb-shell module. Not a
// capability problem and not a per-host outcome: the pair cannot succeed anywhere,
// so it is a client error (400) rather than one of the 409 capability sentinels in
// soulcompat.go. Match with [errors.Is]; for the operator-facing message use
// [DryRunVerbShellError.Detail], which names the module.
var ErrDryRunVerbShell = errors.New("errand: dry_run is not supported for a verb-shell module")

// DryRunVerbShellError is the refusal carrying the module that caused it, so a
// transport mapper can name it without re-deriving it from the request (each of the
// three surfaces carries it in a request type of its own). Unwraps to
// [ErrDryRunVerbShell].
type DryRunVerbShellError struct {
	Module string
}

func (e *DryRunVerbShellError) Error() string {
	return fmt.Sprintf("%s: %s", ErrDryRunVerbShell.Error(), e.Module)
}

func (e *DryRunVerbShellError) Unwrap() error { return ErrDryRunVerbShell }

// Detail is the operator-facing explanation, shared by REST, MCP and Voyage so one
// wording is maintained. It names the module and the reason, and states the two ways
// forward — "unsupported" alone leaves the operator guessing whether to wait, retry,
// or upgrade something.
func (e *DryRunVerbShellError) Detail() string {
	return "module " + e.Module + " is a verb-shell module: it runs an arbitrary command line and has no pure-read " +
		"Plan, so 'dry_run' cannot be honored on any host (the Soul would answer " +
		coremanifest.ReasonDryRunUnsupported + " per target). Drop 'dry_run' to run the command for real, or preview " +
		"a module whose Plan is declared pure-read"
}

// ValidateDryRunModule is the whole keeper-side rule: refuse dry_run on a verb-shell
// module, pass everything else through. dryRun=false is never refused here — the
// Apply path admits verb-shell by name (ADR-033 item 2). nil means admitted.
//
// The concrete return type is deliberate, not an oversight of the "return error"
// convention: it is what keeps the two entry points from drifting. Both need the module
// name to word the refusal, and a bare `error` would make each of them either re-derive
// it or carry an unreachable fallback branch for a second error kind that does not
// exist. If a second keeper-side rejection is ever added here, this signature has to
// change and every call site fails to compile — which is the intended alarm, since a
// silently-dropped second rule would read exactly like an admitted request.
func ValidateDryRunModule(module string, dryRun bool) *DryRunVerbShellError {
	if !dryRun || !coremanifest.IsVerbShell(module) {
		return nil
	}
	return &DryRunVerbShellError{Module: module}
}
