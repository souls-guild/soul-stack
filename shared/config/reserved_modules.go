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

// reservedModuleAddr reports whether a module dependency claims a reserved name at
// address level 1 — `core` as much as `core.file`, since NIM-829 made the bare alias
// the canonical way to write the entry and a rule that only saw the dotted form would
// wave the shorter spelling of the same claim through.
//
// It is only ever asked about the two lists that NAME a module (`modules[]`,
// `required_modules[]`); a destiny dependency is a single-level name that means
// something else entirely, and its caller does not route it here.
func reservedModuleAddr(addr string) bool {
	alias, ok := ModuleAlias(addr)
	return ok && plugin.IsReserved(alias)
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
