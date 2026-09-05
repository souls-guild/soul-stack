// What the driver will and will not bind, and how to write a value it refuses to
// bind.
//
// ★ THE DRIVER ONLY BINDS VALUES ON DML. gocql prepares — and therefore binds — a
// statement only when its first word is select / insert / update / delete / batch:
// `shouldPrepare` in session.go:1258, and conn.go:1607-1670, where `params.values` is
// filled ONLY inside the `shouldPrepare` branch. The else branch builds a
// `writeQueryFrame` with `params.values` still nil, so a `?` in a CREATE ROLE or an
// ALTER ROLE reaches the server with NOTHING bound to it and the statement is a
// syntax error.
//
// That is silent: the driver returns no error for the values it dropped. So the two
// places that could hit it are handled explicitly rather than hopefully — a role
// password is written as a quoted literal by [quoteLiteral], and `command.run`
// REFUSES args it knows would be discarded ([isDML]).
//
// [isDML] mirrors the driver's rule rather than calling it — gocql does not export
// it. The list has been the same for a decade, but "mirrors" has to mean the WHOLE
// rule, including the case with no interior whitespace: the driver leaves its verb
// EMPTY when the statement is a single token, so `delete` alone is not DML to it. An
// earlier version of this function fell back to the whole string there and answered
// true, which is the silent direction — args accepted and then discarded. No valid
// CQL statement is a single token, so nothing reached it; it is fixed because a rule
// that claims to mirror another one either does or does not.
//
// The two directions of drift are not symmetric. If the driver grows a case this
// does not have, we refuse args it would have bound — a message an author can act
// on. The reverse is the silent hole, so the tests below pin the boundary rather
// than trusting the reading.
package main

import (
	"fmt"
	"strings"
	"unicode"
)

// quoteLiteral renders a string as a CQL single-quoted literal.
//
// Doubling the quote is the WHOLE escape rule for CQL strings — unlike SQL dialects
// with C-style escapes, a backslash in a CQL literal is an ordinary character — so
// this is complete rather than a best effort. Control characters are refused anyway:
// a newline inside a statement is not an injection here, but it makes whatever log
// the statement reaches unreadable, and no legitimate password carries one.
func quoteLiteral(addr, value string) (string, error) {
	for _, r := range value {
		if unicode.IsControl(r) {
			return "", fmt.Errorf("%s: must not contain control characters", addr)
		}
	}
	return "'" + strings.ReplaceAll(value, "'", "''") + "'", nil
}

// isDML reports whether the driver will prepare this statement, and therefore
// whether the values handed beside it will actually be bound. See the file comment
// for where this rule lives in the driver.
func isDML(stmt string) bool {
	trimmed := strings.TrimLeftFunc(strings.TrimRightFunc(stmt, func(r rune) bool {
		return unicode.IsSpace(r) || r == ';'
	}), unicode.IsSpace)

	// Empty, not the whole string, when there is no interior whitespace — the
	// driver's own behaviour, and the direction that fails closed.
	verb := ""
	if n := strings.IndexFunc(trimmed, unicode.IsSpace); n >= 0 {
		verb = strings.ToLower(trimmed[:n])
	}
	if verb == "begin" {
		// `BEGIN BATCH ... APPLY BATCH` — the driver takes the LAST word.
		if n := strings.LastIndexFunc(trimmed, unicode.IsSpace); n >= 0 {
			verb = strings.ToLower(trimmed[n+1:])
		}
	}

	switch verb {
	case "select", "insert", "update", "delete", "batch":
		return true
	}
	return false
}
