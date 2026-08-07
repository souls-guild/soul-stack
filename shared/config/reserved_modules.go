package config

// Reserved address level 1, on the two surfaces where a definition NAMES a module it
// wants fetched: `service.yml → modules[]` and `destiny.yml → required_modules[]`.
//
// Before NIM-377 the namespace was DECLARED BY THE ARTIFACT, so `core` was unforgeable:
// no publisher could ship a plugin claiming it without the manifest saying so out loud.
// The artifact no longer carries a name. Address level 1 is the alias an operator picks
// at registration, which means `core.file.present` in a task is a string that a badly
// chosen registration could point at somebody else's binary.
//
// The closed list lives in `shared/plugin` and is checked at both ends of an alias's
// life — at registration, where it is the control, and here, where it is the cheap
// early word to the author. A second copy of the list is how the two ends drift apart,
// so there is exactly one.
//
// This end is NOT the control. An author's definition passing the check says nothing
// about which artifact a cluster actually registered under that alias; it only means
// the definition did not ask for a name no honest registration can hold.

import (
	"fmt"
	"strings"

	"github.com/goccy/go-yaml/ast"

	"github.com/souls-guild/soul-stack/shared/diag"
	"github.com/souls-guild/soul-stack/shared/plugin"
)

// reservedModuleAddr reports whether a two-level `<namespace>.<module>` address names a
// reserved namespace at level 1.
//
// A single-level name (a destiny, `redis`) is never a module address and is left alone —
// the same helper is on the path that validates both lists.
func reservedModuleAddr(addr string) bool {
	ns, _, twoLevel := strings.Cut(addr, ".")
	return twoLevel && plugin.IsReserved(ns)
}

// reservedModuleDiag renders the finding for an address [reservedModuleAddr] rejected.
// label is the human location (`modules[0].name`, `required_modules[2]`).
//
// `core` keeps its own code and sentence: it is the one reserved name an author reaches
// for by accident rather than by collision, and "core modules are always available"
// answers the question they actually had. Every other reserved name is a different
// mistake — the name cannot be registered at all — and says so.
func reservedModuleDiag(root *ast.MappingNode, yamlPath, label, addr string) diag.Diagnostic {
	ns, _, _ := strings.Cut(addr, ".")
	if ns == "core" {
		return atPath(root, yamlPath, diag.Diagnostic{
			Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
			Code:    "core_module_in_modules_list",
			Message: fmt.Sprintf("%s %q is a core module — core modules are always available and must not be listed", label, addr),
			Hint:    "Core modules are available automatically - not listed in `modules:` (ADR-009)",
		})
	}
	return atPath(root, yamlPath, diag.Diagnostic{
		Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
		Code:    "reserved_module_namespace",
		Message: fmt.Sprintf("%s %q claims the reserved name %q at address level 1", label, addr, ns),
		Hint: "no plugin can be registered under a reserved name (NIM-377); reserved: " +
			strings.Join(plugin.ReservedNames(), ", "),
	})
}
