package coremanifest

import (
	"sort"

	"github.com/souls-guild/soul-stack/sdk/schema"
)

// StateModuleAddr is the base address of the state-capture module family
// ([ADR-0084]); the state suffix is the verb, so the base is what an address
// match keys on.
//
// Spelled here rather than in each consumer because three packages need it and
// they must not be able to disagree: `shared/config` folds a capture by it,
// `keeper/internal/stateop` dispatches on it, and this file classifies it.
const StateModuleAddr = Namespace + ".state"

// keeperSideCore is the catalog of core module BASE addresses the Keeper
// executes itself. Everything not listed here is Soul-side — the default, and
// the answer for every plugin address as well ([schema.SideSoul]).
//
// This is the one list, deliberately: the linter refuses `on: keeper` on these
// addresses and the render pipeline routes them keeper-side, and both read it
// from here. A second list would reproduce the drift the comment on
// `keeper/internal/stateop.States` already warns about — a linter rejecting
// what the module accepts, or green-lighting what it refuses — one level up,
// where the symptom is a step dispatched to a host that has no such module.
//
// The two sets are disjoint by construction, which is what makes the address
// sufficient: no core module name appears on both sides. Both halves are
// guarded, and neither guard lives here — `shared/` cannot import either binary:
// `keeper/internal/coremod/side_catalog_guard_test.go` holds this list against a
// fully-wired `coremod.Default()`, so a newly registered keeper-side module
// reddens rather than silently routing to a host, and
// `soul/internal/coremod/side_disjoint_guard_test.go` holds it against the Soul's
// registry from the other direction.
//
// Not every entry has a declaration in [coreModules]: `core.cert` is dispatched
// by the Keeper (`core.cert.registered`/`core.cert.issued`) but ships no schema
// document yet, so its params go unchecked offline. Its SIDE is knowable
// regardless, and getting that wrong is the worse failure of the two — hence a
// catalog of addresses rather than a projection of the declarations.
var keeperSideCore = map[string]struct{}{
	Namespace + ".bootstrap": {}, // ADR-063 — token issue/delivery
	Namespace + ".cert":      {}, // NIM-99 — warrant issue/registration
	Namespace + ".choir":     {}, // ADR-044 — choir voice membership
	Namespace + ".soul":      {}, // ADR-009 — soul registration
	StateModuleAddr:          {}, // ADR-0084 — incarnation state capture
	Namespace + ".vault":     {}, // ADR-017 — Vault KV read/write
}

// SideOf returns the side that executes the core module at base address addr
// (`core.state`, NOT `core.state.set` — split the state suffix off first).
//
// A non-core or unknown address answers [schema.SideSoul]. That is the honest
// answer rather than a third "unknown" value: the caller's question is where to
// route, host-side is where an unrecognised address has always gone, and a
// plugin's own declaration is read from its manifest, not from here.
func SideOf(addr string) schema.Side {
	if _, ok := keeperSideCore[addr]; ok {
		return schema.SideKeeper
	}
	return schema.SideSoul
}

// IsKeeperSide reports whether the core module at base address addr is executed
// by the Keeper. Shorthand for SideOf(addr) == [schema.SideKeeper].
func IsKeeperSide(addr string) bool { return SideOf(addr) == schema.SideKeeper }

// KeeperSideAddrs lists the keeper-side core base addresses in lexicographic
// order — for diagnostics, which must be byte-identical for identical input.
func KeeperSideAddrs() []string {
	out := make([]string, 0, len(keeperSideCore))
	for addr := range keeperSideCore {
		out = append(out, addr)
	}
	sort.Strings(out)
	return out
}
