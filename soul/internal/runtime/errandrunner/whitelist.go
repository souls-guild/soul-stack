package errandrunner

import (
	"fmt"
	"strings"

	sdkmodule "github.com/souls-guild/soul-stack/sdk/module"
	"github.com/souls-guild/soul-stack/shared/coremanifest"
)

// ReasonDryRunUnsupported — reject reason for a dry_run Errand on a module
// whose Plan is not declared pure-read. Distinct from the
// `errand_module_not_allowed` family because the caller maps it to a DIFFERENT
// terminal status (FAILED, not MODULE_NOT_ALLOWED) — see [Runner.Run].
const ReasonDryRunUnsupported = "errand_dry_run_unsupported"

// IsAllowed checks whether the module mod, addressed by fullName
// (`<namespace>.<name>.<state>`), may be invoked via Errand on the path
// selected by dryRun. Returns (ok, reason).
//
// The two paths have DIFFERENT admission conditions, because they invoke
// DIFFERENT module methods. This mirrors the ADR-033 contract row for
// `dry_run`: "only for modules with PlanReadSafe; for verb modules
// (shell/run/probe) the Soul answers FAILED errand_dry_run_unsupported".
//
// dryRun=false — Apply is called (ADR-033 §2):
//  1. verb-shell modules ([coremanifest.IsVerbShell] — shell/exec, imperative
//     by design), allowed by name WITHOUT a marker check;
//  2. `core.http.probe` — the read-only state of a mixed module whose sibling
//     `core.http.request` mutates and therefore cannot use a module-wide marker;
//  3. [sdkmodule.ErrandReadSafe] — the module declares its APPLY safe for
//     ad-hoc invocation (BaseModule does NOT implement this interface, so a
//     custom plugin on BaseModule defaults to deny);
//  4. otherwise reject.
//
// dryRun=true — Plan is called and Apply is NOT reached. The condition is
// [sdkmodule.PlanReadSafe]: the module declares its Plan a genuine pure read
// (ADR-031 Scry). ErrandReadSafe is deliberately NOT consulted here — it is a
// statement about Apply, and asking it on a path that never calls Apply is
// what made `dry_run` unreachable for every module (NIM-488). Verb-shell
// modules get no by-name pass either: they have no pure-read Plan, so dry_run
// on shell/exec is refused, exactly as the ADR states.
//
// The Apply path is unchanged by NIM-488 and stays the ONLY way to reach
// Apply: a module carrying just PlanReadSafe (core.file and 12 others) is
// still rejected whenever dryRun is false — including when the client omits
// the field, since proto3 bool defaults to false.
//
// The former private copy of the shell/exec list now lives in
// [coremanifest.VerbShellModules] — the same set the Keeper's console gate reads
// (ADR-0074 amendment, NIM-197), so the runner and the gate cannot drift apart.
//
// nil-mod — defensive reject (caller must call after Lookup; this is a
// belt-and-suspenders check). It yields the `errand_module_not_allowed`
// reason on both paths: a module that isn't there is not a dry-run capability
// gap.
func IsAllowed(fullName string, mod sdkmodule.SoulModule, dryRun bool) (bool, string) {
	if dryRun {
		if mod == nil {
			return false, notAllowedReason(fullName)
		}
		if _, ok := mod.(sdkmodule.PlanReadSafe); ok {
			return true, ""
		}
		return false, ReasonDryRunUnsupported
	}

	if coremanifest.IsVerbShell(fullName) {
		return true, ""
	}
	if mod == nil {
		return false, notAllowedReason(fullName)
	}
	// ErrandReadSafe is module-wide, while core.http now deliberately mixes a
	// read-only probe with a mutating request. Admit the exact safe state, then
	// default-deny every sibling BEFORE consulting the marker. This structural
	// boundary means accidentally re-adding a module-wide marker can never open
	// request (or a future core.http state) to ad-hoc mutation.
	if fullName == "core.http.probe" {
		return true, ""
	}
	if strings.HasPrefix(fullName, "core.http.") {
		return false, notAllowedReason(fullName)
	}
	if _, ok := mod.(sdkmodule.ErrandReadSafe); ok {
		return true, ""
	}
	return false, notAllowedReason(fullName)
}

// notAllowedReason keeps the `errand_module_not_allowed: <module>` shape
// required for format compatibility with the error codes in
// [docs/naming-rules.md].
func notAllowedReason(fullName string) string {
	return fmt.Sprintf("errand_module_not_allowed: %s", fullName)
}
