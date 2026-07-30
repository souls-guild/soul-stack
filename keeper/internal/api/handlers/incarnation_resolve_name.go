package handlers

// Resolve of the incarnation name a create WOULD compose (`POST /v1/incarnations/
// resolve-name`, NIM-331). Creates nothing, writes nothing, audits nothing.
//
// Under `name_template` (ADR-0079) the name is composed server-side from the
// operator's `input:` components, and POST /v1/incarnations must NOT carry a
// `name`. The operator therefore never sees the name until the create succeeds or
// refuses — and the name is the immutable primary key with no rename, so a wrong
// one costs a destroy and a re-create. ADR-0079 already says the create form
// "should pre-empt with a live preview and a character count"; this is that
// preview's source.
//
// It resolves rather than duplicates. The blocks of a template are CEL, and a
// client-side evaluator would coerce numbers and bools by its own rules and compose
// a DIFFERENT string from the same input: the operator would approve one identity
// and be handed another, silently. So composition stays where it already is
// ([scenario.ComposeName], shared with the create path) and the form only displays
// the answer.
//
// Occupancy is answered HERE and not by probing `GET /v1/incarnations/{name}` from
// the form. That probe is an existence oracle: a scoped operator could walk names
// outside their scope and read their existence off the status code (the NIM-148
// shape, where a 403 leaked exactly that). The reply is scope-aware in the same
// grain the ticket set: "taken" is told to anyone who could create the name,
// "taken by service X" only to a caller who can already see that incarnation.
//
// RBAC is the create's, not a read's: permission incarnation.create, gate (a) on
// the group (same [IncarnationCreateScopeSelector] over the same body fields) and
// gate (b) — [ScreenIncarnationCreateScope] — re-measured on the COMPOSED name.
// Without gate (b) the endpoint would compose names outside the caller's scope and
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

// ResolveNameRequest — NATIVE input of POST /v1/incarnations/resolve-name.
//
// Covens carries no weight in the composition; it is here so gate (a) scopes the
// preview exactly as it scopes the create it previews. Without it a coven-scoped
// operator would build contexts missing the `coven=` dimension their permission is
// written on, and be refused on every keystroke for a create that would succeed.
//
// There is no Name field, by construction: the name is what this call computes.
type ResolveNameRequest struct {
	Service        string
	CreateScenario string
	Input          map[string]any
	Covens         []string
}

// ResolveNameResult — NATIVE result of the resolve (handler-native; the api package
// projects it into the reply DTO).
//
// Composes=false means the chosen scenario declares no `name_template` (or the
// service offers no create scenario at all): the operator types the name, and the
// remaining fields are zero. That is an ANSWER, not a degraded state — the form
// asks the same endpoint either way and needs it to decide whether to show a name
// field or a preview.
//
// Valid=false is the ordinary state of a live preview: the operator has not
// finished typing. InvalidReason then says why in their terms, and Name still
// carries the offending string when there is one (over the ceiling, bad character)
// so the character count has something to count. Available/TakenByService are only
// meaningful when Valid.
type ResolveNameResult struct {
	Composes       bool
	Name           string
	Length         int
	MaxLength      int
	Valid          bool
	InvalidReason  string
	Available      bool
	TakenByService string
}

// ResolveNameTyped — domain function for POST /v1/incarnations/resolve-name (READ,
// no audit, no mutation). inScope is the RBAC read predicate (ADR-047, action=get)
// used ONLY to decide whether the occupant of a taken name may be named.
//
// Errors are *problemError: 422 (no service / not registered / the chosen scenario
// is not an eligible create scenario), 403 (the composed name or a declared coven
// is outside the caller's scope — the same refusal the create would give, phrased
// by the same [createScopeDetail]), 500 (snapshot or database failure).
//
// A template that will not render is NOT an error here. It comes back as
// Valid=false with a reason, because for a preview it is the normal case.
func (h *IncarnationHandler) ResolveNameTyped(ctx context.Context, claims *jwt.Claims, req ResolveNameRequest, inScope func(*incarnation.Incarnation) bool) (ResolveNameResult, error) {
	zero := ResolveNameResult{MaxLength: config.IncarnationNameMaxLen}

	if req.Service == "" {
		return zero, incProblem(problem.TypeValidationFailed, "field 'service' is required")
	}
	if !incarnation.ValidName(req.Service) {
		return zero, incProblem(problem.TypeValidationFailed, "field 'service' must match "+incarnation.NamePattern)
	}
	if claims == nil {
		return zero, incProblem(problem.TypeForbidden, "incarnation.create denied: no operator identity on the request")
	}

	// Stub mode (no service registry / no loader) is the same branch the create
	// takes: it composes nothing and honours the operator's `name`. Answering
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
		// No create scenario at all → a bare incarnation, whose name the operator
		// types. Same shape as a scenario without a template.
		return zero, nil
	}

	preview, err := scenario.PreviewName(ctx, h.loader, serviceRef, chosen, req.Input)
	if err != nil {
		h.logger.Error("incarnation.resolve-name: preview failed",
			slog.String("service", req.Service), slog.String("scenario", chosen), slog.Any("error", err))
		return zero, incProblem(problem.TypeInternalError, "resolve composed name failed")
	}

	out := ResolveNameResult{
		Composes:      preview.Composes,
		Name:          preview.Name,
		Length:        len(preview.Name),
		MaxLength:     config.IncarnationNameMaxLen,
		Valid:         preview.Valid,
		InvalidReason: preview.Reason,
	}
	if !preview.Composes || !preview.Valid {
		// Nothing to measure against a scope and nothing to look up. What is
		// returned is a pure function of the caller's own input, so it discloses
		// nothing they did not supply.
		return out, nil
	}

	// Gate (b) on the COMPOSED name — the same boundary, on the same predicate, as
	// the create this previews. It is what stops the endpoint from becoming an
	// existence oracle for names the caller could never create.
	if err := ScreenIncarnationCreateScope(h.permChecker, claims.Subject,
		preview.Name, req.Service, req.Covens); err != nil {
		return zero, incProblem(problem.TypeForbidden, createScopeDetail(preview.Name, preview.Name, req.Covens))
	}

	taken, holder, err := h.nameOccupant(ctx, preview.Name, inScope)
	if err != nil {
		h.logger.Error("incarnation.resolve-name: occupancy lookup failed",
			slog.String("name", preview.Name), slog.Any("error", err))
		return zero, incProblem(problem.TypeInternalError, "resolve name availability failed")
	}
	out.Available = !taken
	out.TakenByService = holder
	return out, nil
}

// nameOccupant answers "is this incarnation name already taken, and by which
// service" with the scope grain both callers need (the resolve endpoint and the
// create's 409).
//
// taken is the truth for anyone who reaches this call: they hold incarnation.create
// over this very name (gate (b) has passed on the resolve path; on the create path
// the insert has just collided), and they would learn it from the 409 regardless —
// withholding it would only replace a clear answer with a mystery.
//
// service is narrower. Naming the occupant of a name in a scope the caller cannot
// read would turn a name they merely guessed into a report about someone else's
// estate — the "I can see it ⟺ I could have been given it" invariant of
// NIM-202/203. So it is filled only when inScope admits the row, and left empty
// otherwise; the caller still learns the name is taken.
//
// A missing incarnation is (false, "", nil). Any other database failure is returned
// — the caller decides whether that is a 500 or, on the create path, a detail worth
// dropping.
func (h *IncarnationHandler) nameOccupant(ctx context.Context, name string, inScope func(*incarnation.Incarnation) bool) (bool, string, error) {
	inc, err := incarnation.SelectByName(ctx, h.db, name)
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

// takenNameDetail phrases the create's 409 so a composed name is actionable.
//
// Under `name_template` the operator never typed the name in the refusal: they
// typed four components, and "cache-billing-invoices-redis-cache already exists"
// names a string they have not seen before and gives no hint which component to
// change — or whether the collision is even theirs. Naming the holding service
// answers that, under the same scope rule as [nameOccupant]: a caller who cannot
// see the occupant still gets the plain "already exists" this always returned.
func takenNameDetail(name, holder string) string {
	if holder == "" {
		return "incarnation " + name + " already exists"
	}
	return "incarnation " + name + " already exists (service " + holder + ")"
}
