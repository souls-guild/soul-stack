package rbac

import (
	"fmt"
	"strings"
)

// SQL pushdown of a boolean scope predicate (NIM-128, ADR-047 S3/S4). A
// [Purview] is translated into a parameterized SQL boolean expression over a
// resource's columns, so souls/incarnations visibility narrows in the database
// instead of a Go post-filter. Fail-closed: a condition on a dimension the
// resource does not carry renders to FALSE (never TRUE).

// ScopeColumns maps scope dimensions onto SQL expressions for one resource
// table. An empty string means the resource does not carry that dimension —
// a condition on it renders FALSE.
type ScopeColumns struct {
	Coven       string // TEXT[] column (overlap), e.g. "souls.coven"
	Host        string // TEXT column matched by host, e.g. "souls.sid"
	Service     string // TEXT column, e.g. "i.service"
	Incarnation string // TEXT column, e.g. "i.id"
	Traits      string // jsonb column, e.g. "souls.traits"
}

// PurviewSQL renders a Purview into a parameterized SQL boolean expression over
// cols. startIdx is the next positional placeholder number ($startIdx). It
// returns the SQL fragment (always parenthesized and safe to AND into a WHERE),
// the args to append (in placeholder order), and the next free placeholder
// index.
//
//   - Unrestricted → "TRUE" (no narrowing).
//   - Deny / empty (no access) → "FALSE" (fail-closed).
//   - otherwise → OR of each scope predicate's SQL.
func PurviewSQL(p Purview, cols ScopeColumns, startIdx int) (string, []any, int) {
	if p.Unrestricted {
		return "TRUE", nil, startIdx
	}
	if p.IsEmpty() || p.Deny {
		return "FALSE", nil, startIdx
	}
	b := &scopeSQLBuilder{cols: cols, idx: startIdx}
	parts := make([]string, len(p.Exprs))
	for i, e := range p.Exprs {
		parts[i] = b.expr(e)
	}
	return "(" + strings.Join(parts, " OR ") + ")", b.args, b.idx
}

type scopeSQLBuilder struct {
	cols ScopeColumns
	args []any
	idx  int
}

// ph appends an arg and returns its `$N` placeholder.
func (b *scopeSQLBuilder) ph(v any) string {
	p := fmt.Sprintf("$%d", b.idx)
	b.args = append(b.args, v)
	b.idx++
	return p
}

func (b *scopeSQLBuilder) expr(e *ScopeExpr) string {
	if e == nil {
		return "TRUE"
	}
	switch e.Op {
	case OpLeaf:
		return b.cond(e.Cond)
	case OpAnd:
		return b.join(e.Children, " AND ")
	case OpOr:
		return b.join(e.Children, " OR ")
	}
	return "FALSE"
}

func (b *scopeSQLBuilder) join(children []*ScopeExpr, sep string) string {
	parts := make([]string, len(children))
	for i, c := range children {
		parts[i] = b.expr(c)
	}
	return "(" + strings.Join(parts, sep) + ")"
}

func (b *scopeSQLBuilder) cond(c *ScopeCond) string {
	switch c.Dim {
	case dimCoven:
		if b.cols.Coven == "" {
			return "FALSE"
		}
		return CovenScopeSQL(b.cols.Coven, b.ph(append([]string(nil), c.Values...)))
	case dimService:
		return b.inList(b.cols.Service, c.Values)
	case dimIncarnation:
		if c.Match == MatchGlob {
			return b.glob(b.cols.Incarnation, c.Values[0])
		}
		return b.inList(b.cols.Incarnation, c.Values)
	case dimHost:
		if c.Match == MatchGlob {
			return b.glob(b.cols.Host, c.Values[0])
		}
		return b.inList(b.cols.Host, c.Values)
	case dimTrait:
		if b.cols.Traits == "" {
			return "FALSE"
		}
		vals := b.ph(append([]string(nil), c.Values...))
		key := b.ph(c.Key)
		return TraitScopeSQL(b.cols.Traits, key, vals)
	}
	return "FALSE"
}

// TraitScopeSQL renders the trait-dimension predicate: the row's `traits` jsonb
// carries, under key keyPlaceholder, one of the VALUES bound to valuesPlaceholder
// (ONE placeholder holding a text[]).
//
// `trait.<key>=<value>` reaches a WHOLE value and only a whole value (NIM-522):
//
//	scalar    → its text, exactly as `->>` yields it
//	array     → the text of any ONE of its SCALAR elements — string, number or
//	            bool alike (`{"ports": [6379, 6380]}` is reached by
//	            `trait.ports=6379`)
//	object    → nothing. A scope value is a value, never a key, so
//	            `{"tier": {"k": "gold"}}` is NOT reached by `trait.tier=k`.
//	container → its OWN text is not a value either: `->>` over an array yields
//	            `["prod", "stage"]`, and naming that string grants nothing.
//
// The jsonb_typeof guards are what enforce the last two lines, and the CASE is
// what keeps `jsonb_array_elements` off a scalar (it errors there rather than
// returning no rows). `e #>> '{}'` is the element's text in Postgres' own
// rendering — the same text `->>` would give were the element stored alone,
// which is what lets [TraitValues] reproduce this arm byte-for-byte in Go.
//
// It matches the row's OWN traits column and nothing else (NIM-281): a trait
// pair grants only where an operator attached it, and belonging to an
// incarnation attaches nothing.
//
// Exported for the same reason as [CovenScopeSQL]: this predicate is one rule,
// and every surface that narrows by trait renders THIS function rather than its
// own copy of the expression.
func TraitScopeSQL(traitsCol, keyPlaceholder, valuesPlaceholder string) string {
	elems := fmt.Sprintf("CASE WHEN jsonb_typeof(%[1]s -> %[2]s) = 'array' THEN %[1]s -> %[2]s ELSE '[]'::jsonb END",
		traitsCol, keyPlaceholder)
	return fmt.Sprintf(
		"((jsonb_typeof(%[1]s -> %[2]s) NOT IN ('object', 'array') AND %[1]s ->> %[2]s = ANY(%[3]s))"+
			" OR EXISTS (SELECT 1 FROM jsonb_array_elements(%[4]s) AS _trait_elem"+
			" WHERE jsonb_typeof(_trait_elem) NOT IN ('object', 'array')"+
			" AND _trait_elem #>> '{}' = ANY(%[3]s)))",
		traitsCol, keyPlaceholder, valuesPlaceholder, elems)
}

// CovenScopeSQL renders the coven-dimension predicate: the row carries ANY of
// the labels bound to valuesPlaceholder (ONE placeholder holding a text[]).
//
// It matches the row's OWN coven column and nothing else. There is no label
// inheritance (NIM-281, reverting ADR-080): a coven tag exists only where an
// operator attached it, keeper never mints one, and an incarnation's labels —
// or its name — say nothing about the hosts that belong to it. Reaching an
// incarnation's members is a membership question, spelled `incarnation=<name>`.
//
// Exported because this is not only the RBAC pushdown. The souls list filter,
// the bulk selector and the bulk scope gate render the SAME predicate (NIM-250),
// so what an operator can find, what a bulk call selects, and what authorizes
// that call cannot drift into three different answers about one host.
func CovenScopeSQL(covenCol, valuesPlaceholder string) string {
	// Array overlap: the row's coven set intersects the condition's values.
	return fmt.Sprintf("%s && %s::text[]", covenCol, valuesPlaceholder)
}

// inList renders `col = ANY($vals::text[])`, or FALSE when the column is absent.
func (b *scopeSQLBuilder) inList(col string, values []string) string {
	if col == "" {
		return "FALSE"
	}
	return fmt.Sprintf("%s = ANY(%s::text[])", col, b.ph(append([]string(nil), values...)))
}

// glob renders `col LIKE $pat ESCAPE '\'`, or FALSE when the column is absent.
func (b *scopeSQLBuilder) glob(col, glob string) string {
	if col == "" {
		return "FALSE"
	}
	return fmt.Sprintf("%s LIKE %s ESCAPE '\\'", col, b.ph(globToSQLLike(glob)))
}
