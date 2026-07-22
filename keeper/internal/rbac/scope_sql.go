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
	Coven       string // TEXT[] column (overlap), e.g. "s.coven"
	Host        string // TEXT column matched by host, e.g. "s.sid"
	Service     string // TEXT column, e.g. "i.service"
	Incarnation string // TEXT column, e.g. "i.name"
	Traits      string // jsonb column, e.g. "s.traits"
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
		// Array overlap: the row's coven set intersects the condition's values.
		return fmt.Sprintf("%s && %s::text[]", b.cols.Coven, b.ph(append([]string(nil), c.Values...)))
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
		// Scalar value: traits->>'k' ∈ values. List value: traits->'k' ?| values.
		vals := b.ph(append([]string(nil), c.Values...))
		key := b.ph(c.Key)
		return fmt.Sprintf("(%s ->> %s = ANY(%s) OR %s -> %s ?| %s)",
			b.cols.Traits, key, vals, b.cols.Traits, key, vals)
	}
	return "FALSE"
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
