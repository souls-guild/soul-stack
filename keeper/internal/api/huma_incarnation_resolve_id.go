package api

// FULL-TYPED shape of the composed-id RESOLVE route (code-first OpenAPI source,
// ADR-054 §Pattern). POST /v1/incarnations/resolve-id — what id would a create
// with this input compose, is it a legal id, and is it free (NIM-331).
//
// A resolve, not a mutation: nothing is created, nothing is stored, audit is NOT
// wired. POST rather than GET because the answer is computed from `input:` — an
// arbitrary nested object that does not fit a query string and has no business in
// access logs. Same shape and same reason as .../form-prefill.
//
// RBAC is the CREATE's, on the group: permission incarnation.create with the same
// [handlers.IncarnationCreateScopeSelector] gate (a), and gate (b) re-measured on
// the composed id inside ResolveIDTyped. A read permission would be the wrong
// tier — the reply says whether an id is free, and only someone who could take it
// has business asking.
//
// Go types — the single source of truth for the schema.

import (
	"context"
	"net/http"

	"github.com/danielgtaylor/huma/v2"

	"github.com/souls-guild/soul-stack/keeper/internal/api/handlers"
	apimiddleware "github.com/souls-guild/soul-stack/keeper/internal/api/middleware"
)

// incResolveIDInput — huma input for POST /v1/incarnations/resolve-id.
type incResolveIDInput struct {
	Body IncarnationResolveIDRequest
}

// incResolveIDOutput — huma output for the resolve (FULL-TYPED).
type incResolveIDOutput struct {
	Body IncarnationResolveIDReply
}

// incResolveIDOperation — metadata for POST /v1/incarnations/resolve-id.
// DefaultStatus=200. A resolve (no mutation, no state): audit is NOT wired.
// Permission incarnation.create. Errors: 403 scope, 422 service/scenario, 500.
func incResolveIDOperation() huma.Operation {
	return huma.Operation{
		OperationID:   "resolveIncarnationID",
		Method:        http.MethodPost,
		Path:          "/resolve-id",
		Summary:       "Resolve the id a create would compose",
		Description:   "Live preview for the create form: composes the incarnation id from the chosen create scenario's id_template over the input so far, reports its length against the ceiling, and whether the id is free. Composition runs server-side — the same code the create runs — so the previewed id cannot differ from the created one. Creates nothing. Permission incarnation.create; the composed id is measured against the caller's scope, and the occupying service is named only to a caller who may see it.",
		Tags:          []string{"incarnation"},
		DefaultStatus: http.StatusOK,
		Errors:        []int{http.StatusForbidden, http.StatusUnprocessableEntity, http.StatusInternalServerError},
	}
}

// registerHumaIncarnationResolveID mounts POST /v1/incarnations/resolve-id via
// huma (READ resolve, no audit). The inScope predicate (ADR-047, action=get) gates
// ONLY whether the occupant of a taken id may be named. incH nil → no-op.
func registerHumaIncarnationResolveID(humaAPI huma.API, incH *handlers.IncarnationHandler) {
	if incH == nil {
		return
	}
	huma.Register(humaAPI, incResolveIDOperation(), func(ctx context.Context, in *incResolveIDInput) (*incResolveIDOutput, error) {
		claims, ok := apimiddleware.ClaimsFromContext(ctx)
		if !ok {
			return nil, incMissingClaims()
		}
		res, err := incH.ResolveIDTyped(ctx, claims, toResolveIDRequest(in.Body), incH.GetInScopeFor(claims, "get"))
		if err != nil {
			return nil, incProblem(err)
		}
		return &incResolveIDOutput{Body: IncarnationResolveIDReply{
			Composes:       res.Composes,
			ComposedID:     res.ID,
			Length:         res.Length,
			MaxLength:      res.MaxLength,
			Valid:          res.Valid,
			InvalidReason:  res.InvalidReason,
			Available:      res.Available,
			TakenByService: res.TakenByService,
		}}, nil
	})
}
