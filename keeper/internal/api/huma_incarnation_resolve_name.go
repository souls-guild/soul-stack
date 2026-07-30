package api

// FULL-TYPED shape of the composed-name RESOLVE route (code-first OpenAPI source,
// ADR-054 §Pattern). POST /v1/incarnations/resolve-name — what name would a create
// with this input compose, is it a legal name, and is it free (NIM-331).
//
// A resolve, not a mutation: nothing is created, nothing is stored, audit is NOT
// wired. POST rather than GET because the answer is computed from `input:` — an
// arbitrary nested object that does not fit a query string and has no business in
// access logs. Same shape and same reason as .../form-prefill.
//
// RBAC is the CREATE's, on the group: permission incarnation.create with the same
// [handlers.IncarnationCreateScopeSelector] gate (a), and gate (b) re-measured on
// the composed name inside ResolveNameTyped. A read permission would be the wrong
// tier — the reply says whether a name is free, and only someone who could take it
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

// IncarnationResolveNameRequest — the request body of POST /v1/incarnations/
// resolve-name. Field names mirror IncarnationCreateRequest deliberately: the same
// body describes the same intended create, and gate (a) reads `service`/`covens`
// out of it with the SAME selector, so the two cannot drift on what a request means.
//
// There is no `name`: it is the answer, not a parameter. Covens does not affect the
// composition — it is here so a coven-scoped operator's permission matches on the
// preview exactly as it matches on the create.
type IncarnationResolveNameRequest struct {
	Service        string         `json:"service" required:"true" pattern:"^[a-z0-9][a-z0-9-]{0,62}$" doc:"service name from registry (ADR-029)"`
	CreateScenario string         `json:"create_scenario,omitempty" pattern:"^[a-z][a-z0-9_]*$" doc:"chosen create scenario, whose name_template composes the name"`
	Input          map[string]any `json:"input,omitempty" doc:"the create input so far — partial is expected, this is a live preview"`
	Covens         []string       `json:"covens,omitempty" pattern:"^[a-z][a-z0-9]*(-[a-z0-9]+)*$" maxLength:"63" doc:"declared environment tags of the intended create — scope parity with POST /v1/incarnations, not part of the composition"`
}

// incResolveNameInput — huma input for POST /v1/incarnations/resolve-name.
type incResolveNameInput struct {
	Body IncarnationResolveNameRequest
}

// IncarnationResolveNameReply — the native 200 body of the resolve. The struct name
// = the contract schema name.
//
// `composes: false` says the chosen scenario declares no name_template: the
// operator types the name and the form keeps its name field. Every other field is
// then zero.
//
// `valid: false` is the ordinary answer while the operator is still typing, and it
// is a 200, not a 422 — a form that errors on every keystroke has no live preview.
// `invalid_reason` always says why, so the preview is never a silently empty box;
// `composed_name` still carries the offending value when there is one, so the
// character count has something to count.
//
// The template TEXT is not in this reply on purpose: the operator is shown the
// name, not the formula (NIM-340).
//
// `available` and `taken_by_service` are meaningful only when `valid`.
// `taken_by_service` is omitted for a caller who cannot see the occupying
// incarnation — they still learn the name is taken, they just do not get a report
// on someone else's estate.
type IncarnationResolveNameReply struct {
	Composes       bool   `json:"composes" doc:"the chosen create scenario composes the name from name_template (ADR-0079); false → the operator names the incarnation"`
	ComposedName   string `json:"composed_name" doc:"the name a create with this input would produce; carries the offending value when invalid"`
	Length         int    `json:"length" doc:"character count of composed_name"`
	MaxLength      int    `json:"max_length" doc:"the incarnation name ceiling — server-sourced so the form does not restate it"`
	Valid          bool   `json:"valid" doc:"composed_name is a legal incarnation name"`
	InvalidReason  string `json:"invalid_reason,omitempty" doc:"why the name could not be composed or was rejected, in operator terms"`
	Available      bool   `json:"available" doc:"no incarnation holds this name (meaningful only when valid)"`
	TakenByService string `json:"taken_by_service,omitempty" doc:"service of the incarnation holding the name — only when the caller may see it"`
}

// incResolveNameOutput — huma output for the resolve (FULL-TYPED).
type incResolveNameOutput struct {
	Body IncarnationResolveNameReply
}

// incResolveNameOperation — metadata for POST /v1/incarnations/resolve-name.
// DefaultStatus=200. A resolve (no mutation, no state): audit is NOT wired.
// Permission incarnation.create. Errors: 403 scope, 422 service/scenario, 500.
func incResolveNameOperation() huma.Operation {
	return huma.Operation{
		OperationID:   "resolveIncarnationName",
		Method:        http.MethodPost,
		Path:          "/resolve-name",
		Summary:       "Resolve the name a create would compose",
		Description:   "Live preview for the create form: composes the incarnation name from the chosen create scenario's name_template over the input so far, reports its length against the ceiling, and whether the name is free. Composition runs server-side — the same code the create runs — so the previewed name cannot differ from the created one. Creates nothing. Permission incarnation.create; the composed name is measured against the caller's scope, and the occupying service is named only to a caller who may see it.",
		Tags:          []string{"incarnation"},
		DefaultStatus: http.StatusOK,
		Errors:        []int{http.StatusForbidden, http.StatusUnprocessableEntity, http.StatusInternalServerError},
	}
}

// registerHumaIncarnationResolveName mounts POST /v1/incarnations/resolve-name via
// huma (READ resolve, no audit). The inScope predicate (ADR-047, action=get) gates
// ONLY whether the occupant of a taken name may be named. incH nil → no-op.
func registerHumaIncarnationResolveName(humaAPI huma.API, incH *handlers.IncarnationHandler) {
	if incH == nil {
		return
	}
	huma.Register(humaAPI, incResolveNameOperation(), func(ctx context.Context, in *incResolveNameInput) (*incResolveNameOutput, error) {
		claims, ok := apimiddleware.ClaimsFromContext(ctx)
		if !ok {
			return nil, incMissingClaims()
		}
		res, err := incH.ResolveNameTyped(ctx, claims, handlers.ResolveNameRequest{
			Service:        in.Body.Service,
			CreateScenario: in.Body.CreateScenario,
			Input:          in.Body.Input,
			Covens:         in.Body.Covens,
		}, incH.GetInScopeFor(claims, "get"))
		if err != nil {
			return nil, incProblem(err)
		}
		return &incResolveNameOutput{Body: IncarnationResolveNameReply{
			Composes:       res.Composes,
			ComposedName:   res.Name,
			Length:         res.Length,
			MaxLength:      res.MaxLength,
			Valid:          res.Valid,
			InvalidReason:  res.InvalidReason,
			Available:      res.Available,
			TakenByService: res.TakenByService,
		}}, nil
	})
}
