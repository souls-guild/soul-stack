package middleware

import (
	"errors"
	"net/http"

	"github.com/souls-guild/soul-stack/keeper/internal/api/problem"
	"github.com/souls-guild/soul-stack/keeper/internal/rbac"
)

// SelectorExtractor — a function that extracts the runtime context from the request
// for the permission check.
//
// An endpoint that needs no context (e.g. `POST /v1/operators` —
// permission `operator.create` without a selector) passes NoSelector.
// An endpoint with context (e.g. `POST /v1/incarnations`) — a closure
// that reads name from the body / path and returns `{"incarnation": name}`.
//
// A nil return and an empty map are equivalent: a permission with a selector will
// not match, but a bare permission and full-wildcard will.
type SelectorExtractor func(r *http.Request) map[string]string

// NoSelector — an empty extractor for endpoints without context filters.
// Used on operator endpoints (`POST /v1/operators*`) — their permissions
// (`operator.create` / `operator.revoke` / `operator.issue-token`)
// have no selectors in rbac.md, the context is always empty.
func NoSelector(_ *http.Request) map[string]string { return nil }

// MultiSelectorExtractor — a function that extracts a SET of runtime contexts from the
// request for a permission check with OR-semantics across contexts.
//
// Needed where one selector key has MULTIPLE candidate values, while
// [rbac.Permission.Matches] operates on a single-value context (`coven=` for
// an incarnation = `incarnation.covens ∪ {name}`, ADR-008 amendment a). Each
// returned context describes one candidate (one coven label plus the
// unchanged `incarnation`/`service`); the permission is granted if it matches
// AT LEAST ONE of the contexts (see [RequirePermissionMulti]).
//
// Return contract:
//   - nil / an empty slice → access only for bare-/`*`-permissions (same as a
//     nil return from [SelectorExtractor]): no coven-/service-scoped permission will
//     match. Used when the extractor failed to read the data
//     (incarnation not found / broken body) — fail-closed for scoped roles.
//   - a non-empty slice → the permission passes if any context matches.
//
// Within a single context the AND-semantics of [rbac.Permission.Matches] still hold
// (e.g. a permission `... on coven=prod` is not implicitly broken by service —
// a service-only permission matches on any coven iteration, a coven-only one — on its
// own label).
type MultiSelectorExtractor func(r *http.Request) []map[string]string

// PermissionChecker — the narrow subset of the rbac surface the
// middleware needs. Implemented by both [rbac.Enforcer] and [rbac.Holder] (the latter
// wraps the Enforcer with pointer-cmp invalidation for hot-reloading the
// `rbac:` block via [config.Store], see ADR-021 + docs/keeper/config.md).
//
// The narrowing serves two purposes: (1) unit tests can pass a direct
// Enforcer without standing up a Store; (2) the production wire-up in `keeper run` passes a
// Holder, which rebuilds the Enforcer on every Reload swap.
type PermissionChecker interface {
	Check(aid, resource, action string, context map[string]string) error
}

// ActionHolder — the narrow existence-gate surface for read endpoints (ADR-047 §d
// amendment 2026-06-04), needed by [RequireAction]. Implemented by both [rbac.Enforcer]
// and [rbac.Holder] (like [PermissionChecker]).
//
// A separate interface rather than an extension of [PermissionChecker]: the question is
// different (existence without a scope context, bool instead of error), the signature differs. The narrow
// contract keeps the middleware's dependency minimal and lets a unit test
// pass a direct Enforcer without a Store.
type ActionHolder interface {
	HoldsAction(aid, resource, action string) bool
}

// RequirePermission — a middleware factory. Must be used after
// [RequireJWT] (otherwise ClaimsFromContext returns ok=false → 500 logic, not
// 401: a missing JWT is a server configuration error, not the user's).
//
// On deny it returns 403 problem+json. On misconfiguration (no
// claims in the context) — 500 (logging is done by the caller via server-wide
// recovery; here only a short generic detail).
func RequirePermission(e PermissionChecker, resource, action string, extractor SelectorExtractor) func(http.Handler) http.Handler {
	if extractor == nil {
		extractor = NoSelector
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			claims, ok := ClaimsFromContext(r.Context())
			if !ok {
				// The JWT middleware did not run — this is a chain configuration
				// error, not the client's. 500 with no detail.
				WriteInternal(w, r)
				return
			}
			ctx := extractor(r)
			if err := e.Check(claims.Subject, resource, action, ctx); err != nil {
				// ErrOperatorRevoked → 401 (ADR-014 Amendment 2026-05-27,
				// parity with an expired JWT — the token is no longer trusted).
				// ErrPermissionDenied and everything else → 403 (internal
				// diagnostics are not leaked, detail lives in the audit middleware).
				if errors.Is(err, rbac.ErrOperatorRevoked) {
					problem.Write(w, problem.New(problem.TypeOperatorRevokedToken, r.URL.Path,
						"archon "+claims.Subject+" has been revoked"))
					return
				}
				detail := "operator lacks required permission"
				if errors.Is(err, rbac.ErrPermissionDenied) {
					detail = "operator lacks required permission " + resource + "." + action
				}
				problem.Write(w, problem.New(problem.TypeForbidden, r.URL.Path, detail))
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// RequireAction — a middleware factory for the existence-gate on read endpoints (ADR-047 §d
// amendment 2026-06-04). Must be used after [RequireJWT] (like
// [RequirePermission]).
//
// Difference from [RequirePermission]: that one calls the scope-aware [PermissionChecker.Check]
// with a scope context from [SelectorExtractor] and cuts off a scoped operator whose
// context did not match the selector. At the middleware stage a read endpoint does NOT yet know
// the scope context (host/coven/state are resolved from DB rows that don't exist before the
// fetch), so `Check(...,nil)` would produce a false deny for a scoped permission.
// RequireAction asks a different question — does the operator hold the action AT ALL
// ([ActionHolder.HoldsAction]); the scope narrowing is done by the handler after the fetch
// (per-resource resolvers soulpurview/statepredicate).
//
// Does not hold the action → 403 (the same problem+json style as [RequirePermission]).
// Missing claims → 500 (parity: the JWT middleware did not run = a chain configuration
// error, not the client's).
func RequireAction(h ActionHolder, resource, action string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			claims, ok := ClaimsFromContext(r.Context())
			if !ok {
				WriteInternal(w, r)
				return
			}
			if !h.HoldsAction(claims.Subject, resource, action) {
				problem.Write(w, problem.New(problem.TypeForbidden, r.URL.Path,
					"operator lacks required permission "+resource+"."+action))
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// RequireAnyPermission — a middleware factory with OR-semantics over a SET of
// `<action>` values for one resource in a single [SelectorExtractor] context. Must
// be used after [RequireJWT] (like [RequirePermission]).
//
// Difference from [RequirePermissionMulti]: that one ORs a single `<resource>.<action>` across a
// set of *contexts* (multi-value `coven=`); this one ORs several
// *permission names* `<resource>.<action_i>` in a single context. Needed where an
// endpoint accepts any of several rights — e.g. the granular
// `cadence.enable` OR the backcompat grant `cadence.update` on
// `POST /v1/cadences/{id}/enable` (roles with the old `cadence.update` do not lose
// access when granular enable/disable is introduced, ADR-046 amendment 2026-06-02).
//
// Granted if [PermissionChecker.Check] returned nil for AT LEAST ONE action.
// On deny it returns 403 mentioning the FIRST action of the set (the canonical one
// for the endpoint); on missing claims — 500 (parity with [RequirePermission]).
//
// Metrics side-effect: Check is called once per action up to the first
// allow, so on a match of a non-first right the rbac_checks_total{result=
// "deny"} counter is over-counted (parity with the over-count in [RequirePermissionMulti]). The
// actions set is short (2 for the cadence toggle), the effect is minor.
func RequireAnyPermission(e PermissionChecker, resource string, actions []string, extractor SelectorExtractor) func(http.Handler) http.Handler {
	if extractor == nil {
		extractor = NoSelector
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			claims, ok := ClaimsFromContext(r.Context())
			if !ok {
				WriteInternal(w, r)
				return
			}
			ctx := extractor(r)
			var lastErr error
			for _, action := range actions {
				err := e.Check(claims.Subject, resource, action, ctx)
				if err == nil {
					next.ServeHTTP(w, r)
					return
				}
				lastErr = err
				// revoked-AID — short-circuits to 401 on any action
				// (parity with [RequirePermissionMulti], ADR-014 Amendment).
				if errors.Is(err, rbac.ErrOperatorRevoked) {
					problem.Write(w, problem.New(problem.TypeOperatorRevokedToken, r.URL.Path,
						"archon "+claims.Subject+" has been revoked"))
					return
				}
			}
			detail := "operator lacks required permission"
			if errors.Is(lastErr, rbac.ErrPermissionDenied) && len(actions) > 0 {
				detail = "operator lacks required permission " + resource + "." + actions[0]
			}
			problem.Write(w, problem.New(problem.TypeForbidden, r.URL.Path, detail))
		})
	}
}

// RequirePermissionMulti — a middleware factory with OR-semantics over a set of
// contexts from [MultiSelectorExtractor]. Must be used after
// [RequireJWT] (like [RequirePermission]).
//
// Granted if [PermissionChecker.Check] returned nil for AT LEAST ONE
// context. This lands multi-value `coven=` without changing
// [rbac.Permission.Matches] (a single-value context): the extractor expands
// `incarnation.covens ∪ {name}` into a per-candidate set of contexts, the OR happens here.
// An empty set (the extractor returned nil/[]) → try a single empty
// context: only bare-/`*`-permissions pass (fail-closed for
// coven-/service-scoped ones, same as a nil return from [SelectorExtractor]).
//
// On deny it returns 403; on missing claims — 500 (parity with
// [RequirePermission]).
//
// Metrics side-effect: [PermissionChecker.Check] is called once per
// context of the set, and keeper_rbac_checks_total is observed INSIDE Check
// (the Holder wrapper, keeper/internal/rbac/metrics.go). So one logical
// incarnation gate produces up to N counter increments, and on a match of a non-first
// context — N-1 spurious `deny`s before the final `allow`. This is a deliberate minor issue:
// removing it without a no-observe variant of Check is not possible (the middleware has no
// metrics surface — it works through the narrow [PermissionChecker]), and introducing
// one for this nit alone is excessive. Take this into account when alerting on rbac_checks_total{result="deny"}:
// for coven-scoped incarnation endpoints deny is over-counted by the size of the context set.
func RequirePermissionMulti(e PermissionChecker, resource, action string, extractor MultiSelectorExtractor) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			claims, ok := ClaimsFromContext(r.Context())
			if !ok {
				WriteInternal(w, r)
				return
			}

			var contexts []map[string]string
			if extractor != nil {
				contexts = extractor(r)
			}
			// An empty set → a single attempt with an empty context (bare/`*` pass,
			// scoped ones do not). This preserves fail-closed for scoped roles when
			// the extractor failed to land the data (404 / broken body).
			if len(contexts) == 0 {
				contexts = []map[string]string{nil}
			}

			var lastErr error
			for _, ctx := range contexts {
				err := e.Check(claims.Subject, resource, action, ctx)
				if err == nil {
					next.ServeHTTP(w, r)
					return
				}
				lastErr = err
				// ErrOperatorRevoked — short-circuits: a revoked AID does not
				// merely "not fit one of the contexts", it is a deny on any
				// context. Map it to 401 right away (ADR-014 Amendment 2026-05-27).
				if errors.Is(err, rbac.ErrOperatorRevoked) {
					problem.Write(w, problem.New(problem.TypeOperatorRevokedToken, r.URL.Path,
						"archon "+claims.Subject+" has been revoked"))
					return
				}
			}

			detail := "operator lacks required permission"
			if errors.Is(lastErr, rbac.ErrPermissionDenied) {
				detail = "operator lacks required permission " + resource + "." + action
			}
			problem.Write(w, problem.New(problem.TypeForbidden, r.URL.Path, detail))
		})
	}
}
