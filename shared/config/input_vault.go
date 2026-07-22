package config

// Scoped resolution of a `vault:` ref in operator input (docs/input.md →
// "vault_scope").
//
// Trust boundary: an operator-supplied secret field value may be a `vault:` ref.
// Resolving any such ref would let the operator read ANY path of the Keeper's
// Vault token (including `secret/keeper/jwt-signing-key`) — an escalation. So
// input-vault-ref resolution is allowed only for a field with a declared
// `vault_scope` (prefix-glob) and only if the resolved path matches the scope AND
// is not in the hard deny-list (a safety net against a scope authoring mistake).
//
// The single-ref check (scope-match + deny) is a pure function of strings, so it
// lives in shared/config (Soul-safe, no vault client). KV reads and audit are
// keeper-side (keeper/internal/scenario), where this floor is wired in.

import "strings"

// VaultInputFloor — system-floor hard deny-list (fork C): paths under these
// prefixes are NEVER resolved via operator input, even if a field author
// mistakenly declared a `vault_scope` covering them. Checked AFTER the scope match,
// always, unconditionally. Extended by config (keeper.yml → vault.input_deny_paths)
// but not disabled by it — the system-floor cannot be turned off.
var VaultInputFloor = []string{
	"secret/keeper/",
	"secret/internal/",
}

// validVaultScopeGlob — the prefix-glob form for `vault_scope`: one trailing `*`
// preceded by a non-empty logical prefix like `<mount>/<path>` (has a `/`
// separating mount and path). Without `/` there's no mount narrowing — rejected as
// meaningless. Without a trailing `*` it's an exact path, not a prefix; that's
// allowed too (exact match), but at least `<mount>/<leaf>` is required.
func validVaultScopeGlob(scope string) bool {
	if scope == "" {
		return false
	}
	prefix := strings.TrimSuffix(scope, "*")
	// after stripping `*`, `<mount>/<...>` must remain — a `/` not at the start.
	slash := strings.Index(prefix, "/")
	return slash > 0
}

// MatchesVaultScope — a Vault KV logical path (`<mount>/<rel>`, without the
// `vault:` prefix and without the `#field` suffix) falls within the `vault_scope`
// prefix-glob.
//
// Glob semantics — a single trailing `*` = prefix match; without `*` an exact
// match. Interior `*` is unsupported (MVP: one prefix-glob).
func MatchesVaultScope(scope, logical string) bool {
	if scope == "" {
		return false
	}
	if strings.HasSuffix(scope, "*") {
		return strings.HasPrefix(logical, strings.TrimSuffix(scope, "*"))
	}
	return logical == scope
}

// DeniedByVaultFloor — a logical path falls under the hard deny-list (system-floor
// + optional config extension extra). Checked AFTER the scope match,
// unconditionally. extra may be nil (system-floor only).
func DeniedByVaultFloor(logical string, extra []string) bool {
	for _, p := range VaultInputFloor {
		if strings.HasPrefix(logical, p) {
			return true
		}
	}
	for _, p := range extra {
		if p != "" && strings.HasPrefix(logical, p) {
			return true
		}
	}
	return false
}
