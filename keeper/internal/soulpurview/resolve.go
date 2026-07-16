// Package soulpurview resolves scoped visibility of Souls list by
// [rbac.Purview] (ADR-047 S3b). Souls analog of keeper/internal/statepredicate
// (that one resolves incarnations by state CEL; this one resolves Souls by
// Purview scope dimensions).
//
// Resolver accepts already resolved [rbac.Purview] AS PARAMETER and does NOT
// call enforcer itself: caller side (handler) calls ResolvePurview, while
// soulpurview only translates upper scope boundary into souls query parameters.
// This keeps package free from RBAC resolution (S4 target filter reuses the same
// translation over its Purview intersection) and one-way: soulpurview -> rbac,
// without import cycle.
//
// Perf strategy (foundation extended by additive slices as Purview dimensions):
//   - S3b-0: coven dimension - pure SQL pushdown `souls.coven &&
//     ARRAY[purview.Covens]` (offset/total correct without drift, keyset not needed).
//   - S3b-2a: regex dimension - keyset window by `(registered_at, sid)` + Go OR
//     post-filter over internal pages. Presence of regex DISABLES coven SQL
//     pushdown (otherwise AND would narrow BELOW Purview): host visibility =
//     covenMatch OR regexMatch computed in Go ([CompiledScope.Visible]).
//     Single-read ([InScope]) uses the same coven+regex predicate; list/get are
//     consistent (coven-only InScope divergence removed by gate fix).
//   - S3b-2b: soulprint/state dimensions - page-CEL post-filter (not computed yet;
//     [Scope.Partial] marks scope that pilot cannot express so consumer knows
//     result is NOT complete until S3b-2b).
package soulpurview

import (
	"fmt"
	"regexp"

	"github.com/souls-guild/soul-stack/keeper/internal/rbac"
)

// MaxRegexLen is upper bound for one scope pattern length (ReDoS guard).
// RE2 (google/re2: linear time, no backtracking) is safe by nature, but
// pathologically long pattern is still rejected at compilation: scope patterns
// are short by construction (`^web-`, `^db-\d+`), long one is almost certainly
// error/abuse.
const MaxRegexLen = 256

// Scope is translation of [rbac.Purview] into souls query parameters (upper
// boundary of operator visibility). Terminal flags are mutually exclusive with
// Covens.
//
// fail-closed (ADR-047): uncertainty means "hide", NOT "show whole fleet". This
// is OPPOSITE of presence-overlay (`GET /v1/souls` on Redis error fails SAFE by
// returning PG snapshot): scope hides on doubt, presence shows on doubt. These
// two layers MUST NOT borrow strategy from each other (see handler.List).
type Scope struct {
	// Covens are coven labels covered by visibility (deduped, sorted as in
	// [rbac.Purview]). In coven-only mode applied by SQL pushdown
	// `coven && ARRAY[Covens]`; in keyset mode (Regexes present), one of two OR
	// dimensions in [CompiledScope.Visible]. Meaningful only when
	// !Unrestricted && !Empty.
	Covens []string

	// Regexes are RE2 patterns over SID (ADR-047 S2a/S3b-2a). Host visibility =
	// covenMatch(Covens) OR regexMatch(Regexes): union, NOT intersection.
	// Presence of Regexes moves souls-list to keyset mode (see [Scope.NeedsKeyset]):
	// coven SQL pushdown is disabled, OR filter is computed in Go.
	Regexes []string

	// Unrestricted means no scope restrictions: whole available list without filter.
	Unrestricted bool

	// Empty is fail-closed: operator is allowed NO hosts for (resource, action).
	// Result is EMPTY list (NOT whole fleet). Main security invariant: Purview{}
	// (no dimensions, not Unrestricted) -> Empty=true.
	Empty bool

	// Partial means dimensions beyond coven/regex are present (soulprint/state),
	// and pilot does NOT compute them yet (page CEL is S3b-2b). Result under
	// Partial is NOT complete: it omits hosts available ONLY by soulprint/state.
	// Consumer treats this as "not supported yet" (handler does not present
	// partial set as complete). regex from S3b-2a is excluded from Partial because
	// it is computed by keyset filter.
	Partial bool
}

// NeedsKeyset reports whether scope requires keyset mode (has regex dimension
// that cannot be expressed by pure coven SQL pushdown without narrowing below
// Purview). coven-only / Unrestricted / Empty -> offset fast-path (false).
func (s Scope) NeedsKeyset() bool { return len(s.Regexes) > 0 }

// Resolve translates [rbac.Purview] into [Scope] for souls query.
//
// Semantics (symmetric to statepredicate branching by Purview):
//   - Unrestricted=true -> Scope{Unrestricted:true} (whole list);
//   - Covens and/or Regexes present -> Scope{Covens, Regexes} (coven OR regex:
//     coven pushdown when regex absent, otherwise keyset+Go OR post-filter);
//   - soulprint/state present (not computed in S3b-2a) -> Partial=true (access
//     exists, but pilot does not compute it: S3b-2b);
//   - completely empty (Purview{}, not Unrestricted) -> Empty=true (fail-closed).
//
// Deny from Purview (S2 placeholder) is treated as Empty (fail-closed) as
// defensive measure; coven MVP does not set Purview.Deny.
func Resolve(p rbac.Purview) Scope {
	if p.Unrestricted {
		return Scope{Unrestricted: true}
	}
	if p.Deny {
		return Scope{Empty: true}
	}

	// soulprint/state are dimensions that S3b-2a does NOT compute (coven+regex
	// are computed). Their presence marks result Partial (not complete until S3b-2b).
	hasUnsupported := len(p.SoulprintExprs) > 0 || len(p.StateExprs) > 0
	hasComputable := len(p.Covens) > 0 || len(p.Regexes) > 0

	if !hasComputable && !hasUnsupported {
		// No introduced dimension and not Unrestricted -> fail-closed.
		return Scope{Empty: true}
	}

	return Scope{
		Covens:  p.Covens,
		Regexes: p.Regexes,
		Partial: hasUnsupported,
	}
}

// CompiledScope is [Scope] with precompiled RE2 patterns (compiled once per
// request, not per host). Object returned by [CompileScope] knows whether a host
// is visible within OR boundary of scope.
type CompiledScope struct {
	unrestricted bool
	empty        bool
	covens       []string
	regexes      []*regexp.Regexp
}

// CompileScope compiles scope regex patterns ONCE. Broken pattern or pattern
// over [MaxRegexLen] -> error (caller treats as fail-CLOSED: hide/empty, NOT 500
// and NOT over-show). RE2 (Go regexp) is linear time, so ReDoS is impossible;
// length limit is only a guard against pathological input.
func CompileScope(s Scope) (CompiledScope, error) {
	cs := CompiledScope{
		unrestricted: s.Unrestricted,
		empty:        s.Empty,
		covens:       s.Covens,
	}
	for _, pat := range s.Regexes {
		if len(pat) > MaxRegexLen {
			return CompiledScope{}, fmt.Errorf("soulpurview: regex pattern too long (%d > %d)", len(pat), MaxRegexLen)
		}
		re, err := regexp.Compile(pat)
		if err != nil {
			return CompiledScope{}, fmt.Errorf("soulpurview: invalid regex %q: %w", pat, err)
		}
		cs.regexes = append(cs.regexes, re)
	}
	return cs, nil
}

// Visible reports whether host (sid + covens) is visible in OR boundary of scope:
//
//	visible ⟺ covenMatch(soulCovens, scope.Covens) OR regexMatch(sid, scope.Regexes)
//
// Union (OR), NOT intersection: host matching AT LEAST ONE dimension is visible;
// otherwise keyset filter would narrow visibility BELOW operator Purview.
// Terminals: Unrestricted -> always true; Empty -> always false (fail-closed).
func (cs CompiledScope) Visible(sid string, soulCovens []string) bool {
	if cs.unrestricted {
		return true
	}
	if cs.empty {
		return false
	}
	for _, sc := range cs.covens {
		for _, hc := range soulCovens {
			if sc == hc {
				return true
			}
		}
	}
	for _, re := range cs.regexes {
		if re.MatchString(sid) {
			return true
		}
	}
	return false
}

// InScope reports whether ONE host (sid + covens=soulCovens) is visible in OR
// boundary of scope (single-object check for `GET /v1/souls/{sid}`,
// `/soulprint`, `/history`, ADR-047 S3b-1). Same union semantics as
// [CompiledScope.Visible] in keyset List path: list filters selection,
// single-read checks concrete host, both decide visibility with one predicate:
//
//	visible ⟺ covenMatch(soulCovens, scope.Covens) OR regexMatch(sid, scope.Regexes)
//
// fail-closed (symmetric to [Scope] and [CompiledScope.Visible]):
//   - Unrestricted -> true (any host, including without covens);
//   - Empty -> false (no visible host; operator has no rights);
//   - eval-error (broken/too long regex in Purview, [CompileScope] error) ->
//     false (hide, NOT show and NOT 500): uncertainty = "outside scope";
//   - otherwise -> covenMatch OR regexMatch (through [CompiledScope.Visible]).
//
// S3b-2a -> gate fix: InScope is now coven+regex (list/get divergence from
// S3b-2a removed; regex-visible host in List is also available by GET /{sid}).
// soulprint/state dimensions (Scope.Partial) remain deferred until S3b-2b: they
// are NOT computed here, giving strict narrowing (under-show, never over-show;
// operator may miss a host available ONLY by soulprint, but will not see
// someone else's host). Empty scope (neither Covens nor Regexes) with
// !Unrestricted -> false.
func InScope(scope Scope, sid string, soulCovens []string) bool {
	if scope.Unrestricted {
		return true
	}
	if scope.Empty {
		return false
	}
	compiled, err := CompileScope(scope)
	if err != nil {
		// eval-error fail-CLOSED: broken regex in Purview hides host (as in
		// listKeyset), rather than revealing existence or falling into 500.
		return false
	}
	return compiled.Visible(sid, soulCovens)
}
