package cmd

import (
	"errors"
	"strings"

	"github.com/spf13/cobra"
)

// targetFlags — the common set of `--target-*` flags shared by all
// `soulctl run <sub>` subcommands (C1). All three sub-commands gather the
// same selector → different backend endpoints translate it into their own
// body shape (see build).
//
// Semantics:
//   - sids/coven — exact-match lists (CSV in the flag → []string).
//   - glob → CEL expression `sid.glob("X")` (shared/cel.glob, member-overload).
//   - regex → CEL expression `sid.matches("X")` (stdlib).
//   - where → raw CEL (operator-asserted).
//
// AND-merge: glob/regex/where are joined with `&&` into one final `where`.
// sids and coven stay separate fields — the backend does the AND-intersection
// itself (ADR-040/ADR-041 security invariant: an invocation narrows scope,
// never widens it).
type targetFlags struct {
	SIDs  string
	Coven string
	Glob  string
	Regex string
	Where string
}

// bind attaches the `--target-*` flags to a cobra command. Names match across
// all `run` sub-commands (hence a shared helper instead of duplication).
func (t *targetFlags) bind(c *cobra.Command) {
	c.Flags().StringVar(&t.SIDs, "target-sids", "",
		"CSV exact-match SID-ов (`host1,host2`)")
	c.Flags().StringVar(&t.Coven, "target-coven", "",
		"CSV Coven-меток (AND-семантика по `souls.coven`)")
	c.Flags().StringVar(&t.Glob, "target-glob", "",
		"shell-glob по SID; превращается в CEL `sid.glob(\"X\")`")
	c.Flags().StringVar(&t.Regex, "target-regex", "",
		"regex по SID; превращается в CEL `sid.matches(\"X\")`")
	c.Flags().StringVar(&t.Where, "target-where", "",
		"raw CEL-предикат (`soulprint.self.os.family == \"debian\"`)")
}

// resolvedTarget — the result of parsing/merging. Any empty component stays
// empty; the calling sub-command decides which fields to put in the body.
type resolvedTarget struct {
	SIDs  []string
	Coven []string
	Where string
}

// hasAny reports whether at least one target component is set.
func (r resolvedTarget) hasAny() bool {
	return len(r.SIDs) > 0 || len(r.Coven) > 0 || r.Where != ""
}

// resolve parses the CSV fields and joins glob/regex/where into the final
// CEL. Empty CSV tokens are dropped (`a,,b` → `[a, b]`); pattern-shape
// validation is server-side (soul-lint validates CEL; filepath.Match syntax
// is checked at runtime).
func (t targetFlags) resolve() (resolvedTarget, error) {
	out := resolvedTarget{
		SIDs:  splitCSV(t.SIDs),
		Coven: splitCSV(t.Coven),
	}
	parts := make([]string, 0, 3)
	if t.Glob != "" {
		parts = append(parts, "sid.glob("+quoteCEL(t.Glob)+")")
	}
	if t.Regex != "" {
		parts = append(parts, "sid.matches("+quoteCEL(t.Regex)+")")
	}
	if t.Where != "" {
		// Wrap raw CEL in parens — the operator may have written `a || b`;
		// without parens the AND-merge would change precedence.
		parts = append(parts, "("+t.Where+")")
	}
	out.Where = strings.Join(parts, " && ")
	return out, nil
}

// require checks that "a target must be set." Used by sub-commands where a
// scope without a target is meaningless (cmd: nothing to run without hosts).
func (r resolvedTarget) require() error {
	if !r.hasAny() {
		return errors.New("требуется хотя бы один `--target-*` флаг (sids/coven/glob/regex/where)")
	}
	return nil
}

// splitCSV splits a string on commas, trims whitespace, and drops empty
// tokens. Empty input → nil (not []string{}) so json-omitempty works right.
func splitCSV(s string) []string {
	if s == "" {
		return nil
	}
	raw := strings.Split(s, ",")
	out := make([]string, 0, len(raw))
	for _, p := range raw {
		p = strings.TrimSpace(p)
		if p != "" {
			out = append(out, p)
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// quoteCEL escapes a string literal for CEL. CEL accepts double-quoted
// strings with Go escape semantics (cel-spec § Lexical analysis → string), so
// escaping `\` and `"` is enough.
func quoteCEL(s string) string {
	var b strings.Builder
	b.Grow(len(s) + 2)
	b.WriteByte('"')
	for _, r := range s {
		switch r {
		case '\\':
			b.WriteString(`\\`)
		case '"':
			b.WriteString(`\"`)
		case '\n':
			b.WriteString(`\n`)
		case '\t':
			b.WriteString(`\t`)
		case '\r':
			b.WriteString(`\r`)
		default:
			b.WriteRune(r)
		}
	}
	b.WriteByte('"')
	return b.String()
}
