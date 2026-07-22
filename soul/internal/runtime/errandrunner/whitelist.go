package errandrunner

import (
	"fmt"

	sdkmodule "github.com/souls-guild/soul-stack/sdk/module"
)

// hardcodedWhitelist — verb modules, imperative by-design (shell-exec), that
// the Errand runner allows to call by name WITHOUT a marker check
// ([sdkmodule.ErrandReadSafe]). ADR-033 §2: hardcoded list — core.cmd.shell
// and core.exec.run; everything else (including future custom plugins) goes
// through the marker-interface default-deny.
//
// Matched by exact equality of the full address `<ns>.<name>.<state>`: state
// matters (`core.cmd.shell` is allowed, a hypothetical `core.cmd.foo` isn't).
var hardcodedWhitelist = map[string]struct{}{
	"core.cmd.shell": {},
	"core.exec.run":  {},
}

// IsAllowed checks whether the module mod, addressed by fullName
// (`<namespace>.<name>.<state>`), is safe to invoke via Errand. Returns
// (ok, reason). reason is always `errand_module_not_allowed: <module>` for
// format compatibility with the error codes in [docs/naming-rules.md].
//
// Algorithm (ADR-033 §2):
//  1. Hardcoded list (verb modules shell/exec, imperative by-design).
//  2. [sdkmodule.ErrandReadSafe] marker — the module declares itself "safe
//     for ad-hoc invocation" (BaseModule does NOT implement this interface,
//     so a custom plugin on BaseModule defaults to deny).
//  3. Otherwise reject.
//
// nil-mod — defensive reject (caller must call after Lookup; this is a
// belt-and-suspenders check).
func IsAllowed(fullName string, mod sdkmodule.SoulModule) (bool, string) {
	if _, ok := hardcodedWhitelist[fullName]; ok {
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
