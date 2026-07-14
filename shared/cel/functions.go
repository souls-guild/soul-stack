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

// internalIdentGuard catches identifiers prefixed with `__` in the AUTHOR's
// expression. The `__` prefix is reserved for internal mechanisms of the CEL layer:
// the vault() macro expands to `__vault_read(path, __vault_resolver)`, where
// `__vault_read`/`__vault_resolver` are hidden arguments unavailable to the author.
//
// WITHOUT this guard the author could write `${ __vault_read('secret/anything',
// __vault_resolver).password }` directly and read ANY path, bypassing the vault()
// macro and the `vault(`-token guard/lint/mask (security blocker). The guard applies
// ALWAYS (with or without a KVReader): a `__` identifier in author text is always an
// error, independent of a vault client.
//
// guardUnsupported runs on the AUTHOR text BEFORE macro expansion
// (`__vault_read`/`__vault_resolver` appear only inside env.Compile), so a legal
// `vault('secret/x')` doesn't hit the guard. A `\w` to the left is NOT allowed (else
// `a__b` would false-fire), `.`/token start are allowed: the Soul Stack vocabulary has
// no legal bare identifiers with `__`.
//
// Matching runs over text WITH STRING LITERALS STRIPPED (see stripStringLiterals): a
// `__` sequence INSIDE a literal is data, not a CEL identifier (e.g. the field
// `__host` in the predicate string soulprint.hosts.where("__host == 'x'"), which can
// call nothing), and is not caught.
var internalIdentGuard = regexp.MustCompile(`(^|\W)__\w`)

// guardUnsupported returns [ErrUnsupported] if the expression contains a construct
// outside pilot scope. vaultEnabled=true (Engine with a KVReader) lifts the vault()
// guard — the function is registered and works. essence is NOT rejected by the guard:
// it's declared as a variable and resolved from Vars.Essence (the effective layer); an
// empty Essence gives the normal no-such-key, not a panic.
func guardUnsupported(expr string, vaultEnabled bool) error {
	for _, p := range unsupportedPatterns {
		if p.re.MatchString(expr) {
			return &ErrUnsupported{Expr: expr, Feature: p.feature}
		}
	}
	if internalIdentGuard.MatchString(stripStringLiterals(expr)) {
		return &ErrUnsupported{Expr: expr, Feature: "идентификатор с префиксом '__' (зарезервирован за internal-механизмами CEL)"}
	}
	if !vaultEnabled && vaultGuard.MatchString(expr) {
		return &ErrUnsupported{Expr: expr, Feature: "vault(...)"}
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

// stringLiteralRe matches a CEL string literal (single/double quotes). Used by
// stripStringLiterals to cut out literal contents before the text guard over
// identifiers — literal contents are data, not CEL tokens.
var stringLiteralRe = regexp.MustCompile(`'[^']*'|"[^"]*"`)

// stripStringLiterals replaces string-literal contents with empty quotes, preserving
// the expression structure outside literals. Needed by guards that scan text for CEL
// identifiers/calls: a token inside a literal (`"__host"`) is data, not an identifier.
// Not for CEL semantics — only for text analysis.
func stripStringLiterals(expr string) string {
	return stringLiteralRe.ReplaceAllStringFunc(expr, func(lit string) string {
		return lit[:1] + lit[len(lit)-1:]
	})
}

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
