package config

import (
	"github.com/souls-guild/soul-stack/shared/coremanifest"
)

// KeeperSideModule reports whether a task's module ADDRESS names a keeper-side
// module — `core.state.set` yes, `core.pkg.present` no. Takes the author-written
// address with its state suffix and splits it, so callers hold one spelling.
//
// The catalog it asks is [coremanifest.IsKeeperSide]; a malformed address is
// not keeper-side (module_format_invalid says so elsewhere, and answering
// "keeper" on garbage would route it into the one executor that cannot report a
// per-host failure).
func KeeperSideModule(addr string) bool {
	name, _, ok := SplitModuleAddr(addr)
	return ok && coremanifest.IsKeeperSide(name)
}

// keeperSide is THE rule, written once: a task executes keeper-side when its
// module says so, or when the legacy `on: keeper` literal is present.
//
// The first half is the rule ([ADR-0084] amendment / NIM-747): the module
// declares the side and the task inherits it, because the two core sets are
// disjoint and the address alone therefore decides. `on:` means "which covens"
// and nothing else on a core address, where the literal is now refused as
// redundant (`on_keeper_redundant`).
//
// The second half is not a gap waiting to close — it is the boundary. A PLUGIN
// address carrying `on: keeper` stays legal because nothing readable FROM HERE
// declares such a plugin's side: the declaration is `side:` in the artifact's
// stamped schema document (NIM-749), which lives in the Keeper's plugin cache
// and not in the service repo this package parses. The Keeper does execute such
// a plugin (NIM-758, closing NIM-688) — it reads that document at dispatch,
// where it has it. Refusing the literal here would remove the only spelling
// those scenarios have and silently route them at hosts.
//
// Written as a pure function over the two facts, rather than once over
// [Task] and again over the YAML AST, because the linter sees a task before it
// is decoded and the render pipeline sees it after. Two spellings of one rule
// is how a linter comes to accept what the engine refuses.
func keeperSide(moduleAddr string, onKeeperLiteral bool) bool {
	return KeeperSideModule(moduleAddr) || onKeeperLiteral
}

// IsKeeperSideTask reports whether a decoded task executes on the Keeper. The
// post-decode half of [keeperSide]; `keeper/internal/render.IsKeeperTask`
// delegates here so routing and linting cannot disagree.
func IsKeeperSideTask(t Task) bool {
	addr := ""
	if t.Module != nil {
		addr = t.Module.Module
	}
	on, _ := t.On.(string)
	return keeperSide(addr, on == KeeperTarget)
}
