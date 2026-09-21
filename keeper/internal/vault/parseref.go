package vault

import (
	"errors"
	"fmt"
	"strings"
)

// ErrInvalidVaultRef means the string doesn't match the
// `vault:<mount>/<path>` format. The sentinel lets calling code distinguish
// "format is broken" from Vault transport errors.
var ErrInvalidVaultRef = errors.New("vault: invalid ref format (expected vault:<mount>/<path>)")

// ParseRef parses a `vault:<mount>/<rel>` string into the logical path
// (`<mount>/<rel>`) expected by [Client.ReadKV].
//
// Used by all consumers of `*_ref` fields in `keeper.yml`
// (`postgres.dsn_ref`, `auth.jwt.signing_key_ref`, etc.) for uniform
// normalization. A leading `/` after `vault:` is allowed (`vault:/secret/...`).
//
// # Logical path normalization (security invariant)
//
// The returned logical path is normalized: repeated slashes (`//`) are
// collapsed into one. This is the single normalization point for BOTH
// resolution channels — authoring (render.readVaultRef) and operator-input
// (scenario.input_vault) — so scope-match / deny-list / [Client.ReadKV]
// always work off one canonical value. Without this, an unnormalized path
// could bypass the hard deny-list: `secret//keeper/x` didn't match the
// `secret/keeper/` prefix, but ReadKV collapsed it to the forbidden
// `secret/keeper/x` (operator→Vault escalation).
//
// `.` and `..` segments are REJECTED as an invalid ref (not silently
// normalized): `..` climbs above the mount, has no meaning for Vault KV
// semantics, and is a scope-bypass vector. Case is left untouched — Vault
// paths are case-sensitive.
//
// Examples:
//
//	"vault:secret/keeper/postgres"         → "secret/keeper/postgres"
//	"vault:secret/keeper/jwt-signing-key"  → "secret/keeper/jwt-signing-key"
//	"vault:/secret/keeper/k"               → "secret/keeper/k"
//	"vault:secret//keeper/x"               → "secret/keeper/x"
//	"vault:secret/./keeper/x"              → error (segment `.`)
//	"vault:secret/keeper/../keeper/x"      → error (segment `..`)
//
// Any deviation from the format (no `vault:` prefix, empty body, no `/`
// separator between the mount and rel part, trailing slash with no rel,
// `.`/`..` segment) returns a wrapped [ErrInvalidVaultRef].
func ParseRef(ref string) (string, error) {
	const prefix = "vault:"
	if !strings.HasPrefix(ref, prefix) {
		return "", fmt.Errorf("%w: got %q", ErrInvalidVaultRef, ref)
	}
	body := strings.TrimPrefix(ref, prefix)
	body = strings.TrimPrefix(body, "/")
	if body == "" {
		return "", fmt.Errorf("%w: empty path in %q", ErrInvalidVaultRef, ref)
	}
	slash := strings.Index(body, "/")
	if slash <= 0 || slash == len(body)-1 {
		return "", fmt.Errorf("%w: missing <mount>/<path> in %q", ErrInvalidVaultRef, ref)
	}
	normalized, err := normalizeLogical(body)
	if err != nil {
		return "", fmt.Errorf("%w: %v in %q", ErrInvalidVaultRef, err, ref)
	}
	return normalized, nil
}

// normalizeLogical collapses repeated slashes in the logical path
// (`mount/rel`) and rejects `.`/`..` segments (scope/deny bypass).
// Equivalent to path.Clean on the rel part plus an explicit `..` ban, but
// without depending on path semantics (path.Clean silently collapses
// `a/../b`→`b`, while we need to REJECT `..`, not "fix" the operator's
// path). Case and the segments themselves are left unchanged.
func normalizeLogical(body string) (string, error) {
	segments := strings.Split(body, "/")
	out := segments[:0]
	for _, seg := range segments {
		switch seg {
		case "":
			// repeated slash (`//`) — collapse it, drop the segment.
			continue
		case ".", "..":
			return "", fmt.Errorf("segment %q is not allowed in vault path", seg)
		default:
			out = append(out, seg)
		}
	}
	if len(out) < 2 {
		return "", fmt.Errorf("normalization left no <mount>/<path>")
	}
	return strings.Join(out, "/"), nil
}
