package errandrunner

import (
	"fmt"

	sdkmodule "github.com/souls-guild/soul-stack/sdk/module"
	"github.com/souls-guild/soul-stack/shared/coremanifest"
)

// IsAllowed checks whether the module mod, addressed by fullName
// (`<namespace>.<name>.<state>`), is safe to invoke via Errand. Returns
// (ok, reason). reason is always `errand_module_not_allowed: <module>` for
// format compatibility with the error codes in [docs/naming-rules.md].
//
// Algorithm (ADR-033 §2):
//  1. Verb-shell modules ([coremanifest.IsVerbShell] — shell/exec, imperative
//     by-design), allowed by name WITHOUT a marker check.
//  2. [sdkmodule.ErrandReadSafe] marker — the module declares itself "safe
//     for ad-hoc invocation" (BaseModule does NOT implement this interface,
//     so a custom plugin on BaseModule defaults to deny).
//  3. Otherwise reject.
//
// The former private copy of the shell/exec list now lives in
// [coremanifest.VerbShellModules] — the same set the Keeper's console gate reads
// (ADR-0074 amendment, NIM-197), so the runner and the gate cannot drift apart.
//
// nil-mod — defensive reject (caller must call after Lookup; this is a
// belt-and-suspenders check).
func IsAllowed(fullName string, mod sdkmodule.SoulModule) (bool, string) {
	if coremanifest.IsVerbShell(fullName) {
		return true, ""
	}
	if mod == nil {
		return false, fmt.Sprintf("errand_module_not_allowed: %s", fullName)
	}
	if _, ok := mod.(sdkmodule.ErrandReadSafe); ok {
		return true, ""
	}
	return false, fmt.Sprintf("errand_module_not_allowed: %s", fullName)
}
