package rbac

import (
	"regexp"
	"strings"
	"sync/atomic"
)

// Host glob support (NIM-128, ADR-047 S5). `host matches <glob>` replaces the
// former regex selector. A glob uses `*` (any run, incl. empty) and `?` (one
// char); every other character is literal, matched anchored (full string). The
// operator never sees RE2 syntax — internally the glob is compiled to an
// anchored RE2 pattern, reusing Go's linear-time regexp engine (ReDoS-safe).

// globToRE2 translates an anchored glob into an anchored RE2 source string.
func globToRE2(glob string) string {
	var b strings.Builder
	b.WriteString("^")
	for i := 0; i < len(glob); i++ {
		c := glob[i]
		switch c {
		case '*':
			b.WriteString(".*")
		case '?':
			b.WriteString(".")
		default:
			b.WriteString(regexp.QuoteMeta(string(c)))
		}
	}
	b.WriteString("$")
	return b.String()
}

// compileGlob compiles a glob to a *regexp.Regexp. Never fails for a
// well-formed glob (QuoteMeta escapes everything), but the error is propagated
// defensively.
//
// There is deliberately NO `globMatch(glob, target string)` beside it (NIM-845).
// Such a helper compiled the pattern on every call, and its callers were not the
// per-REQUEST gate they looked like: `soulpurview.InScope` is asked once per
// resolved host, so a role with two `host matches` predicates against a
// 5000-host command Voyage paid 10 000 compiles inside one
// `POST /v1/voyages`. The compiled pattern belongs to the condition that owns
// the glob — see [newGlobCond] — and a match is asked of that condition, so
// there is no longer a spelling of "match this glob" that can compile.
func compileGlob(glob string) (*regexp.Regexp, error) {
	globCompiles.Add(1)
	return regexp.Compile(globToRE2(glob))
}

// globCompiles counts calls to [compileGlob] — the only place this package
// compiles a glob.
//
// It is here for the guard in scope_glob_compile_guard_test.go (NIM-845). The
// invariant worth holding is "a visibility decision over N elements compiles
// ZERO patterns", and there is no way to state that as a test from the outside:
// an allocation threshold would be a proxy that drifts with the regexp package,
// and nothing else about a compile is observable. One atomic increment, paid once
// per glob per role version, next to the compile it counts.
var globCompiles atomic.Uint64

// globToSQLLike translates a glob into a SQL LIKE pattern (`*`→`%`, `?`→`_`),
// escaping LIKE metacharacters (`%` `_` `\`) that appear literally in the
// glob. The caller must use `LIKE ... ESCAPE '\'`. Used by the souls/
// incarnations visibility SQL pushdown.
func globToSQLLike(glob string) string {
	var b strings.Builder
	for i := 0; i < len(glob); i++ {
		c := glob[i]
		switch c {
		case '*':
			b.WriteByte('%')
		case '?':
			b.WriteByte('_')
		case '%', '_', '\\':
			b.WriteByte('\\')
			b.WriteByte(c)
		default:
			b.WriteByte(c)
		}
	}
	return b.String()
}
