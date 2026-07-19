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

// Matches reports whether the permission satisfies the request.
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
	if p.IsWildcard {
		// Bare `*` = cluster-admin (any context). A scoped `* on <expr>`
		// (NIM-128) is bounded — it matches only where its scope holds,
		// enforced against the request context like any other permission.
		if p.Scope == nil {
			return true
		}
		return evalScope(p.Scope, scopeInputFromContext(context))
	}
	if p.Resource != resource {
		return false
	}
	if p.Action != "*" && p.Action != action {
		return false
	}
	if p.Scope == nil {
		return true
	}
	return evalScope(p.Scope, scopeInputFromContext(context))
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
