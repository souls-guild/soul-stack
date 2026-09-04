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

// IncarnationResolveIDRequest — the request body of POST /v1/incarnations/
// resolve-id. Field names mirror IncarnationCreateRequest deliberately: the same
// body describes the same intended create, and gate (a) reads `service`/`covens`
// out of it with the SAME selector, so the two cannot drift on what a request means.
//
// There is no `id`: it is the answer, not a parameter. Covens does not affect the
// composition — it is here so a coven-scoped operator's permission matches on the
// preview exactly as it matches on the create.
type IncarnationResolveIDRequest struct {
	Service        string         `json:"service" required:"true" pattern:"^[a-z0-9][a-z0-9-]{0,62}$" doc:"service name from registry (ADR-029)"`
	CreateScenario string         `json:"create_scenario,omitempty" pattern:"^[a-z][a-z0-9_]*$" doc:"chosen create scenario, whose id_template composes the id"`
	Input          map[string]any `json:"input,omitempty" doc:"the create input so far — partial is expected, this is a live preview"`
	Covens         []string       `json:"covens,omitempty" pattern:"^[a-z][a-z0-9]*(-[a-z0-9]+)*$" maxLength:"63" doc:"declared environment tags of the intended create — scope parity with POST /v1/incarnations, not part of the composition"`
}

// incResolveIDInput — huma input for POST /v1/incarnations/resolve-id.
type incResolveIDInput struct {
	Body IncarnationResolveIDRequest
}

// IncarnationResolveIDReply — the native 200 body of the resolve. The struct name
// = the contract schema name.
//
// `composes: false` says the chosen scenario declares no id_template: the
// operator types the id and the form keeps its id field. Every other field is
// then zero.
//
// `valid: false` is the ordinary answer while the operator is still typing, and it
// is a 200, not a 422 — a form that errors on every keystroke has no live preview.
// `invalid_reason` always says why, so the preview is never a silently empty box;
// `composed_id` still carries the offending value when there is one, so the
// character count has something to count.
//
// The template TEXT is not in this reply on purpose: the operator is shown the
// id, not the formula (NIM-340).
//
// `available` and `taken_by_service` are meaningful only when `valid`.
// `taken_by_service` is omitted for a caller who cannot see the occupying
// incarnation — they still learn the name is taken, they just do not get a report
// on someone else's estate.
type IncarnationResolveIDReply struct {
	Composes       bool   `json:"composes" doc:"the chosen create scenario composes the id from id_template (ADR-0079); false → the operator names the incarnation"`
	ComposedID     string `json:"composed_id" doc:"the id a create with this input would produce; carries the offending value when invalid"`
	Length         int    `json:"length" doc:"character count of composed_id"`
	MaxLength      int    `json:"max_length" doc:"the incarnation id ceiling — server-sourced so the form does not restate it"`
	Valid          bool   `json:"valid" doc:"composed_id is a legal incarnation id"`
	InvalidReason  string `json:"invalid_reason,omitempty" doc:"why the id could not be composed or was rejected, in operator terms"`
	Available      bool   `json:"available" doc:"no incarnation holds this id (meaningful only when valid)"`
	TakenByService string `json:"taken_by_service,omitempty" doc:"service of the incarnation holding the id — only when the caller may see it"`
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
		res, err := incH.ResolveIDTyped(ctx, claims, handlers.ResolveIDRequest{
			Service:        in.Body.Service,
			CreateScenario: in.Body.CreateScenario,
			Input:          in.Body.Input,
			Covens:         in.Body.Covens,
		}, incH.GetInScopeFor(claims, "get"))
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
