package config

// The Vault path segments the platform owns ([ADR-0083] §7, NIM-706).
//
// A declared secret's path is DERIVED — `<mount>/<service>/<incarnation>/<state-field>`
// — so the moment a name is chosen somewhere else in the system, it decides a Vault
// path. Two of those choices can land the derivation on top of a path that already has
// an owner, and neither owner is told:
//
//   - the SERVICE name. Four writers put a fixed word in the first segment after the
//     mount: keeper's own runtime secrets and sigil keys (`secret/keeper/…`,
//     [ADR-014]), the herald credential writer (`secret/herald/…`) and the provider
//     credential writer (`secret/provider/…`, both keeper/internal/secretwrite). A
//     service registered as `herald` derives into the second one, and
//     secretwrite.WriteString REPLACES a KV entry rather than merging into it, so
//     whichever of the two writes last silently destroys the other's fields.
//   - the STATE FIELD name. Inside a service's own namespace the platform ALSO derives
//     `<mount>/<service>/<incarnation>/tls/{cert,key}` for issued TLS material
//     (keeper/internal/certissue.VaultPath). A collection secret on a state field named
//     `tls` with an element key of `cert` derives the very same path — a collision no
//     service-name rule can reach, because both sides are inside one service.
//
// Both are refused by NAME, at the point the name enters the system, rather than
// detected at derivation time: a derivation-time error surfaces during a run, on the
// operator, for a decision the author made at authoring time.
//
// One closed list per axis and one predicate over it. A second copy of either list is
// how the two ends drift apart, and a gap between them is exactly the collision this
// prevents — the same reasoning [shared/plugin.IsReserved] is built on. The lists are
// closed: adding to one is propose-and-wait plus a PR to docs/naming-rules.md.

import (
	"sort"
	"strings"
)

// The `service_name_reserved` diagnostic code went with the manifest's `name:`
// (NIM-726): it could only be raised against a name the manifest stated. The rule
// itself is unchanged and now has one enforcement point, serviceregistry.validateFields,
// which is where the name is actually minted.

// reservedVaultNamespaces — service names that would derive on top of a path family
// the platform writes under a fixed first segment.
//
// `internal` carries no writer today. It is here because [VaultInputFloor] has denied
// `secret/internal/` since NIM-74 as a reserved-for-the-platform prefix, and a name the
// read side refuses to hand back is a name the write side must refuse to mint under —
// a service allowed to register as `internal` could write secrets it can never reveal.
var reservedVaultNamespaces = map[string]struct{}{
	"keeper":   {},
	"herald":   {},
	"provider": {},
	"internal": {},
}

// reservedStateFields — state field names the platform already derives under inside
// every service's own namespace.
//
// `tls` is keeper/internal/certissue's E3 prefix: `<mount>/<svc>/<inc>/tls/{cert,key}`.
// The refusal covers a scalar secret as well as a collection one. A scalar derives
// `<mount>/<svc>/<inc>/tls`, which KV v2 keeps distinct from `…/tls/cert`, so it is not
// a data collision — but it puts two owners on one prefix, which is the confusion the
// namespace fence exists to remove, and one predicate beats a rule that holds for one
// shape of declaration and not the other.
var reservedStateFields = map[string]struct{}{
	"tls": {},
}

// IsReservedVaultNamespace reports whether name is a service name the platform reserves
// as the first path segment of its own Vault writes.
//
// The comparison folds case and trims surrounding space before looking up: a validator
// that accepted `Herald ` while rejecting `herald` would hand the collision back through
// the door it left open. Callers that also enforce their own name grammar get the strict
// form anyway; this one is safe on its own.
func IsReservedVaultNamespace(name string) bool {
	_, ok := reservedVaultNamespaces[foldReserved(name)]
	return ok
}

// IsReservedStateField reports whether name is a state field the platform already
// derives under within a service's own namespace.
func IsReservedStateField(name string) bool {
	_, ok := reservedStateFields[foldReserved(name)]
	return ok
}

// ReservedVaultNamespaceNames returns the closed service-name list, sorted, for error
// hints and catalogs.
func ReservedVaultNamespaceNames() []string { return sortedSet(reservedVaultNamespaces) }

// ReservedStateFieldNames returns the closed state-field list, sorted, for error hints.
func ReservedStateFieldNames() []string { return sortedSet(reservedStateFields) }

// foldReserved normalises a candidate before the lookup — see [IsReservedVaultNamespace].
func foldReserved(name string) string { return strings.ToLower(strings.TrimSpace(name)) }

func sortedSet(m map[string]struct{}) []string {
	out := make([]string, 0, len(m))
	for n := range m {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// PathUnderReservedNamespace reports whether a logical Vault path lands inside one of
// the reserved namespaces ([reservedVaultNamespaces]) — the read-side half of the same
// rule registration enforces on the name.
//
// The path is compared as the VAULT CLIENT will see it rather than as it was typed, the
// same three rewrites [PathAddressesOwnNamespace] mirrors:
//
//   - the `vault:` marker and a `#<field>` selector are stripped;
//   - empty and `.` segments are dropped (vault.normalizeLogical collapses repeated
//     slashes, Client.relativeKVPath trims a leading one, so `secret//keeper/…` reaches
//     the very entry this refuses while splitting into a segs[1] of "");
//   - the mount may be absent. [vault.Client.ReadKV] documents `<service>/<inc>/…` as an
//     accepted form and substitutes the mount itself, so the reserved word can land in
//     segs[0] as readily as in segs[1]. Both positions are checked, which is also what
//     makes this independent of `vault.kv_mount` — the predecessor of this check spelled
//     the mount literally (`secret/keeper/`) and went blind the moment an operator
//     configured a different one.
//
// Segments are compared WHOLE: a prefix comparison would refuse `secret/keeper-notes/`
// while a namespace gate's one unaffordable mistake is the opposite — letting
// `<mount>/<reserved>-evil/` through as if it were a different namespace. Checking
// segs[0] costs an over-refusal in one configuration, a KV mount named exactly after a
// reserved word, which is the right direction for a floor to be wrong in.
func PathUnderReservedNamespace(logical string) bool {
	logical = strings.TrimPrefix(logical, vaultRefMarker)
	if i := strings.IndexByte(logical, '#'); i >= 0 {
		logical = logical[:i]
	}
	raw := strings.Split(logical, "/")
	segs := raw[:0]
	for _, seg := range raw {
		if seg == "" || seg == "." {
			continue
		}
		segs = append(segs, seg)
	}
	for i, seg := range segs {
		if i > 1 {
			break
		}
		if IsReservedVaultNamespace(seg) {
			return true
		}
	}
	return false
}
