package cel

import (
	"regexp"
	"strings"

	"github.com/google/cel-go/common/ast"
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

// The three text patterns below are [guardUnsupportedText]'s, the fallback for text the
// parser refuses; a parsed expression is judged by its calls ([Engine.guardUnsupported]).
// Each catches the construct's characteristic token, so a refusal names the construct
// rather than an opaque CEL "no such field/overload".
//
// soulprint.hosts / .where(...) are not here: they are implemented by a compile-time
// AST rewrite of a static literal predicate into a native filter-comprehension (see
// hosts.go); destiny-pass isolation and receiver/literal validation are done there.
//
//   - now( — eval-time time (see above), refused everywhere.
var unsupportedPatterns = []struct {
	feature string
	re      *regexp.Regexp
}{
	{"now()", regexp.MustCompile(`\bnow\s*\(`)},
}

// vaultGuard catches a vault() call — refused only when the Engine is built without a
// KVReader ([WithVault] registers the function, [vault.go]).
var vaultGuard = regexp.MustCompile(`\bvault\s*\(`)

// generateSecretGuard catches a generate_secret() call — refused in the passes where
// the function is not registered ([ADR-0083] §3: migration, flow-control, service
// vars). Without it the author gets a bare undeclared-reference; with it, a stated
// reason for a call that would otherwise have produced a marker nobody resolves.
var generateSecretGuard = regexp.MustCompile(`\bgenerate_secret\s*\(`)

// generateSecretFeature is the [ErrUnsupported] feature of a generate_secret() call
// outside the one pass that registers it.
const generateSecretFeature = "generate_secret(...) (only the scenario/destiny render pass can request a secret)"

// guardUnsupported returns [ErrUnsupported] if the expression calls a function outside
// pilot scope: now() always, vault() on an Engine without a KVReader, generate_secret()
// outside the ordinary scenario/destiny pass. vars is NOT rejected by the guard: it's
// declared as a variable and resolved from Vars.Vars (the flat namespace: the service's
// own vars under the destiny/task locals); an empty map gives the normal no-such-key,
// not a panic.
//
// It reads the calls in the parsed tree, like [Engine.guardInternalIdents], not the
// text: the Keeper substitutes values into a predicate as literals
// (predicate-rendering.md §5), and a value holding `vault(` is data. The calls in a
// `.where("<predicate>")` literal count, since the rewrite makes them code. Text the
// parser refuses falls back to [guardUnsupportedText], so its refusal reads as it did.
func (e *Engine) guardUnsupported(expr string) error {
	called, ok := e.calledFunctions(expr)
	if !ok {
		return guardUnsupportedText(expr, e.kv != nil, e.genSecret)
	}
	switch {
	case called["now"]:
		return &ErrUnsupported{Expr: expr, Feature: "now()"}
	case e.kv == nil && called["vault"]:
		return &ErrUnsupported{Expr: expr, Feature: "vault(...)"}
	case !e.genSecret && called["generate_secret"]:
		return &ErrUnsupported{Expr: expr, Feature: generateSecretFeature}
	}
	return nil
}

// calledFunctions returns the name of every call in expr and in its `.where`
// predicates, a leading dot dropped and member calls included (`x.now()` counts). ok is
// false when expr or one of its predicates does not parse.
func (e *Engine) calledFunctions(expr string) (map[string]bool, bool) {
	parsed, err := e.parseNoMacro(expr)
	if err != nil {
		return nil, false
	}
	called := map[string]bool{}
	ok := true
	var walk func(ast.Expr)
	walk = func(root ast.Expr) {
		ast.PostOrderVisit(root, ast.NewExprVisitor(func(n ast.Expr) {
			if n.Kind() != ast.CallKind {
				return
			}
			c := n.AsCall()
			called[strings.TrimPrefix(c.FunctionName(), ".")] = true
			if pred, isWhere := wherePredicate(c); isWhere {
				p, err := e.parseNoMacro(pred)
				if err != nil {
					ok = false
					return
				}
				walk(p.Expr())
			}
		}))
	}
	walk(parsed.Expr())
	return called, ok
}

// guardUnsupportedText is [Engine.guardUnsupported] over raw text, for text the parser
// refuses: the compiler refuses it too, and this keeps the more useful reason.
func guardUnsupportedText(expr string, vaultEnabled, genSecretEnabled bool) error {
	for _, p := range unsupportedPatterns {
		if p.re.MatchString(expr) {
			return &ErrUnsupported{Expr: expr, Feature: p.feature}
		}
	}
	if !vaultEnabled && vaultGuard.MatchString(expr) {
		return &ErrUnsupported{Expr: expr, Feature: "vault(...)"}
	}
	if !genSecretEnabled && generateSecretGuard.MatchString(expr) {
		return &ErrUnsupported{Expr: expr, Feature: generateSecretFeature}
	}
	return nil
}

// normalize trims the edges and nothing else. The engine compiles the normalized text,
// so collapsing whitespace inside it would have to know where every literal, comment
// and backtick name ends — a second lexer beside the parser's, the kind that let a
// direct __vault_read through (NIM-965). The compile cache is keyed by the text as
// written.
func normalize(expr string) string {
	return strings.TrimSpace(expr)
}
