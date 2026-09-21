package plugin

import (
	"regexp"
	"sort"
	"strings"
)

// Reserved registration aliases (NIM-377 §7, absorbing NIM-375).
//
// An alias is address level 1 — the `redis` in `redis.acl.present` — and since
// NIM-377 it is chosen by the OPERATOR at registration rather than declared by the
// artifact. That is what makes a reserved list necessary: the artifact used to carry
// its own namespace, so a publisher could not name themselves `core` without saying
// so out loud; now anyone with `plugin.allow` can pick the word, and an address that
// reads `core.file.present` must never be able to mean a third-party plugin.
//
// One closed list, checked at BOTH ends of the alias's life:
//   - at registration ([keeper/internal/sigil].Service.Allow, the git resolver's
//     catalog pass) — the alias never enters the cluster;
//   - in a destiny's `required_modules:` ([soul-lint]) — an author naming a reserved
//     alias is told offline, before anything is granted.
//
// Both callers reach the same [IsReserved]; a second copy of the list is how the two
// ends drift apart, and a gap between them is precisely the shadowing this prevents.

// AliasPattern is the wire/CLI shape of a registration alias: lowercase kebab-case,
// starting with a letter, at most 63 characters.
//
// The alias names a directory in the host cache and a level of an address, so its
// charset is the intersection of what a path segment and an address segment may hold:
// no dots (they separate address levels), no slashes or `..` (path traversal), no
// uppercase (a case-insensitive filesystem would fold two registrations into one
// slot).
const AliasPattern = `^[a-z][a-z0-9-]{0,62}$`

var reAlias = regexp.MustCompile(AliasPattern)

// ValidAlias reports whether name is a well-formed registration alias
// ([AliasPattern]). It says nothing about the name being available — combine with
// [IsReserved].
func ValidAlias(name string) bool { return reAlias.MatchString(name) }

// reservedAliases — the closed set, as agreed in NIM-377's brief.
//
// Tier 1 (mandatory) is the three engine namespaces: an address starting with one of
// them already means something the engine itself serves, and a plugin answering to it
// would shadow the built-in registry.
//
// Tier 2 is the Soul Stack dictionary (docs/naming-rules.md) plus the four words every
// ecosystem eventually collides on (`local`, `default`, `internal`, `test`, `example`).
// `herald` and `provider` joined it in NIM-706: they are dictionary entities on the same
// footing as the rest, and the platform writes Vault path families under both
// (keeper/internal/secretwrite), which is why they are ALSO refused as service names —
// see [shared/config.IsReservedVaultNamespace], a separate and deliberately narrower
// list, because a service name that collides destroys a secret whereas an alias that
// collides only shadows an address.
// These are not shadowing risks today — they are reserved because an operator reading
// `scenario.something.present` in a diff would reasonably assume it came from the
// engine, and because taking a dictionary word back later is a breaking rename.
var reservedAliases = map[string]struct{}{
	// Tier 1 — engine namespaces.
	"core":   {},
	"keeper": {},
	"soul":   {},

	// Tier 2 — the dictionary and the usual collisions.
	"destiny":     {},
	"herald":      {},
	"provider":    {},
	"scenario":    {},
	"service":     {},
	"incarnation": {},
	"soulprint":   {},
	"coven":       {},
	"archon":      {},
	"sigil":       {},
	"soulstack":   {},
	"soul-stack":  {},
	"local":       {},
	"default":     {},
	"internal":    {},
	"test":        {},
	"example":     {},
}

// IsReserved reports whether name is on the closed reserved list.
//
// The comparison folds case and trims surrounding space before looking up: a
// registration path that accepted `Core ` while rejecting `core` would hand an
// attacker the shadowing back through the door the validator left open. Callers that
// also enforce [ValidAlias] get the strict form anyway; this one is safe on its own.
func IsReserved(name string) bool {
	_, ok := reservedAliases[strings.ToLower(strings.TrimSpace(name))]
	return ok
}

// ReservedNames returns the closed list, sorted, for error hints and catalogs.
func ReservedNames() []string {
	out := make([]string, 0, len(reservedAliases))
	for n := range reservedAliases {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}
