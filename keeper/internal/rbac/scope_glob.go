package rbac

import (
	"regexp"
	"strings"
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
func compileGlob(glob string) (*regexp.Regexp, error) {
	return regexp.Compile(globToRE2(glob))
}

// globMatch reports whether target matches glob (anchored). On a compile error
// (unreachable for validated globs) it fails closed (no match).
func globMatch(glob, target string) bool {
	re, err := compileGlob(glob)
	if err != nil {
		return false
	}
	return re.MatchString(target)
}

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
