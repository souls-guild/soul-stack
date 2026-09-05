// Identifiers and type expressions — the only parts of a statement this plugin
// BUILDS as text rather than binds as a value.
//
// CQL has no bind marker for an identifier: a keyspace, table, column or role name
// has to be concatenated into the statement. That is the injection surface, and it
// is closed here by admitting only names that cannot carry one, rather than by
// escaping.
//
// Identifiers and type expressions are not the ONLY things this plugin builds as
// text. The role password is a third, and it is closed the other way — by escaping,
// in [quoteLiteral] — because a password cannot be restricted to a safe character
// set the way a name can. Values on a DML statement are the only things that travel
// as bound parameters; the driver refuses to bind anything else (cql.go).
//
// The admitted set is deliberately NARROWER than what Cassandra accepts: lowercase
// only. Cassandra folds an unquoted identifier to lowercase, so `CREATE KEYSPACE
// MyApp` creates `myapp`, and a subsequent lookup for `MyApp` in system_schema finds
// nothing — the state would create the keyspace on every run and report changed
// forever. Refusing the uppercase name says that once, at Validate time, instead of
// converging to a lie. Managing quoted (case-sensitive) identifiers is not in this
// artifact.
package main

import (
	"fmt"
	"regexp"
	"strings"
)

// identifierRe is Cassandra's unquoted-identifier grammar narrowed to lowercase, and
// bounded by the server's own 48-character limit on keyspace and table names.
var identifierRe = regexp.MustCompile(`^[a-z_][a-z0-9_]{0,47}$`)

// typeCharRe is the character set a CQL type expression may draw from: a bare type
// (`text`, `int`), a collection (`map<text, int>`, `list<frozen<address>>`) or a
// tuple. Quote, semicolon, parenthesis and backslash are outside it.
//
// The character set alone is NOT enough, which is what [checkTypeExpr] adds. A comma
// and a space are legitimate INSIDE `<...>` and nowhere else: `int, b text` and
// `text static` are both drawn entirely from this set, and both smuggle a second
// column definition into the `col + " " + type` that table.go concatenates. The
// first would create an undeclared column and then fail the comparison on every
// later run, because what comes back for that column is `int` and what is declared
// normalizes to `int,btext` — a state that can never converge again.
var typeCharRe = regexp.MustCompile(`^[a-z0-9_<>, ]+$`)

// checkIdentifier reports whether name may be concatenated into a statement,
// addressed as the parameter that carried it.
func checkIdentifier(addr, name string) error {
	if name == "" {
		return fmt.Errorf("%s: must be a non-empty string", addr)
	}
	if !identifierRe.MatchString(name) {
		return fmt.Errorf("%s: %q is not a usable CQL identifier here — "+
			"lowercase letters, digits and underscore only, starting with a letter or underscore, at most 48 characters "+
			"(Cassandra folds an unquoted identifier to lowercase, and this plugin does not manage quoted ones)", addr, name)
	}
	return nil
}

// checkTypeExpr reports whether a column's declared CQL type may be concatenated
// into a CREATE TABLE. ONE type, not a fragment of a column list.
func checkTypeExpr(addr, expr string) error {
	trimmed := strings.TrimSpace(expr)
	if trimmed == "" {
		return fmt.Errorf("%s: must be a non-empty CQL type", addr)
	}
	if !typeCharRe.MatchString(trimmed) {
		return fmt.Errorf("%s: %q is not a usable CQL type expression — "+
			"lowercase type names, digits, underscore and the collection punctuation <>, only", addr, expr)
	}

	// A comma or a space ends the type unless it is inside a collection's angle
	// brackets, where it separates that collection's own parameters. Outside them it
	// would start a SECOND column definition in the statement being built.
	depth := 0
	for _, r := range trimmed {
		switch r {
		case '<':
			depth++
		case '>':
			depth--
			if depth < 0 {
				return fmt.Errorf("%s: %q has unbalanced <>", addr, expr)
			}
		case ',', ' ':
			if depth == 0 {
				return fmt.Errorf("%s: %q is more than one type — a %q outside <> would declare a second column. "+
					"One column, one type; declare the other column as its own entry of params.columns", addr, expr, string(r))
			}
		}
	}
	if depth != 0 {
		return fmt.Errorf("%s: %q has unbalanced <>", addr, expr)
	}
	return nil
}

// normalizeType puts a type expression into the one spelling both sides of a
// comparison can be held to: `map<text, int>` as Cassandra reports it back and
// `map<text,int>` as an author wrote it are the same type, and a difference in
// whitespace is not drift.
func normalizeType(expr string) string {
	return strings.ToLower(strings.Join(strings.Fields(strings.ReplaceAll(expr, " ", "")), ""))
}

// qualify renders `<keyspace>.<table>`. Both halves have passed [checkIdentifier];
// the statements never rely on a session keyspace, so a table action works against a
// keyspace the session was not connected into.
func qualify(keyspace, table string) string {
	return keyspace + "." + table
}
