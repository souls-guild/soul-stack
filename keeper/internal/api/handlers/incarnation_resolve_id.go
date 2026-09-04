package handlers

// Resolve of the incarnation id a create WOULD compose (`POST /v1/incarnations/
// resolve-id`, NIM-331). Creates nothing, writes nothing, audits nothing.
//
// Under `id_template` (ADR-0079) the id is composed server-side from the
// operator's `input:` components, and POST /v1/incarnations must NOT carry an
// `id`. The operator therefore never sees it until the create succeeds or
// refuses — and the id is the immutable primary key with no rename, so a wrong
// one costs a destroy and a re-create. ADR-0079 already says the create form
// "should pre-empt with a live preview and a character count"; this is that
// preview's source.
//
// It resolves rather than duplicates. The blocks of a template are CEL, and a
// client-side evaluator would coerce numbers and bools by its own rules and compose
// a DIFFERENT string from the same input: the operator would approve one identity
// and be handed another, silently. So composition stays where it already is
// ([scenario.ComposeID], shared with the create path) and the form only displays
// the answer.
//
// Occupancy is answered HERE and not by probing `GET /v1/incarnations/{id}` from
// the form. That probe is an existence oracle: a scoped operator could walk ids
// outside their scope and read their existence off the status code (the NIM-148
// shape, where a 403 leaked exactly that). The reply is scope-aware in the same
// grain the ticket set: "taken" is told to anyone who could create the id,
// "taken by service X" only to a caller who can already see that incarnation.
//
// RBAC is the create's, not a read's: permission incarnation.create, gate (a) on
// the group (same [IncarnationCreateScopeSelector] over the same body fields) and
// gate (b) — [ScreenIncarnationCreateScope] — re-measured on the COMPOSED id.
// Without gate (b) the endpoint would compose ids outside the caller's scope and
// report their occupancy, which is the oracle again by another door.

import (
	"context"
	"errors"
	"log/slog"

	"github.com/souls-guild/soul-stack/keeper/internal/api/problem"
	"github.com/souls-guild/soul-stack/keeper/internal/incarnation"
	"github.com/souls-guild/soul-stack/keeper/internal/jwt"
	"github.com/souls-guild/soul-stack/keeper/internal/scenario"
	"github.com/souls-guild/soul-stack/shared/config"
)

// ResolveIDRequest — NATIVE input of POST /v1/incarnations/resolve-id.
//
// Covens carries no weight in the composition; it is here so gate (a) scopes the
// preview exactly as it scopes the create it previews. Without it a coven-scoped
// operator would build contexts missing the `coven=` dimension their permission is
// written on, and be refused on every keystroke for a create that would succeed.
//
// There is no ID field, by construction: the id is what this call computes.
type ResolveIDRequest struct {
	Service        string
	CreateScenario string
	Input          map[string]any
	Covens         []string
}

// ResolveIDResult — NATIVE result of the resolve (handler-native; the api package
// projects it into the reply DTO).
//
// Composes=false means the chosen scenario declares no `id_template` (or the
// service offers no create scenario at all): the operator types the id, and the
// remaining fields are zero. That is an ANSWER, not a degraded state — the form
// asks the same endpoint either way and needs it to decide whether to show an id
// field or a preview.
//
// Valid=false is the ordinary state of a live preview: the operator has not
// finished typing. InvalidReason then says why in their terms, and ID still
// carries the offending string when there is one (over the ceiling, bad character)
// so the character count has something to count. Available/TakenByService are only
// meaningful when Valid.
type ResolveIDResult struct {
	Composes       bool
	ID             string
	Length         int
	MaxLength      int
	Valid          bool
	InvalidReason  string
	Available      bool
	TakenByService string
}

// ResolveIDTyped — domain function for POST /v1/incarnations/resolve-id (READ,
// no audit, no mutation). inScope is the RBAC read predicate (ADR-047, action=get)
// used ONLY to decide whether the occupant of a taken id may be named.
//
// Errors are *problemError: 422 (no service / not registered / the chosen scenario
// is not an eligible create scenario), 403 (the composed id or a declared coven
// is outside the caller's scope — the same refusal the create would give, phrased
// by the same [createScopeDetail]), 500 (snapshot or database failure).
//
// A template that will not render is NOT an error here. It comes back as
// Valid=false with a reason, because for a preview it is the normal case.
func (h *IncarnationHandler) ResolveIDTyped(ctx context.Context, claims *jwt.Claims, req ResolveIDRequest, inScope func(*incarnation.Incarnation) bool) (ResolveIDResult, error) {
	zero := ResolveIDResult{MaxLength: config.IncarnationIDMaxLen}

	if req.Service == "" {
		return zero, incProblem(problem.TypeValidationFailed, "field 'service' is required")
	}
	if !incarnation.ValidID(req.Service) {
		return zero, incProblem(problem.TypeValidationFailed, "field 'service' must match "+incarnation.IDPattern)
	}
	if claims == nil {
		return zero, incProblem(problem.TypeForbidden, "incarnation.create denied: no operator identity on the request")
	}

	// Stub mode (no service registry / no loader) is the same branch the create
	// takes: it composes nothing and honours the operator's `id`. Answering
	// "composes nothing" keeps the form's two modes agreeing with the create's two
	// modes instead of inventing a third.
	if h.services == nil || h.loader == nil {
		return zero, nil
	}
	serviceRef, ok := h.services.Resolve(req.Service)
	if !ok {
		return zero, incProblem(problem.TypeValidationFailed,
			"service "+req.Service+" is not registered (manage via service.* API, ADR-029)")
	}

	// The same choice gate the create runs, and for the same reason: `input:` — and
	// therefore the template over it — belongs to the CHOSEN scenario. A preview
	// against an ineligible choice would compose over the wrong contract.
	chosen, bare, err := scenario.ValidateCreateScenarioChoice(ctx, h.loader, serviceRef, req.CreateScenario)
	if err != nil {
		return zero, h.mapCreatePlanError("", req.Service, err)
	}
	if bare {
		// No create scenario at all → a bare incarnation, whose id the operator
		// types. Same shape as a scenario without a template.
		return zero, nil
	}

	preview, err := scenario.PreviewID(ctx, h.loader, serviceRef, chosen, req.Input)
	if err != nil {
		h.logger.Error("incarnation.resolve-id: preview failed",
			slog.String("service", req.Service), slog.String("scenario", chosen), slog.Any("error", err))
		return zero, incProblem(problem.TypeInternalError, "resolve composed id failed")
	}

	out := ResolveIDResult{
		Composes:      preview.Composes,
		ID:            preview.ID,
		Length:        len(preview.ID),
		MaxLength:     config.IncarnationIDMaxLen,
		Valid:         preview.Valid,
		InvalidReason: preview.Reason,
	}
	if !preview.Composes || !preview.Valid {
		// Nothing to measure against a scope and nothing to look up. What is
		// returned is a pure function of the caller's own input, so it discloses
		// nothing they did not supply.
		return out, nil
	}

	// Gate (b) on the COMPOSED id — the same boundary, on the same predicate, as
	// the create this previews. It is what stops the endpoint from becoming an
	// existence oracle for ids the caller could never create.
	if err := ScreenIncarnationCreateScope(h.permChecker, claims.Subject,
		preview.ID, req.Service, req.Covens); err != nil {
		return zero, incProblem(problem.TypeForbidden, createScopeDetail(preview.ID, preview.ID, req.Covens))
	}

	taken, holder, err := h.idOccupant(ctx, preview.ID, inScope)
	if err != nil {
		h.logger.Error("incarnation.resolve-id: occupancy lookup failed",
			slog.String("name", preview.ID), slog.Any("error", err))
		return zero, incProblem(problem.TypeInternalError, "resolve id availability failed")
	}
	out.Available = !taken
	out.TakenByService = holder
	return out, nil
}

// idOccupant answers "is this incarnation id already taken, and by which
// service" with the scope grain both callers need (the resolve endpoint and the
// create's 409).
//
// taken is the truth for anyone who reaches this call: they hold incarnation.create
// over this very id (gate (b) has passed on the resolve path; on the create path
// the insert has just collided), and they would learn it from the 409 regardless —
// withholding it would only replace a clear answer with a mystery.
//
// service is narrower. Naming the occupant of an id in a scope the caller cannot
// read would turn an id they merely guessed into a report about someone else's
// estate — the "I can see it ⟺ I could have been given it" invariant of
// NIM-202/203. So it is filled only when inScope admits the row, and left empty
// otherwise; the caller still learns the id is taken.
//
// A missing incarnation is (false, "", nil). Any other database failure is returned
// — the caller decides whether that is a 500 or, on the create path, a detail worth
// dropping.
func (h *IncarnationHandler) idOccupant(ctx context.Context, name string, inScope func(*incarnation.Incarnation) bool) (bool, string, error) {
	inc, err := incarnation.SelectByID(ctx, h.db, name)
	if err != nil {
		if errors.Is(err, incarnation.ErrIncarnationNotFound) {
			return false, "", nil
		}
		return false, "", err
	}
	if inc == nil {
		return false, "", nil
	}
	if inScope == nil || !inScope(inc) {
		return true, "", nil
	}
	return true, inc.Service, nil
}

// takenIDDetail phrases the create's 409 so a composed id is actionable.
//
// Under `id_template` the operator never typed the id in the refusal: they
// typed four components, and "cache-billing-invoices-redis-cache already exists"
// names a string they have not seen before and gives no hint which component to
// change — or whether the collision is even theirs. Naming the holding service
// answers that, under the same scope rule as [idOccupant]: a caller who cannot
// see the occupant still gets the plain "already exists" this always returned.
func takenIDDetail(name, holder string) string {
	if holder == "" {
		return "incarnation " + name + " already exists"
	}
	return "incarnation " + name + " already exists (service " + holder + ")"
}
