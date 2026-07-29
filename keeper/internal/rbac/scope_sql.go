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
	Incarnation string // TEXT column, e.g. "i.name"
	Traits      string // jsonb column, e.g. "souls.traits"

	// MembershipSID enables label inheritance (ADR-080): the resource's SID
	// column, QUALIFIED (e.g. "souls.sid"), correlating a subquery over
	// `incarnation_membership`. When set, the coven and trait conditions also
	// match labels the row inherits from the incarnations it belongs to — a
	// label lives only where it was attached, and the union happens here.
	// Empty (the incarnation table, which carries its labels directly) leaves
	// both conditions matching own columns only.
	//
	// MUST be table-qualified: the subquery aliases `incarnation_membership m`,
	// so a bare "sid" would resolve to `m.sid` and correlate the row to itself.
	MembershipSID string
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
		return CovenScopeSQL(b.cols.Coven, b.cols.MembershipSID, b.ph(append([]string(nil), c.Values...)))
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
		own := fmt.Sprintf("(%s ->> %s = ANY(%s) OR %s -> %s ?| %s)",
			b.cols.Traits, key, vals, b.cols.Traits, key, vals)
		// Inherited (ADR-080): the same pair on an incarnation the host belongs
		// to. Own and inherited are OR-ed, never ranked — `owner=dba` on the
		// incarnation and `owner=bobik` on the host both grant.
		return b.orInherited(own, fmt.Sprintf("i.traits ->> %s = ANY(%s) OR i.traits -> %s ?| %s",
			key, vals, key, vals))
	}
	return "FALSE"
}

// CovenScopeSQL renders the coven-dimension predicate: the row carries ANY of
// the labels bound to valuesPlaceholder (ONE placeholder holding a text[]).
//
// With membershipSID set it resolves EFFECTIVE labels (ADR-080) — the row's own
// coven column OR the labels of an incarnation it belongs to, that incarnation's
// NAME included. An empty membershipSID leaves it matching the own column alone
// (the incarnation table carries its labels directly, with nothing to inherit
// from).
//
// covenCol and membershipSID MUST be table-qualified: the subquery aliases
// `incarnation_membership m`, so a bare `sid` would bind there and correlate
// every row to itself. The `i` / `m` aliases are local to the subquery, so an
// outer query using those letters is unaffected.
//
// Exported because this is not only the RBAC pushdown. The souls list filter,
// the bulk selector and the bulk scope gate render the SAME predicate (NIM-250),
// so what an operator can find, what a bulk call selects, and what authorizes
// that call cannot drift into three different answers about one host.
func CovenScopeSQL(covenCol, membershipSID, valuesPlaceholder string) string {
	// Array overlap: the row's coven set intersects the condition's values.
	own := fmt.Sprintf("%s && %s::text[]", covenCol, valuesPlaceholder)
	if membershipSID == "" {
		return own
	}
	// Inherited (ADR-080): an incarnation the row belongs to carries a matching
	// tag. Its NAME counts as one — a host's effective covens include the names
	// of its incarnations, so `coven=<incarnation>` reaches the hosts inside it
	// instead of showing the container and hiding its contents.
	return fmt.Sprintf(`(%s OR EXISTS (
    SELECT 1 FROM incarnation_membership m
    JOIN incarnation i ON i.name = m.incarnation_name
    WHERE m.sid = %s AND (i.covens && %s::text[] OR i.name = ANY(%s::text[]))
))`, own, membershipSID, valuesPlaceholder, valuesPlaceholder)
}

// orInherited widens an own-column predicate with the same predicate evaluated
// over the incarnations the row belongs to (ADR-080 label inheritance). Returns
// `own` untouched when the resource carries no membership correlation
// (MembershipSID empty) — the incarnation table holds its labels directly.
//
// `incarnationPred` is written against the alias `i` (and may reference `m`);
// both are local to the subquery, so an outer query using those letters is
// unaffected.
func (b *scopeSQLBuilder) orInherited(own, incarnationPred string) string {
	if b.cols.MembershipSID == "" {
		return own
	}
	return fmt.Sprintf(`(%s OR EXISTS (
    SELECT 1 FROM incarnation_membership m
    JOIN incarnation i ON i.name = m.incarnation_name
    WHERE m.sid = %s AND (%s)
))`, own, b.cols.MembershipSID, incarnationPred)
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
