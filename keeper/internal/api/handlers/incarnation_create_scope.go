package handlers

import (
	"errors"

	"github.com/souls-guild/soul-stack/keeper/internal/api/middleware"
)

// The create gate is asked twice, and the second time is the boundary (NIM-333).
//
// Gate (a) runs before the handler: [middleware.RequirePermissionMulti] over
// [IncarnationCreateContexts]. It is an OR — granted if ANY one context matches —
// which is right for its job (a cheap pre-filter answering "does this operator hold
// `incarnation.create` in a scope that touches this request at all") and wrong as a
// boundary, for two independent reasons:
//
//   - **The OR admits a superset.** A request declaring `covens: [billing, prod]`
//     emits one context per coven, and an operator scoped `coven=billing` matches
//     the first. The create then proceeds carrying the `prod` label too — a label
//     the caller may not use, on an object it just minted. Every `coven=prod` role
//     now reads and runs that incarnation, and its service vars resolve through
//     whatever `prod` overlay the service declares (ADR-0082), so the caller has
//     written into a scope it does not hold. This is not specific to templating and
//     predates it: named create has always had it.
//   - **`incarnation=` is unanswerable before composition.** Under `id_template`
//     (ADR-0079) the name is composed server-side from the resolved input, so at
//     gate (a) it does not exist. Answering nil there is what made templated create
//     the privilege of an unrestricted role, which is the defect NIM-333 opened on.
//
// Gate (b) — this file — is asked once the plan is resolved and the name is known.
// It is an **AND over every declared dimension**: each declared coven must be inside
// the caller's scope, all-or-nothing, with no silent trim of the labels they may not
// use. That is the same shape the per-host bind gate settled on (NIM-209/NIM-232):
// a bulk claim is admitted whole or refused whole, never quietly reduced.
//
// What gate (b) does NOT do, stated because the opposite is the natural guess: it
// does not enable a role scoped ONLY by `incarnation=`. Such a role fails gate (a)
// before composition ever runs — the dimension is absent from that context by
// construction — so the create is refused and gate (b) is never reached. Pinned by
// TestToolsCall_IncarnationCreate_Templated_IncarnationScopedRoleStillDenied. Gate
// (b) earns its place on the two things above: the AND over declared covens, and
// measuring the composed name for a caller whose purview carries an `incarnation=`
// dimension that some OTHER permission of theirs got past gate (a).
//
// Both gates read the same predicate — [middleware.PermissionChecker.Check] over
// contexts from one builder — so there is no second notion of scope, which is the
// NIM-219 invariant. Gate (a) stays because refusing early is cheaper than resolving
// a service snapshot and rendering a template for someone who holds nothing.

// ErrCreateScopeExceeded — the create request reaches outside the caller's scope for
// `incarnation.create`: a declared coven they may not use, a service outside their
// reach, or a composed name outside it. Distinct from a gate-(a) refusal because the
// remedy differs — the operator holds create somewhere, and the request asks for
// more than that somewhere covers.
var ErrCreateScopeExceeded = errors.New("handlers: incarnation create request exceeds the caller's scope")

// ScreenIncarnationCreateScope is gate (b). name is the EFFECTIVE name (composed or
// operator-given); covens are the declared labels.
//
// Every declared coven is checked, and all must pass. No declared covens → a single
// check over `{incarnation, service}`.
//
// Shared by REST [IncarnationHandler.CreateTyped] and the MCP create tool so the two
// surfaces cannot drift — the same reason gate (a) builds its contexts from one
// function.
//
// checker nil → refusal. Fail-closed on a missing dependency, the same choice the
// per-host bind gate makes: without the checker the boundary cannot be evaluated,
// and creating regardless is exactly what the gate exists to prevent.
func ScreenIncarnationCreateScope(checker middleware.PermissionChecker, aid, name, service string, covens []string) error {
	if checker == nil || name == "" {
		return ErrCreateScopeExceeded
	}
	contexts := IncarnationCreateContexts(name, service, covens)
	if len(contexts) == 0 {
		// Nothing to measure — the request carries no dimension at all. The handler
		// answers 422 for that, but the gate must not treat it as a pass.
		return ErrCreateScopeExceeded
	}
	// AND, not OR: contexts is one entry per declared coven (or a single entry when
	// none are declared), and the caller must cover every one of them.
	for _, ctx := range contexts {
		if err := checker.Check(aid, "incarnation", "create", ctx); err != nil {
			return ErrCreateScopeExceeded
		}
	}
	return nil
}

// ScreenIncarnationCreateRosterScope is the create-path counterpart of the
// `incarnation.bind-member` route gate (NIM-371). A create that carries a roster
// writes `incarnation_membership` — the same relation `POST .../members` writes — so
// it must clear the same permission, or create becomes the way around NIM-209's gate
// (a): an operator holding only `incarnation.create` could compose a roster at birth
// that they would be refused a minute later.
//
// Same AND over the declared dimensions as [ScreenIncarnationCreateScope], over the
// same contexts from the same builder: the roster is part of the create request, so
// the scope question is asked about the incarnation being created, not about hosts
// (the per-host boundary is gate (b) of membership — [incarnation.ScreenBindCandidates]).
//
// checker nil → refusal, fail-closed like every other gate here.
func ScreenIncarnationCreateRosterScope(checker middleware.PermissionChecker, aid, name, service string, covens []string) error {
	if checker == nil || name == "" {
		return ErrCreateRosterScopeExceeded
	}
	contexts := IncarnationCreateContexts(name, service, covens)
	if len(contexts) == 0 {
		return ErrCreateRosterScopeExceeded
	}
	for _, ctx := range contexts {
		if err := checker.Check(aid, "incarnation", "bind-member", ctx); err != nil {
			return ErrCreateRosterScopeExceeded
		}
	}
	return nil
}

// ErrCreateRosterScopeExceeded — the create request declares a roster, but the caller
// does not hold `incarnation.bind-member` over what it is creating. Separate from
// [ErrCreateScopeExceeded] because the remedy differs: the operator may create this
// incarnation, just not populate it in the same breath.
var ErrCreateRosterScopeExceeded = errors.New("handlers: incarnation create roster exceeds the caller's bind-member scope")

// createScopeDetail phrases the gate-(b) refusal so the operator knows which lever
// to pull. A composed name is the interesting case — it is not in their request, so
// naming it is the only way they can tell what was judged.
func createScopeDetail(name, composedName string, covens []string) string {
	if composedName != "" {
		return "incarnation.create denied: composed name " + composedName +
			" or a declared coven is outside your scope — adjust the input components feeding id_template, or the declared covens"
	}
	if len(covens) > 0 {
		return "incarnation.create denied: " + name +
			" declares a coven outside your scope — every declared coven must be within it, and the request is refused whole rather than trimmed"
	}
	return "incarnation.create denied: " + name + " is outside your scope"
}
