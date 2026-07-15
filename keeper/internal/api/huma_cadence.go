package api

// PILOT-1 of the OpenAPI spec-first → code-first rollout on huma v2, FULL-TYPED
// shape (ADR-054 Amendment 2026-06-12, supersedes ADR-051). Proves out ONE route —
// POST /v1/cadences — on top of chi-mux via huma as the REFERENCE for the ~20-domain
// rollout: typed input + an extracted domain function + a converter + typed output,
// without a delegating RawBody bridge.
//
// === FULL-TYPED PATTERN (rollout reference instructions) ===
//
//  1. TYPED INPUT. cadenceCreateInput{ Body cadenceCreateHumaBody } — huma decodes
//     and validates Body against the schema from the huma tags (required/enum/
//     additionalProperties: false HONEST). No RawBody — huma enforces the
//     strictness, not a domain decode.
//
//  2. EXTRACTED DOMAIN FUNCTION. CadenceHandler.CreateTyped(ctx, claims, req)
//     (reply, error) — all the business logic (RBAC guards / persist tx + notify /
//     invalidation / self-audit) without http.ResponseWriter/*http.Request. Errors
//     are *handlers.problemError (internal); the legacy Create(w,r) remains a
//     thin wrapper over CreateTyped (the strict bridge CreateCadence).
//
//  3. CONVERTER (thin). huma handler: typed input.Body → domain model
//     handlers.CadenceCreateRequest (field-by-field) → CreateTyped → domain
//     CadenceCreateReply → converted into typed cadenceCreateOutput. Both ends
//     are typed; the converter is the only "glue".
//
//  4. TYPED OUTPUT. cadenceCreateOutput{ Status 201; Location header; Body }.
//     Replaces manually writing to (w). omitempty/nullable are pinned by a
//     golden-JSON snapshot test (the rollout's wire-regression guard).
//
//  5. CLAIMS. RequireJWT (the group's chi middleware) puts claims into the
//     request context BEFORE humachi; huma passes the same ctx to the handler →
//     middleware.ClaimsFromContext(ctx) reads them directly. No context bridge /
//     Unwrap.
//
//  6. PROBLEM+JSON OVERRIDE (smart, FULL-TYPED). huma.NewError is overridden to
//     our problem format (humaProblemError). On validation-fail huma calls it
//     with errs ...error (ErrorDetails). The override detects "unexpected
//     property" → 400 TypeMalformedRequest (unknown→400, the cluster contract),
//     everything else (missing-required/enum) → 422 TypeValidationFailed.
//     installHumaErrorOverride is called ONCE explicitly in buildRouter (a
//     single point for the rollout).
//
//  7. SPEC MERGE. huma emits OpenAPI 3.1; the handwritten meta/openapi.yaml is
//     3.0.3. The pilot-1 huma fragment is NOT merged in (GET /openapi.yaml still
//     returns the handwritten 3.0.3 — the cadence path is described there, the
//     shape matches). The pilot-1 huma dump is used only in the guard test.
//     Bumping the header to 3.1 happens once, on the rollout's first merge batch.

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"

	"github.com/danielgtaylor/huma/v2"
	"github.com/danielgtaylor/huma/v2/adapters/humachi"
	"github.com/danielgtaylor/huma/v2/validation"
	"github.com/go-chi/chi/v5"

	"github.com/souls-guild/soul-stack/keeper/internal/api/handlers"
	apimiddleware "github.com/souls-guild/soul-stack/keeper/internal/api/middleware"
	"github.com/souls-guild/soul-stack/keeper/internal/api/problem"
)

// humaProblemError overrides huma.NewError to our RFC 7807 problem+json.
// Installed by installHumaErrorOverride (globally for the huma package). Any
// huma-generated error (415, body-limit, body validation) huma delivers through
// this type → the cluster's single error contract is preserved. Domain errors
// go through CreateTyped → the converter (see registerHumaCadence) and never
// reach this path.
type humaProblemError struct {
	problem.Details
}

func (e humaProblemError) Error() string { return e.Details.Detail }

// GetStatus implements huma.StatusError — huma sets this response code.
func (e humaProblemError) GetStatus() int { return e.Details.Status }

// MarshalJSON — the error body is our problem.Details (not the huma ErrorModel).
// problem.Details fields carry RFC 7807 json tags → stdlib marshal produces
// exactly our format.
func (e humaProblemError) MarshalJSON() ([]byte, error) {
	return json.Marshal(e.Details)
}

// ContentType implements huma.ContentTypeFilter — the single problem+json contract.
func (e humaProblemError) ContentType(string) string { return problem.ContentType }

// newHumaProblemError builds the problem shape from the huma status/message and
// the list of validation errors (FULL-TYPED classification, ADR-054 §Invariant-2):
//
//   - an ErrorDetail with Message == "unexpected property" is present → 400
//     TypeMalformedRequest (unknown field → 400, the cluster contract; otherwise
//     huma would return 422 — a divergence);
//   - an ErrorDetail with a query.-Location AND a parse Message is present
//     (typed-query bind, ADR-054 §Pattern fourth tier) → 400 TypeMalformedRequest
//     (bad date-time/int/bool/float query → 400; enum-mismatch does NOT fall
//     into this set → 422, see below);
//   - status 400/415 → TypeMalformedRequest (malformed body / unsupported media);
//   - other 4xx (missing-required/enum/type-mismatch, huma default 422) →
//     TypeValidationFailed;
//   - 5xx → TypeInternalError.
//
// instance is empty (huma carries no URL path in the NewError hook).
func newHumaProblemError(status int, msg string, errs []error) humaProblemError {
	if hasUnexpectedProperty(errs) {
		d := problem.New(problem.TypeMalformedRequest, "", "unknown field in request body")
		return humaProblemError{Details: d}
	}
	if hasQueryParseError(errs) {
		d := problem.New(problem.TypeMalformedRequest, "", msg)
		d.Status = http.StatusBadRequest
		return humaProblemError{Details: d}
	}
	var typ string
	switch {
	case status == http.StatusBadRequest, status == http.StatusUnsupportedMediaType:
		typ = problem.TypeMalformedRequest
	case status >= 400 && status < 500:
		typ = problem.TypeValidationFailed
	default:
		typ = problem.TypeInternalError
	}
	d := problem.New(typ, "", msg)
	d.Status = status // preserve the actual huma status (not the table default)
	return humaProblemError{Details: d}
}

// hasUnexpectedProperty scans huma's validation errors for the unknown-body-field
// marker (validation.MsgUnexpectedProperty). huma puts it into
// *huma.ErrorDetail.Message on an additionalProperties:false violation.
// If present → classify as 400.
func hasUnexpectedProperty(errs []error) bool {
	for _, e := range errs {
		if d, ok := e.(*huma.ErrorDetail); ok && d.Message == validation.MsgUnexpectedProperty {
			return true
		}
	}
	return false
}

// queryParseMessages — the exact huma messages from the typed-query parse phase
// (huma v2.38 parseInto, huma.go:1685-1769). A bad-value bind of a typed query
// parameter (time.Time/int/bool/float) is added as an ErrorDetail with Location
// `query.<name>` and ONE of these Messages, BEFORE schema validation (huma.go:
// 892-896 res.Add(pb, value, err.Error())). Enum-mismatch, on the other hand,
// comes from schema validation (validate.go:586 `s.msgEnum` = "expected value to
// be one of …") and does NOT fall into this set → it falls through to the
// default 422 branch. This is how the detector tells a parse error (400) apart
// from an enum-mismatch (422) on the same query.-Location: the discriminator is
// the Message literal, not the Location.
//
// COUPLING to huma Message literals: on a huma bump, re-check against parseInto
// (the exact errors.New(...) strings). `invalid date/time` is a PREFIX (huma
// appends the suffix " for format <fmt>"), hence the prefix match. The guard set
// (huma_audit_endpoint_test.go, especially BadSource_422) catches a detector
// regression if it drifts out of sync with huma's strings.
var queryParseMessages = []string{
	"invalid date/time",                   // huma.go:1747 "invalid date/time for format <fmt>" (prefix)
	"invalid integer",                     // huma.go:1694/1701
	"invalid boolean",                     // huma.go:1715
	"invalid float",                       // huma.go:1708
	"required query parameter is missing", // huma.go:876/887 missing required query-param → 400 (parity legacy strict RequiredParamError; incarnation destroy allow_destroy)
}

// hasQueryParseError scans huma's validation errors for a typed query parameter
// parse-fail marker: *huma.ErrorDetail with a Location prefix of `query.` AND a
// Message from queryParseMessages (prefix match). enum-mismatch on query.source
// does NOT match (its Message differs) → stays 422. See queryParseMessages for
// the coupling and the discriminator.
func hasQueryParseError(errs []error) bool {
	for _, e := range errs {
		d, ok := e.(*huma.ErrorDetail)
		if !ok || !strings.HasPrefix(d.Location, "query.") {
			continue
		}
		for _, m := range queryParseMessages {
			if strings.HasPrefix(d.Message, m) {
				return true
			}
		}
	}
	return false
}

// installHumaErrorOverride overrides the global huma.NewError with our problem
// format. Idempotent. SINGLE POINT: called ONCE explicitly in buildRouter (not
// in the huma.API factory — one install for the whole rollout, not per domain).
// huma.NewError is a package-level var, the override is global for the process
// (for all huma.API instances); this suits the rollout — a single error contract
// across the whole API.
func installHumaErrorOverride() {
	huma.NewError = func(status int, msg string, errs ...error) huma.StatusError {
		return newHumaProblemError(status, msg, errs)
	}
}

// registerHumaCadence mounts POST /v1/cadences via huma onto the given
// chi.Router (the group that already carries RequireJWT/RequirePermission/
// maxBody/metrics). cadenceH is the domain handler; nil → no-op (the
// opt-in-domain pattern from router.go).
//
// FULL-TYPED handler: huma validates the typed Body → converts to the domain
// model → CreateTyped → converts the reply into the typed output. Domain
// problem errors are delivered through humaProblemError (the same error
// contract as huma validation).
//
// IMPORTANT (path): humaAPI is created by newHumaCadenceAPI on top of the
// /v1/cadences chi group with RequirePermission(cadence.create) attached. chi
// inside the group matches RELATIVE to /v1/cadences → huma.Operation.Path = "/"
// (see cadenceCreateOperation). chi mounts the route as /v1/cadences (chi.Walk
// sees it, drift test green).
func registerHumaCadence(humaAPI huma.API, cadenceH *handlers.CadenceHandler) {
	if cadenceH == nil {
		return
	}
	huma.Register(humaAPI, cadenceCreateOperation(), func(ctx context.Context, in *cadenceCreateInput) (*cadenceCreateOutput, error) {
		claims, ok := apimiddleware.ClaimsFromContext(ctx)
		if !ok {
			return nil, huma.NewError(http.StatusInternalServerError, "missing claims")
		}
		req := toCadenceCreateRequest(in.Body)
		reply, err := cadenceH.CreateTyped(ctx, claims, req)
		if err != nil {
			return nil, cadenceProblem(err)
		}
		return &cadenceCreateOutput{
			Status:   http.StatusCreated,
			Location: reply.Location,
			Body: CadenceCreateReply{
				CadenceID: reply.CadenceID,
				Name:      reply.Name,
				Enabled:   reply.Enabled,
				NextRunAt: reply.NextRunAt,
				Location:  reply.Location,
			},
		}, nil
	})
}

// registerHumaCadencePatch mounts PATCH /v1/cadences/{id} via huma on the
// /v1/cadences chi group (WRITE-SELF-AUDIT: cadence.updated is written by the
// handler ITSELF inside PatchTyped, no audit middleware attached — unlike the
// middleware-audit domains role/operator). cadenceH nil → no-op. Handler:
// claims → convert typed body → PatchTyped (read-modify-write + self-audit) →
// 200 WITH A BODY (cadenceDTO). {id} is bound by huma, ULID validation is
// domain-side (in PatchTyped).
func registerHumaCadencePatch(humaAPI huma.API, cadenceH *handlers.CadenceHandler) {
	if cadenceH == nil {
		return
	}
	huma.Register(humaAPI, cadencePatchOperation(), func(ctx context.Context, in *cadencePatchInput) (*cadencePatchOutput, error) {
		claims, ok := apimiddleware.ClaimsFromContext(ctx)
		if !ok {
			return nil, cadenceMissingClaims()
		}
		dto, err := cadenceH.PatchTyped(ctx, claims, in.ID, toCadencePatchRequest(in.Body))
		if err != nil {
			return nil, cadenceProblem(err)
		}
		return &cadencePatchOutput{Body: dto}, nil
	})
}

// registerHumaCadenceDelete mounts DELETE /v1/cadences/{id} via huma (WRITE-SELF-
// AUDIT: cadence.deleted is written by the handler ITSELF inside DeleteTyped).
// cadenceH nil → no-op. Handler: claims → DeleteTyped (delete + invalidation +
// self-audit) → empty 204 output.
func registerHumaCadenceDelete(humaAPI huma.API, cadenceH *handlers.CadenceHandler) {
	if cadenceH == nil {
		return
	}
	huma.Register(humaAPI, cadenceDeleteOperation(), func(ctx context.Context, in *cadenceDeleteInput) (*cadenceDeleteOutput, error) {
		claims, ok := apimiddleware.ClaimsFromContext(ctx)
		if !ok {
			return nil, cadenceMissingClaims()
		}
		if err := cadenceH.DeleteTyped(ctx, claims, in.ID); err != nil {
			return nil, cadenceProblem(err)
		}
		return &cadenceDeleteOutput{Status: http.StatusNoContent}, nil
	})
}

// registerHumaCadenceEnable mounts POST /v1/cadences/{id}/enable via huma
// (WRITE-SELF-AUDIT: cadence.updated is written by the handler ITSELF inside
// SetEnabledTyped).
func registerHumaCadenceEnable(humaAPI huma.API, cadenceH *handlers.CadenceHandler) {
	if cadenceH == nil {
		return
	}
	huma.Register(humaAPI, cadenceEnableOperation(), func(ctx context.Context, in *cadenceToggleInput) (*cadenceToggleOutput, error) {
		return cadenceToggle(ctx, cadenceH, in.ID, true)
	})
}

// registerHumaCadenceDisable mounts POST /v1/cadences/{id}/disable via huma
// (WRITE-SELF-AUDIT: cadence.updated is written by the handler ITSELF).
func registerHumaCadenceDisable(humaAPI huma.API, cadenceH *handlers.CadenceHandler) {
	if cadenceH == nil {
		return
	}
	huma.Register(humaAPI, cadenceDisableOperation(), func(ctx context.Context, in *cadenceToggleInput) (*cadenceToggleOutput, error) {
		return cadenceToggle(ctx, cadenceH, in.ID, false)
	})
}

// registerHumaCadenceList mounts GET /v1/cadences (list) via huma on the
// /v1/cadences chi group (READ-with-typed-query, no audit). cadenceH nil → no-op.
// Handler: typed-query (enabled/kind enum + offset/limit int32) → ListTyped →
// typed envelope output. RBAC cadence.list is on the group. Out-of-range
// pagination is enforced by the DOMAIN (CheckPageBounds → 400), not huma
// min/max. Teardown T1: removes the last live strict-mount
// (strictWrapper.ListCadences) on /v1.
func registerHumaCadenceList(humaAPI huma.API, cadenceH *handlers.CadenceHandler) {
	if cadenceH == nil {
		return
	}
	huma.Register(humaAPI, cadenceListOperation(), func(ctx context.Context, in *cadenceListInput) (*cadenceListOutput, error) {
		reply, err := cadenceH.ListTyped(ctx, in.Enabled, in.Kind, int(in.Offset), int(in.Limit))
		if err != nil {
			return nil, cadenceProblem(err)
		}
		return &cadenceListOutput{Body: reply}, nil
	})
}

// registerHumaCadenceGet mounts GET /v1/cadences/{id} via huma (READ-with-path,
// no audit). cadenceH nil → no-op. Handler: GetTyped(id) → typed 200 output
// (404/422/500 via problem). RBAC cadence.list is on the group. This migration
// completes the cadence domain on huma and removes the sibling-subrouter
// blocker (see cadenceGetOperation).
func registerHumaCadenceGet(humaAPI huma.API, cadenceH *handlers.CadenceHandler) {
	if cadenceH == nil {
		return
	}
	huma.Register(humaAPI, cadenceGetOperation(), func(ctx context.Context, in *cadenceGetInput) (*cadenceGetOutput, error) {
		dto, err := cadenceH.GetTyped(ctx, in.ID)
		if err != nil {
			return nil, cadenceProblem(err)
		}
		return &cadenceGetOutput{Body: dto}, nil
	})
}

// registerHumaCadenceRuns mounts GET /v1/cadences/{id}/runs via huma (READ-with-
// typed-query, no audit). cadenceH nil → no-op. Handler: typed-query → RunsTyped →
// typed envelope output. RBAC incarnation.history is on the group.
// CheckPageBounds → 400 (the range is enforced by the DOMAIN, not huma min/max).
func registerHumaCadenceRuns(humaAPI huma.API, cadenceH *handlers.CadenceHandler) {
	if cadenceH == nil {
		return
	}
	huma.Register(humaAPI, cadenceRunsOperation(), func(ctx context.Context, in *cadenceRunsInput) (*cadenceRunsOutput, error) {
		reply, err := cadenceH.RunsTyped(ctx, in.ID, in.Statuses, int(in.Offset), int(in.Limit))
		if err != nil {
			return nil, cadenceProblem(err)
		}
		return &cadenceRunsOutput{Body: reply}, nil
	})
}

// cadenceToggle — the shared enable/disable branch (SetEnabledTyped + 200 body).
// claims from ctx → SetEnabledTyped (self-audit) → cadenceToggleOutput.
func cadenceToggle(ctx context.Context, cadenceH *handlers.CadenceHandler, id string, enabled bool) (*cadenceToggleOutput, error) {
	claims, ok := apimiddleware.ClaimsFromContext(ctx)
	if !ok {
		return nil, cadenceMissingClaims()
	}
	reply, err := cadenceH.SetEnabledTyped(ctx, claims, id, enabled)
	if err != nil {
		return nil, cadenceProblem(err)
	}
	return &cadenceToggleOutput{Body: CadenceEnabledReply{CadenceID: reply.CadenceID, Enabled: reply.Enabled}}, nil
}

// cadenceMissingClaims — a defensive response for a missing claims in ctx
// (unreachable: RequireJWT puts claims in before huma). problem+json (parity
// roleMissingClaims).
func cadenceMissingClaims() huma.StatusError {
	return humaProblemError{Details: problem.New(problem.TypeInternalError, "", "missing claims")}
}

// toCadencePatchRequest — converts the typed huma PATCH body → the domain model
// handlers.CadencePatchRequest (field-by-field, FULL-TYPED §Pattern step 3).
// Pointers are passed through as-is (nil = "leave untouched"); target is
// converted to the domain VoyageTargetRequest (nil → nil = "leave untouched").
// The domain applyCadencePatch treats presence the same way the legacy
// (w,r)-Patch decode did.
func toCadencePatchRequest(b CadencePatchRequest) handlers.CadencePatchRequest {
	req := handlers.CadencePatchRequest{
		Name:            b.Name,
		Enabled:         b.Enabled,
		ScheduleKind:    b.ScheduleKind,
		IntervalSeconds: b.IntervalSeconds,
		CronExpr:        b.CronExpr,
		OverlapPolicy:   b.OverlapPolicy,
		ScenarioName:    b.ScenarioName,
		Module:          b.Module,
		Input:           b.Input,
		Batch:           b.Batch,
		BatchSize:       b.BatchSize,
		BatchPercent:    b.BatchPercent,
		Concurrency:     b.Concurrency,
		BatchMode:       b.BatchMode,
		MaxFailures:     b.MaxFailures,
		FailThreshold:   b.FailThreshold,
		RequireAlive:    b.RequireAlive,
		OnFailure:       b.OnFailure,
	}
	if b.Target != nil {
		req.Target = &handlers.VoyageTargetRequest{
			Incarnations: b.Target.Incarnations,
			Service:      b.Target.Service,
			SIDs:         b.Target.SIDs,
			Where:        b.Target.Where,
			Coven:        b.Target.Coven,
		}
	}
	return req
}

// toCadenceCreateRequest — converts the typed huma body → the domain model
// (field-by-field, FULL-TYPED §Pattern step 3). Thin glue; the domain model
// shape is handlers.CadenceCreateRequest (an alias of the domain type, the
// same one the legacy Create decoded into).
func toCadenceCreateRequest(b CadenceCreateRequest) handlers.CadenceCreateRequest {
	return handlers.CadenceCreateRequest{
		Name:            b.Name,
		Enabled:         b.Enabled,
		ScheduleKind:    b.ScheduleKind,
		IntervalSeconds: b.IntervalSeconds,
		CronExpr:        b.CronExpr,
		OverlapPolicy:   b.OverlapPolicy,
		Kind:            b.Kind,
		ScenarioName:    b.ScenarioName,
		Module:          b.Module,
		Input:           b.Input,
		Target: &handlers.VoyageTargetRequest{
			Incarnations: b.Target.Incarnations,
			Service:      b.Target.Service,
			SIDs:         b.Target.SIDs,
			Where:        b.Target.Where,
			Coven:        b.Target.Coven,
		},
		Batch:                b.Batch,
		BatchSize:            b.BatchSize,
		BatchPercent:         b.BatchPercent,
		Concurrency:          b.Concurrency,
		BatchMode:            b.BatchMode,
		MaxFailures:          b.MaxFailures,
		FailThreshold:        b.FailThreshold,
		InterBatchIntervalMS: b.InterBatchIntervalMS,
		InterUnitIntervalMS:  b.InterUnitIntervalMS,
		RequireAlive:         b.RequireAlive,
		OnFailure:            b.OnFailure,
		Notify:               toNotifyRequests(b.Notify),
	}
}

// toNotifyRequests converts the huma notify[] into the domain shape (parity
// §Pattern). nil/empty → nil (no notifications).
func toNotifyRequests(in []VoyageNotify) []handlers.VoyageNotifyRequest {
	if len(in) == 0 {
		return nil
	}
	out := make([]handlers.VoyageNotifyRequest, len(in))
	for i, n := range in {
		out[i] = handlers.VoyageNotifyRequest{
			Herald:       n.Herald,
			On:           n.On,
			OnlyFailures: derefBool(n.OnlyFailures),
			OnlyChanges:  derefBool(n.OnlyChanges),
			Annotations:  marshalAnnotations(n.Annotations),
			Projection:   n.Projection,
		}
	}
	return out
}

// derefBool — *bool → bool (nil → false). The domain VoyageNotifyRequest carries
// bool (omitempty), the huma shape is *bool (to distinguish "not set" in the
// schema); the semantics are the same (absence = false).
func derefBool(p *bool) bool {
	return p != nil && *p
}

// cadenceProblem delivers a CreateTyped error through huma as problem+json. The
// domain *handlers.problemError → humaProblemError (its Details, status from the
// table). A non-problem error (an off-path case) → 500 internal.
func cadenceProblem(err error) huma.StatusError {
	if d, ok := handlers.AsProblemDetails(err); ok {
		return humaProblemError{Details: d}
	}
	return humaProblemError{Details: problem.New(problem.TypeInternalError, "", "internal error")}
}

// newHumaCadenceAPI assembles a huma.API on top of a chi group. OpenAPIPath/
// DocsPath/SchemasPath are empty — huma does NOT mount its own spec/docs/schemas
// routes (the spec is served by servedOpenAPIHandler). installHumaErrorOverride
// is NOT called here — it runs as ONE explicit call in buildRouter (a single
// point for the rollout). The rolled-out part: one huma.API per chi group with
// identical wiring.
func newHumaCadenceAPI(r chi.Router) huma.API {
	cfg := huma.DefaultConfig("Soul Stack Keeper Operator API", "v1")
	cfg.OpenAPIPath = "" // do not mount the huma spec route (the spec is served by servedOpenAPIHandler)
	cfg.DocsPath = ""    // do not mount huma-docs
	cfg.SchemasPath = "" // do not mount huma-schemas
	// Remove the SchemaLinkTransformer (a huma default via CreateHooks): it adds
	// a "$schema" field + a Link header to the response body (JSON-Schema
	// self-describe). This is a wire CHANGE against the legacy oapi reply (it
	// has none there) — the golden-JSON guard catches it. For the rollout: a
	// single "bare" envelope without huma body decorations. CreateHooks=nil
	// also removes the other default hooks (we don't need them — huma doesn't
	// mount spec/docs).
	cfg.CreateHooks = nil
	api := humachi.New(r, cfg)
	// INCARNATION (handler-native T5d): enum IncarnationStatus is a native
	// SchemaProvider type (huma_enums.go/huma_incarnation_status.go), carried
	// directly in the reply Body → a separate IncarnationStatus → native alias
	// is no longer needed (no oapi-status field in Body).
	// VOYAGE/CADENCE (handler-native T5d): OUTPUT structs (Voyage.target —
	// native api.VoyageTarget, CadenceDTO.target — json.RawMessage) no longer
	// pull in the generated VoyageTarget → the former aliasVoyageTarget alias
	// was removed (see huma_voyage_target.go).
	// Global alias: generic sharedapi.PagedResponse[<incarnation-element>] → a
	// named-struct envelope with the contract name/shape (IncarnationListReply/
	// IncarnationHistoryReply). Changes only the list/history OpenAPI Body
	// schema, not the wire body (same json fields). See
	// huma_incarnation_envelope.go.
	registerIncarnationEnvelopes(api)
	// Rollout batch N1 (operator/service): generic PagedResponse[Operator] →
	// OperatorListReply (see huma_operator_envelope.go); handlers.
	// ServiceScenariosReply → ServiceScenariosListReply (see
	// huma_service_envelope.go). Changes only the OpenAPI name/shape of Body,
	// not the wire.
	registerOperatorEnvelopes(api)
	registerServiceEnvelopes(api)
	// Rollout batch N4: generic PagedResponse[Voyage] (cadence runs Body) →
	// VoyageListReply (see huma_cadence_envelope.go). Aligns the runs response
	// onto the same named schema VoyageListReply as voyage list (handwritten
	// spec :2378). Only the OpenAPI name/shape of Body, not the wire.
	registerCadenceEnvelopes(api)
	// SOUL (handler-native T5d): enum SoulStatus/SoulTransport are native
	// SchemaProvider types (huma_soul_status.go), carried directly in the reply
	// Body → a separate alias is not needed. generic PagedResponse[handlers.
	// SoulListView] → soulListReply (CURSOR, the 6-field SoulListReply shape,
	// see huma_soul_envelope.go). nested SoulSshTarget — native input↔output
	// (CLASS A). Only OpenAPI schemas, not the wire.
	registerSoulEnvelopes(api)
	// Class C additional emission: typed schema SoulprintFacts (+ 6
	// sub-schemas) for GET soulprint (typed_facts=json.RawMessage doesn't
	// surface nested types via reflect walk; an alias on typed *SoulprintFacts
	// emits them) + ErrandAccepted (the 202 body of exec/errand-get is
	// marshaled via json.RawMessage → the schema wasn't emitted). Only
	// OpenAPI, not the wire. See huma_soul_soulprint.go / huma_errand_accepted.go.
	registerSoulprintFacts(api)
	registerErrandAccepted(api)
	return api
}

// HumaCadenceSpecYAML assembles the OpenAPI fragment of the cadence routes
// migrated to huma (pilot-1 — createCadence only) as a YAML string, WITHOUT
// mounting on the real router. A hook for the rollout's spec-merge target and
// guard/golden tests. Delegates to the generic [humaDumpSpec], registering the
// operation via the same registerHumaCadence (a single register path — no
// dump-vs-mount duplication): the handler stub is not called during dump.
// Returns a 3.1.0 spec (huma default).
func HumaCadenceSpecYAML() (string, error) {
	return humaDumpSpec(func(api huma.API) error {
		stub := handlers.CadenceSpecStub()
		registerHumaCadence(api, stub)
		registerHumaCadenceList(api, stub)
		registerHumaCadenceGet(api, stub)
		registerHumaCadenceRuns(api, stub)
		registerHumaCadencePatch(api, stub)
		registerHumaCadenceDelete(api, stub)
		registerHumaCadenceEnable(api, stub)
		registerHumaCadenceDisable(api, stub)
		return nil
	})
}

// marshalAnnotations — JSON-serializes the huma-shaped annotations into the
// domain RawMessage (the domain prepareNotifyErr validates the object shape).
// nil → nil.
func marshalAnnotations(m map[string]any) []byte {
	if len(m) == 0 {
		return nil
	}
	// Safe: map[string]any always serializes; an error is impossible.
	b, _ := json.Marshal(m)
	return b
}
