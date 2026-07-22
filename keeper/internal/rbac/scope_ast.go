package rbac

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
)

// Boolean scope grammar (NIM-128, ADR-047 S5). A role/permission scope is a
// boolean predicate tree over five dimensions — coven / service / incarnation /
// host / trait — joined by AND/OR with grouping. It replaces the former
// single `key=v1,v2` selector (a flat map) and removes the regex/soulprint/
// state dimensions (pattern-matching moves to `host matches <glob>`).
//
// Grammar (EBNF):
//
//	scope     := "*" | expr           ; "*" handled at the permission layer
//	expr      := or_expr
//	or_expr   := and_expr ( "OR"  and_expr )*   ; OR is the lowest precedence
//	and_expr  := factor  ( "AND" factor  )*     ; AND binds tighter than OR
//	factor    := condition | "(" expr ")"
//	condition := dim ( "=" value | "in" "(" value_list ")" )
//	           | "host" "matches" glob
//	           | "trait." key "=" value
//
// Operators AND/OR/in/matches are case-insensitive on input. A value that
// contains characters outside the bare class must be double-quoted. The old
// flat form `coven=a,b` remains valid — it is a single in-list condition.

const (
	dimCoven       = "coven"
	dimService     = "service"
	dimIncarnation = "incarnation"
	dimHost        = "host"
	dimTrait       = "trait"
)

// scopeDims is the closed enum of scope dimensions (NIM-128). Replaces the
// former allowedSelectorKeys with regex/soulprint/state removed.
var scopeDims = map[string]struct{}{
	dimCoven:       {},
	dimService:     {},
	dimIncarnation: {},
	dimHost:        {},
	dimTrait:       {},
}

// Size caps (fail-closed): a scope predicate that exceeds them fails to parse,
// so a bloated expression never loads into the enforcer and the DNF used by
// the least-privilege subset check ([subsetContains]) stays bounded.
const (
	maxScopeAtoms    = 32  // total leaf conditions in one predicate
	maxScopeDepth    = 4   // parenthesis nesting depth
	maxScopeValueLen = 256 // one value / glob length
	maxDNFConjuncts  = 256 // disjuncts after DNF normalization
	maxScopeTotalLen = 4096
)

var (
	reScopeExact    = regexp.MustCompile(`^[A-Za-z0-9_.-]+$`)   // bare exact value
	reScopeGlob     = regexp.MustCompile(`^[A-Za-z0-9_.*?-]+$`) // bare glob (adds * ?)
	reScopeTraitKey = regexp.MustCompile(`^[a-z][a-z0-9_.-]*$`) // trait.<key>
)

// MatchOp is the operator of a leaf condition.
type MatchOp uint8

const (
	// MatchIn — membership in an exact value set (`=v` is a one-element set,
	// `in (a,b)` a multi-element one; OR within the set).
	MatchIn MatchOp = iota
	// MatchGlob — host glob match (`host matches <glob>`, `*`/`?` wildcards).
	MatchGlob
)

// ScopeOp is the node kind of a [ScopeExpr].
type ScopeOp uint8

const (
	OpLeaf ScopeOp = iota // a single condition (Cond set)
	OpAnd                 // conjunction of Children
	OpOr                  // disjunction of Children
)

// ScopeCond is a leaf condition `<dim>[.<key>] <op> <values>`.
type ScopeCond struct {
	Dim    string   // one of scopeDims
	Key    string   // trait key (Dim==trait only); "" otherwise
	Match  MatchOp  // MatchIn | MatchGlob
	Values []string // exact set (>=1); a single glob for MatchGlob
}

// ScopeExpr is a boolean scope predicate tree. A leaf holds Cond; an internal
// node (OpAnd/OpOr) holds Children (same-op children are flattened by the
// parser). A nil *ScopeExpr means "no scope restriction" (unrestricted).
type ScopeExpr struct {
	Op       ScopeOp
	Children []*ScopeExpr
	Cond     *ScopeCond
}

// --- lexer ---

type tokKind uint8

const (
	tokWord tokKind = iota
	tokQuoted
	tokLParen
	tokRParen
	tokComma
	tokEq
	tokEOF
)

type token struct {
	kind tokKind
	text string
	pos  int
}

func lexScope(s string) ([]token, error) {
	if len(s) > maxScopeTotalLen {
		return nil, fmt.Errorf("scope: expression too long (%d > %d)", len(s), maxScopeTotalLen)
	}
	var toks []token
	i := 0
	for i < len(s) {
		c := s[i]
		switch {
		case c == ' ' || c == '\t' || c == '\n' || c == '\r':
			i++
		case c == '(':
			toks = append(toks, token{tokLParen, "(", i})
			i++
		case c == ')':
			toks = append(toks, token{tokRParen, ")", i})
			i++
		case c == ',':
			toks = append(toks, token{tokComma, ",", i})
			i++
		case c == '=':
			toks = append(toks, token{tokEq, "=", i})
			i++
		case c == '"':
			j := i + 1
			for j < len(s) && s[j] != '"' {
				j++
			}
			if j >= len(s) {
				return nil, fmt.Errorf("scope: unterminated quoted value at %d", i)
			}
			toks = append(toks, token{tokQuoted, s[i+1 : j], i})
			i = j + 1
		default:
			// bareword: run of value/word characters
			j := i
			for j < len(s) && isWordByte(s[j]) {
				j++
			}
			if j == i {
				return nil, fmt.Errorf("scope: unexpected character %q at %d", string(c), i)
			}
			toks = append(toks, token{tokWord, s[i:j], i})
			i = j
		}
	}
	toks = append(toks, token{tokEOF, "", len(s)})
	return toks, nil
}

func isWordByte(c byte) bool {
	return (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') ||
		(c >= '0' && c <= '9') || c == '_' || c == '.' || c == '-' ||
		c == '*' || c == '?'
}

// --- parser ---

type scopeParser struct {
	toks []token
	i    int
}

func (p *scopeParser) peek() token { return p.toks[p.i] }
func (p *scopeParser) advance() token {
	t := p.toks[p.i]
	if p.i < len(p.toks)-1 {
		p.i++
	}
	return t
}
func (p *scopeParser) atEnd() bool { return p.peek().kind == tokEOF }

// peekKeyword reports whether the current token is a bareword equal (case-
// insensitively) to kw. AND/OR/in/matches are recognized positionally, so a
// value literally named "and" is still a valid value inside `=`/`in(...)`.
func (p *scopeParser) peekKeyword(kw string) bool {
	t := p.peek()
	return t.kind == tokWord && strings.EqualFold(t.text, kw)
}

// ParseScopeExpr parses a boolean scope predicate (NIM-128). An empty string
// is an error here (callers that treat "" as "no scope" must check before
// calling). Enforces the size caps (fail-closed).
func ParseScopeExpr(s string) (*ScopeExpr, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil, fmt.Errorf("scope: empty expression")
	}
	toks, err := lexScope(s)
	if err != nil {
		return nil, err
	}
	p := &scopeParser{toks: toks}
	expr, err := p.parseOr(1)
	if err != nil {
		return nil, err
	}
	if !p.atEnd() {
		return nil, fmt.Errorf("scope: unexpected %q after expression", p.peek().text)
	}
	if n := countLeaves(expr); n > maxScopeAtoms {
		return nil, fmt.Errorf("scope: too many conditions (%d > %d)", n, maxScopeAtoms)
	}
	// Validate DNF size up front (fail-closed): the subset check relies on it.
	if _, err := toDNF(expr); err != nil {
		return nil, err
	}
	return expr, nil
}

func (p *scopeParser) parseOr(depth int) (*ScopeExpr, error) {
	first, err := p.parseAnd(depth)
	if err != nil {
		return nil, err
	}
	terms := []*ScopeExpr{first}
	for p.peekKeyword("OR") {
		p.advance()
		t, err := p.parseAnd(depth)
		if err != nil {
			return nil, err
		}
		terms = append(terms, t)
	}
	if len(terms) == 1 {
		return terms[0], nil
	}
	return &ScopeExpr{Op: OpOr, Children: terms}, nil
}

func (p *scopeParser) parseAnd(depth int) (*ScopeExpr, error) {
	first, err := p.parseFactor(depth)
	if err != nil {
		return nil, err
	}
	terms := []*ScopeExpr{first}
	for p.peekKeyword("AND") {
		p.advance()
		t, err := p.parseFactor(depth)
		if err != nil {
			return nil, err
		}
		terms = append(terms, t)
	}
	if len(terms) == 1 {
		return terms[0], nil
	}
	return &ScopeExpr{Op: OpAnd, Children: terms}, nil
}

func (p *scopeParser) parseFactor(depth int) (*ScopeExpr, error) {
	if p.peek().kind == tokLParen {
		if depth+1 > maxScopeDepth {
			return nil, fmt.Errorf("scope: nesting too deep (> %d)", maxScopeDepth)
		}
		p.advance()
		e, err := p.parseOr(depth + 1)
		if err != nil {
			return nil, err
		}
		if p.peek().kind != tokRParen {
			return nil, fmt.Errorf("scope: expected ')' , got %q", p.peek().text)
		}
		p.advance()
		return e, nil
	}
	cond, err := p.parseCondition()
	if err != nil {
		return nil, err
	}
	return &ScopeExpr{Op: OpLeaf, Cond: cond}, nil
}

func (p *scopeParser) parseCondition() (*ScopeCond, error) {
	t := p.peek()
	if t.kind != tokWord {
		return nil, fmt.Errorf("scope: expected a condition, got %q", t.text)
	}
	if strings.EqualFold(t.text, "AND") || strings.EqualFold(t.text, "OR") {
		return nil, fmt.Errorf("scope: unexpected %q (expected a condition)", t.text)
	}
	p.advance()

	// trait.<key>=<value>
	if strings.HasPrefix(t.text, dimTrait+".") {
		key := t.text[len(dimTrait)+1:]
		if !reScopeTraitKey.MatchString(key) {
			return nil, fmt.Errorf("scope: trait key %q must match [a-z][a-z0-9_.-]*", key)
		}
		if p.peek().kind != tokEq {
			return nil, fmt.Errorf("scope: trait.%s must be followed by '='", key)
		}
		p.advance()
		v, err := p.parseValue(false)
		if err != nil {
			return nil, err
		}
		return &ScopeCond{Dim: dimTrait, Key: key, Match: MatchIn, Values: []string{v}}, nil
	}

	dim := t.text
	if _, ok := scopeDims[dim]; !ok || dim == dimTrait {
		return nil, fmt.Errorf("scope: unknown dimension %q (allowed: coven|service|incarnation|host|trait.<key>)", dim)
	}

	switch {
	case p.peek().kind == tokEq:
		// `dim=v` or the old flat form `dim=v1,v2` (comma-list = in-list,
		// backward-compatible with existing default_scope/permission rows).
		p.advance()
		vals, err := p.parseValueList()
		if err != nil {
			return nil, err
		}
		return &ScopeCond{Dim: dim, Match: MatchIn, Values: vals}, nil
	case p.peekKeyword("in"):
		p.advance()
		if p.peek().kind != tokLParen {
			return nil, fmt.Errorf("scope: '%s in' must be followed by '('", dim)
		}
		p.advance()
		vals, err := p.parseValueList()
		if err != nil {
			return nil, err
		}
		if p.peek().kind != tokRParen {
			return nil, fmt.Errorf("scope: expected ')' after '%s in (...)', got %q", dim, p.peek().text)
		}
		p.advance()
		return &ScopeCond{Dim: dim, Match: MatchIn, Values: vals}, nil
	case p.peekKeyword("matches"):
		// `matches <glob>` is valid for host and incarnation (both are name/SID
		// identifiers a glob can range over); coven/service stay exact-only.
		if dim != dimHost && dim != dimIncarnation {
			return nil, fmt.Errorf("scope: 'matches' is only valid for host or incarnation, not %q", dim)
		}
		p.advance()
		g, err := p.parseValue(true)
		if err != nil {
			return nil, err
		}
		return &ScopeCond{Dim: dim, Match: MatchGlob, Values: []string{g}}, nil
	default:
		return nil, fmt.Errorf("scope: expected '=', 'in (...)', or 'matches' after %q, got %q", dim, p.peek().text)
	}
}

func (p *scopeParser) parseValueList() ([]string, error) {
	var out []string
	for {
		v, err := p.parseValue(false)
		if err != nil {
			return nil, err
		}
		out = append(out, v)
		if p.peek().kind == tokComma {
			p.advance()
			continue
		}
		break
	}
	return out, nil
}

// parseValue reads one value token. glob=true validates it as a host glob
// (allows * ?), otherwise as an exact value. A quoted value may carry any
// character except '"' (spaces allowed); an unquoted value must match the
// bare character class.
func (p *scopeParser) parseValue(glob bool) (string, error) {
	t := p.peek()
	switch t.kind {
	case tokQuoted:
		p.advance()
		if t.text == "" {
			return "", fmt.Errorf("scope: empty value")
		}
		if len(t.text) > maxScopeValueLen {
			return "", fmt.Errorf("scope: value too long (%d > %d)", len(t.text), maxScopeValueLen)
		}
		return t.text, nil
	case tokWord:
		p.advance()
		if len(t.text) > maxScopeValueLen {
			return "", fmt.Errorf("scope: value too long (%d > %d)", len(t.text), maxScopeValueLen)
		}
		if glob {
			if !reScopeGlob.MatchString(t.text) {
				return "", fmt.Errorf("scope: glob %q does not match [A-Za-z0-9_.*?-]+ (quote it)", t.text)
			}
		} else {
			if !reScopeExact.MatchString(t.text) {
				return "", fmt.Errorf("scope: value %q does not match [A-Za-z0-9_.-]+ (quote it)", t.text)
			}
		}
		return t.text, nil
	default:
		return "", fmt.Errorf("scope: expected a value, got %q", t.text)
	}
}

// --- canonical stringification ---

// String renders the scope tree back to its canonical form (deterministic:
// same predicate → same string, for audit diffs and UI round-trip). A nil
// expr renders to "".
func (e *ScopeExpr) String() string {
	if e == nil {
		return ""
	}
	if e.Op == OpLeaf {
		return e.Cond.String()
	}
	sep := " AND "
	if e.Op == OpOr {
		sep = " OR "
	}
	parts := make([]string, len(e.Children))
	for i, c := range e.Children {
		s := c.String()
		if c.Op != OpLeaf { // a nested internal node → parenthesize
			s = "(" + s + ")"
		}
		parts[i] = s
	}
	return strings.Join(parts, sep)
}

// String renders a leaf condition canonically.
func (c *ScopeCond) String() string {
	if c.Match == MatchGlob {
		return c.Dim + " matches " + quoteScopeValue(c.Values[0], true)
	}
	base := c.Dim
	if c.Dim == dimTrait {
		base = dimTrait + "." + c.Key
	}
	if c.Dim == dimTrait || len(c.Values) == 1 {
		return base + "=" + quoteScopeValue(c.Values[0], false)
	}
	qs := make([]string, len(c.Values))
	for i, v := range c.Values {
		qs[i] = quoteScopeValue(v, false)
	}
	return base + " in (" + strings.Join(qs, ", ") + ")"
}

func quoteScopeValue(v string, glob bool) string {
	ok := reScopeExact.MatchString(v)
	if glob {
		ok = reScopeGlob.MatchString(v)
	}
	if ok {
		return v
	}
	return `"` + v + `"`
}

// --- helpers ---

func countLeaves(e *ScopeExpr) int {
	if e == nil {
		return 0
	}
	if e.Op == OpLeaf {
		return 1
	}
	n := 0
	for _, c := range e.Children {
		n += countLeaves(c)
	}
	return n
}

// toDNF normalizes an expression into disjunctive normal form: a slice of
// conjuncts, each a slice of leaf conditions AND-ed together; the whole is
// their OR. Enforces [maxDNFConjuncts] (fail-closed — a predicate that would
// explode is rejected). Used by the least-privilege subset check.
func toDNF(e *ScopeExpr) ([][]*ScopeCond, error) {
	if e == nil {
		return nil, nil
	}
	switch e.Op {
	case OpLeaf:
		return [][]*ScopeCond{{e.Cond}}, nil
	case OpOr:
		var out [][]*ScopeCond
		for _, c := range e.Children {
			d, err := toDNF(c)
			if err != nil {
				return nil, err
			}
			out = append(out, d...)
			if len(out) > maxDNFConjuncts {
				return nil, fmt.Errorf("scope: expression too complex (DNF > %d disjuncts)", maxDNFConjuncts)
			}
		}
		return out, nil
	case OpAnd:
		result := [][]*ScopeCond{{}}
		for _, c := range e.Children {
			cd, err := toDNF(c)
			if err != nil {
				return nil, err
			}
			var next [][]*ScopeCond
			for _, r := range result {
				for _, d := range cd {
					merged := make([]*ScopeCond, 0, len(r)+len(d))
					merged = append(merged, r...)
					merged = append(merged, d...)
					next = append(next, merged)
					if len(next) > maxDNFConjuncts {
						return nil, fmt.Errorf("scope: expression too complex (DNF > %d disjuncts)", maxDNFConjuncts)
					}
				}
			}
			result = next
		}
		return result, nil
	}
	return nil, fmt.Errorf("scope: unknown node op %d", e.Op)
}

// scopeDimsUsed returns the set of dimensions referenced anywhere in the
// expression (for Purview summaries / consumer diagnostics).
func scopeDimsUsed(e *ScopeExpr) map[string]struct{} {
	out := make(map[string]struct{})
	var walk func(*ScopeExpr)
	walk = func(n *ScopeExpr) {
		if n == nil {
			return
		}
		if n.Op == OpLeaf {
			out[n.Cond.Dim] = struct{}{}
			return
		}
		for _, c := range n.Children {
			walk(c)
		}
	}
	walk(e)
	return out
}

// sortedSet returns a sorted slice of a string set's keys (nil for empty).
func sortedSet(set map[string]struct{}) []string {
	if len(set) == 0 {
		return nil
	}
	out := make([]string, 0, len(set))
	for v := range set {
		out = append(out, v)
	}
	sort.Strings(out)
	return out
}
