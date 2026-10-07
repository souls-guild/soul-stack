package cel

import (
	"regexp"
	"strings"

	"github.com/google/cel-go/common/types"
	"github.com/google/cel-go/common/types/ref"
)

// stringType — the target type for canonical stringification of CEL values when
// concatenating in interpolation ([templating.md §5]).
var stringType ref.Type = types.StringType

// Base CEL functions — from the google/cel-go standard library wired in via
// cel.StdLib() in [New]. The ones actually used by Soul Stack expressions
// ([templating.md §2.3]):
//
//   - size(x)            — size of a string/list/map.
//   - contains(s, sub)   — substring/membership (receiver-form method).
//   - timestamp/duration — time arithmetic.
//
// All are pure: no I/O, network, sleep.
//
// Soul Stack custom functions each live in their own file and are registered via
// additional EnvOptions:
//
//   - glob(pattern)      — [glob.go], pattern matching.
//   - vault(path)        — [vault.go], keeper-side Vault KV read (macro, registered
//                          only when the Engine has a KVReader).
//   - merge(m, m...)     — [merge.go], SHALLOW last-wins merge of maps
//                          ([ADR-010 Amendment 2026-06-22]).
//
// Extending the custom-function list goes through an ADR, not silently
// ([templating.md §2.3]).
//
// now() from the starter minimum [templating.md §2.3] is not provided by cel-go as a
// global function (eval-time time comes via timestamp literals and context
// variables). Introducing now() as a custom function is outside pilot scope and needs
// a decision on eval-time semantics; until then now(...) is rejected by the guard as
// unsupported.

// unsupportedPatterns — constructs declared by the spec but NOT in pilot scope.
// Rejected before compile to return a meaningful [ErrUnsupported] instead of an
// opaque CEL "no such field/overload".
//
// soulprint.hosts / .where(...) are no longer here: they are implemented by a
// compile-time AST rewrite of a static literal predicate into a native
// filter-comprehension (see hosts.go); destiny-pass isolation and receiver/literal
// validation are done there too, not by a text guard.
//
// vault( is no longer here unconditionally: with an Engine that has a KVReader
// ([WithVault]) the vault() function is registered and works ([vault.go]); its guard
// remains only when no KVReader is set (see vaultGuard in [guardUnsupported]) — to
// give a meaningful ErrUnsupported instead of "no such function" in Vault-less
// contexts.
//
// Each pattern catches the construct's characteristic token:
//   - now(                   — eval-time time (see above).
var unsupportedPatterns = []struct {
	feature string
	re      *regexp.Regexp
}{
	{"now()", regexp.MustCompile(`\bnow\s*\(`)},
}

// vaultGuard catches a vault() call — rejected only when the Engine is built without a
// KVReader (vault() is not registered, see [guardUnsupported]).
var vaultGuard = regexp.MustCompile(`\bvault\s*\(`)

// generateSecretGuard catches a generate_secret() call — rejected in the passes where
// the function is not registered ([ADR-0083] §3: migration, flow-control, service
// vars). Without it the author gets a bare undeclared-reference; with it, a stated
// reason for a call that would otherwise have produced a marker nobody resolves.
var generateSecretGuard = regexp.MustCompile(`\bgenerate_secret\s*\(`)

// guardUnsupported returns [ErrUnsupported] if the expression contains a construct
// outside pilot scope. vaultEnabled=true (Engine with a KVReader) lifts the vault()
// guard — the function is registered and works; genSecretEnabled=true (the ordinary
// scenario/destiny pass) lifts the generate_secret() guard the same way. vars is NOT rejected by the guard:
// it's declared as a variable and resolved from Vars.Vars (the flat namespace: the
// service's own vars under the destiny/task locals); an empty map gives the normal
// no-such-key, not a panic.
func guardUnsupported(expr string, vaultEnabled, genSecretEnabled bool) error {
	for _, p := range unsupportedPatterns {
		if p.re.MatchString(expr) {
			return &ErrUnsupported{Expr: expr, Feature: p.feature}
		}
	}
	if !vaultEnabled && vaultGuard.MatchString(expr) {
		return &ErrUnsupported{Expr: expr, Feature: "vault(...)"}
	}
	if !genSecretEnabled && generateSecretGuard.MatchString(expr) {
		return &ErrUnsupported{Expr: expr, Feature: "generate_secret(...) (only the scenario/destiny render pass can request a secret)"}
	}
	return nil
}

// normalize brings an expression to a canonical form for the compile-cache key:
// collapses internal whitespace runs and trims the edges. Doesn't change CEL semantics
// (whitespace outside string literals is insignificant); string literals in Soul Stack
// expressions use single quotes ([templating.md §2.2]), and whitespace inside them is
// preserved as-is — so normalization touches only whitespace, not literal contents.
func normalize(expr string) string {
	return spaceRun.ReplaceAllStringFunc(strings.TrimSpace(expr), normalizeWhitespace)
}

var spaceRun = regexp.MustCompile(`'[^']*'|"[^"]*"|\s+`)

func normalizeWhitespace(s string) string {
	if s == "" {
		return s
	}
	switch s[0] {
	case '\'', '"':
		return s // string literal — leave untouched
	default:
		return " "
	}
}
