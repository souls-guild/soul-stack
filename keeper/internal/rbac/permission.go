package rbac

import "regexp"

// Permission is one expanded permission string.
//
// Grammar (rbac.md § Permission format):
//
//	permission := "*" | <resource>.<action> ( " on " <selector> )?
//	selector   := <key>=<v1>,<v2>,…
//	key        ∈ {service, coven, incarnation, host}
//
// Wildcard variants:
//   - `*` (full) → IsWildcard=true, Resource/Action="", Selector=nil.
//   - `<resource>.*` → Action="*", everything else as usual.
//
// Selector — `nil` means "no filter" (any context matches). A non-nil empty
// map (`len==0`) is an invalid form (parsePermission returns an error before
// construction).
type Permission struct {
	// IsWildcard — full wildcard `*` (equivalent to cluster-admin).
	IsWildcard bool

	// Resource — the first segment (`incarnation`, `operator`, …).
	// Empty when IsWildcard=true.
	Resource string

	// Action — the second segment, or `*`. Empty when IsWildcard=true.
	Action string

	// Selector — map of key → list of values. nil = "no filter" (the
	// permission applies to any context). Multiple values within one key
	// are OR-logic per rbac.md § Semantics.
	Selector map[string][]string
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
//   - Selector=nil → true (permission with no filter).
//   - Selector → for each key-pair in the selector: the key must be present
//     in the context AND the context's value must be among the selector's
//     values. All selector keys must match (AND across keys); OR within one
//     key's values.
//
// Selector keys missing from the context make the permission inapplicable
// (deny). This is deliberate: `incarnation.create on service=foo` must not
// accidentally fire on a request that doesn't specify service in the context.
func (p Permission) Matches(resource, action string, context map[string]string) bool {
	if p.IsWildcard {
		return true
	}
	if p.Resource != resource {
		return false
	}
	if p.Action != "*" && p.Action != action {
		return false
	}
	if p.Selector == nil {
		return true
	}
	for key, values := range p.Selector {
		if key == "regex" {
			// ADR-047 S2a: regex matches against SID/hostname, sourced from
			// the context's host or sid key (some endpoints set `host`,
			// others `sid`). Neither present → deny (like an exact key
			// missing its own context key). OR across one key's patterns.
			target, ok := regexTarget(context)
			if !ok {
				return false
			}
			if !regexAny(values, target) {
				return false
			}
			continue
		}
		if key == "soulprint" {
			// ADR-047 S2b: the soulprint predicate is a CEL expression
			// over host facts (`soulprint.self.*`). The current context
			// (map[string]string) carries no nested SoulprintFacts, so in
			// S2b the soulprint dimension is fail-closed: deny. REAL CEL
			// eval against facts happens in slices S3/S4 (the list/target
			// resolver feeds facts into [EvalSoulprintExpr]); feeding facts
			// into the Check context would require widening the Matches
			// signature, which is out of scope here (S2b boundary). This
			// explicit branch marks the fail-closed as intentional, not a
			// side effect of "key not in context".
			return false
		}
		if key == "state" {
			// ADR-047 S2c: the state predicate is a CEL expression over
			// incarnation.state. The current context (map[string]string)
			// carries no nested incarnation.state, so in S2c the state
			// dimension is fail-closed: deny. REAL CEL eval against state
			// happens in slice S3b (the incarnation list/target resolver
			// feeds state into [EvalStateExpr]); feeding state into the
			// Check context would require widening the Matches signature,
			// which is out of scope here (S2c boundary). Symmetric with the
			// soulprint branch.
			return false
		}
		if key == "trait" {
			// ADR-047 amendment (ADR-060 §7 slice 1): trait is a `key:value`
			// exact match against incarnation.traits. The current context
			// (map[string]string) carries no nested incarnation.traits, so
			// in slice 1 the trait dimension is fail-closed: deny. REAL
			// matching against an incarnation's traits happens in the
			// incarnation-list/get resolver (slice 1 §7:
			// inc.Traits[key]==value); feeding traits into the Check context
			// would require widening the Matches signature, which is not
			// done here. Symmetric with the state/soulprint branches.
			return false
		}
		ctxVal, ok := context[key]
		if !ok {
			return false
		}
		matched := false
		for _, v := range values {
			if v == ctxVal {
				matched = true
				break
			}
		}
		if !matched {
			return false
		}
	}
	return true
}

// regexTarget extracts the string the regex selector matches against: the
// host key takes priority, otherwise sid. nil/neither present → (_, false).
func regexTarget(context map[string]string) (string, bool) {
	if v, ok := context["host"]; ok {
		return v, true
	}
	if v, ok := context["sid"]; ok {
		return v, true
	}
	return "", false
}

// regexAny reports true if at least one pattern matches target. Patterns
// were already validated by [parseRegexValue] at snapshot load
// (regexp.Compile succeeded), so MustCompile here wouldn't panic; recompiling
// on the hot path is acceptable for MVP (Check isn't the hottest path,
// scoped roles are rare). On desync with a broken pattern — fail-closed
// (no match), not panic.
func regexAny(patterns []string, target string) bool {
	for _, pat := range patterns {
		re, err := regexp.Compile(pat)
		if err != nil {
			continue
		}
		if re.MatchString(target) {
			return true
		}
	}
	return false
}
