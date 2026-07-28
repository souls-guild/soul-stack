package rbac

// Permission is one expanded permission string.
//
// Grammar (rbac.md § Permission format, NIM-128 boolean scope):
//
//	permission := "*" | <resource>.<action> ( " on " <scope-expr> )?
//	scope-expr := boolean predicate over coven/service/incarnation/host/trait
//	              (see scope_ast.go)
//
// Wildcard variants:
//   - `*` (full) → IsWildcard=true, Resource/Action="", Scope=nil.
//   - `<resource>.*` → Action="*", everything else as usual.
//
// Scope — `nil` means "no filter" (any context matches). A boolean scope
// predicate restricts the context (NIM-128, replaces the former flat
// `map[string][]string` selector; regex/soulprint/state dimensions removed).
type Permission struct {
	// IsWildcard — full wildcard `*` (equivalent to cluster-admin).
	IsWildcard bool

	// Resource — the first segment (`incarnation`, `operator`, …).
	// Empty when IsWildcard=true.
	Resource string

	// Action — the second segment, or `*`. Empty when IsWildcard=true.
	Action string

	// Scope — the boolean scope predicate (NIM-128). nil = "no filter" (the
	// permission applies to any context). Evaluated by [evalScope] over a
	// [ScopeInput].
	Scope *ScopeExpr
}

// ScopeInput is the full node context a scope predicate is evaluated against
// (NIM-128 unified resolver). Every dimension is a SET — a Soul has many
// covens; a request context carries at most one value per dimension. A
// dimension absent (empty) from the input makes its conditions fail-closed
// (deny), matching the former "selector key not in context → deny".
type ScopeInput struct {
	Covens       []string
	Services     []string
	Incarnations []string
	Hosts        []string            // sid / hostname candidates
	Traits       map[string][]string // trait key → values (scalar → one-element)
}

// covers reports whether the permission NAMES (resource, action) at all, before
// any scope is considered: a full `*` covers everything, `<resource>.*` covers
// every action of that resource. Split out of the matching logic because both
// decision paths ([Enforcer.Check] and [Enforcer.ResolvePurview]) ask this same
// question first and then part ways over scope.
func (p Permission) covers(resource, action string) bool {
	if p.IsWildcard {
		return true
	}
	if p.Resource != resource {
		return false
	}
	return p.Action == "*" || p.Action == action
}

// effectiveScope is the ONE rule that turns a permission plus the default_scope
// of the role it is held through into the predicate that actually bounds it
// (ADR-047(a)/(b)). nil result = unrestricted.
//
//   - a per-permission scope (`on <expr>`) FULLY overrides the role's
//     default_scope — the role sets a base, an individual permission moves it;
//   - a BARE `*` is never bounded by default_scope (exception #1: `*` literally
//     means everything, or the bootstrap cluster-admin locks itself out —
//     ADR-013/ADR-014). A scoped `* on X` needs no exception: it carries its own
//     predicate and the rule above already returns it;
//   - a bare permission INHERITS the role's default_scope; a role that
//     introduces no scope at all leaves it unrestricted (exception #2,
//     backcompat — existing roles do not break).
//
// Both the gate ([Enforcer.Check]) and the resolver ([Enforcer.ResolvePurview])
// read this function, so the two can no longer disagree about what a role's
// scope covers. They did until NIM-219: Check matched permissions role-blind,
// so a bare permission under a `default_scope`d role passed in ANY context while
// the read path was correctly confined — the gate was the wider of the two, on
// the side where it is more dangerous (mutating endpoints).
func effectiveScope(p Permission, roleScope *ScopeExpr) *ScopeExpr {
	if p.Scope != nil {
		return p.Scope
	}
	if p.IsWildcard {
		return nil
	}
	return roleScope
}

// MatchesInRole reports whether the permission satisfies the request when held
// through a role whose default_scope is roleScope (nil = the role introduces no
// scope). This is the form a decision is made on — the role's scope is inherited
// by its bare permissions (ADR-047(a)), exactly as [Enforcer.ResolvePurview]
// inherits it; see [effectiveScope] for the shared rule.
//
// Contract of resource/action/context — see [Permission.Matches].
func (p Permission) MatchesInRole(resource, action string, roleScope *ScopeExpr, context map[string]string) bool {
	if !p.covers(resource, action) {
		return false
	}
	return evalScope(effectiveScope(p, roleScope), scopeInputFromContext(context))
}

// Matches reports whether the permission satisfies the request, read in
// ISOLATION — as if held through a role with no default_scope. It is the
// roleScope==nil case of [Permission.MatchesInRole].
//
// A permission is not a decision: the role it hangs on may carry a
// `default_scope` that bounds it (ADR-047(a)). Anything authorizing a request
// must therefore go through [Enforcer.Check] / [Permission.MatchesInRole] and
// pass that scope — calling Matches with a role-held permission reads a bare
// right as unrestricted, which is precisely the escalation NIM-219 closed.
//
// Contract:
//   - resource and action are non-empty strings representing a concrete
//     action (no wildcards in the request).
//   - context is the request's runtime context: `{"service": "redis-cluster",
//     "incarnation": "redis-prod"}`. nil is fine (= no keys).
//
// Logic:
//   - IsWildcard → true for any resource/action/context.
//   - Resource mismatch → false.
//   - Action mismatch (accounting for `*`) → false.
//   - Scope=nil → true (permission with no filter).
//   - Scope → evaluate the boolean predicate over the context (built into a
//     [ScopeInput]); a dimension absent from the context fails closed.
//
// The flat request context carries no traits, so a trait condition fails
// closed here — the real trait/host-glob evaluation happens in the unified
// resolver ([EvalScope] with a full [ScopeInput]). Mutating endpoints that
// carry coven/service/incarnation/host in the request context use this path.
func (p Permission) Matches(resource, action string, context map[string]string) bool {
	return p.MatchesInRole(resource, action, nil, context)
}

// scopeInputFromContext builds a [ScopeInput] from the flat request context
// map used by mutating endpoints. host is sourced from both `host` and `sid`
// (as the former regexTarget did). Traits are absent in this path.
func scopeInputFromContext(context map[string]string) ScopeInput {
	in := ScopeInput{}
	if v, ok := context[dimCoven]; ok {
		in.Covens = []string{v}
	}
	if v, ok := context[dimService]; ok {
		in.Services = []string{v}
	}
	if v, ok := context[dimIncarnation]; ok {
		in.Incarnations = []string{v}
	}
	if v, ok := context["host"]; ok {
		in.Hosts = append(in.Hosts, v)
	}
	if v, ok := context["sid"]; ok {
		in.Hosts = append(in.Hosts, v)
	}
	return in
}

// EvalScope reports whether a scope predicate is satisfied by the full node
// context (NIM-128 unified resolver). A nil predicate = unrestricted → true.
// Exported for the souls/incarnation resolvers that build a rich [ScopeInput].
func EvalScope(e *ScopeExpr, in ScopeInput) bool {
	return evalScope(e, in)
}

func evalScope(e *ScopeExpr, in ScopeInput) bool {
	if e == nil {
		return true
	}
	switch e.Op {
	case OpLeaf:
		return evalCond(e.Cond, in)
	case OpAnd:
		for _, c := range e.Children {
			if !evalScope(c, in) {
				return false
			}
		}
		return true
	case OpOr:
		for _, c := range e.Children {
			if evalScope(c, in) {
				return true
			}
		}
		return false
	}
	return false
}

func evalCond(c *ScopeCond, in ScopeInput) bool {
	switch c.Dim {
	case dimCoven:
		return anyInSet(in.Covens, c.Values)
	case dimService:
		return anyInSet(in.Services, c.Values)
	case dimIncarnation:
		if c.Match == MatchGlob {
			return anyGlobMatch(c.Values[0], in.Incarnations)
		}
		return anyInSet(in.Incarnations, c.Values)
	case dimHost:
		if c.Match == MatchGlob {
			return anyGlobMatch(c.Values[0], in.Hosts)
		}
		return anyInSet(in.Hosts, c.Values)
	case dimTrait:
		return anyInSet(in.Traits[c.Key], c.Values)
	}
	return false
}

// anyGlobMatch reports whether the glob matches any element of have. Empty
// have → false (fail-closed).
func anyGlobMatch(glob string, have []string) bool {
	for _, h := range have {
		if globMatch(glob, h) {
			return true
		}
	}
	return false
}

// anyInSet reports whether any element of have is present in the want set.
// Empty have → false (fail-closed: a dimension absent from the context does
// not satisfy a condition on it).
func anyInSet(have, want []string) bool {
	if len(have) == 0 {
		return false
	}
	set := make(map[string]struct{}, len(want))
	for _, w := range want {
		set[w] = struct{}{}
	}
	for _, h := range have {
		if _, ok := set[h]; ok {
			return true
		}
	}
	return false
}
