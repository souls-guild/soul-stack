package api

// Registration and spec-dump of GET /v1/audit on huma full-typed (ADR-054 §Pattern
// FOURTH tier — read-with-typed-query). A READ variant (WITHOUT audit-middleware:
// reading audit_log produces no audit event itself). huma validates the typed query →
// convert into the domain handlers.AuditListFilter → ListTyped → typed envelope output.
// Domain problem errors (invalid source → 422) are delivered via
// humaProblemError with the same error contract as huma-bind (bad date-time/int → 400).

import (
	"context"

	"github.com/danielgtaylor/huma/v2"

	"github.com/souls-guild/soul-stack/keeper/internal/api/handlers"
	"github.com/souls-guild/soul-stack/keeper/internal/api/problem"
)

// registerHumaAuditList mounts GET /v1/audit via huma on the passed chi.Router
// (the group already carrying RequireJWT/RequirePermission(audit.read)/maxBody/metrics).
// auditH — the domain handler; nil → no-op (the opt-in-domain pattern of router.go: the route
// is wired only when auditH is non-nil).
//
// The READ variant of the tier: huma binds/validates the typed query (bad date-time/int → 400
// via error-override; bad source-enum → 422), converts into handlers.AuditListFilter
// → ListTyped → typed output. Without reading claims (audit.read is attached on the group; the AID
// filter comes as a query parameter, not from the JWT).
func registerHumaAuditList(humaAPI huma.API, auditH *handlers.AuditHandler) {
	if auditH == nil {
		return
	}
	huma.Register(humaAPI, auditListOperation(), func(ctx context.Context, in *auditListInput) (*auditListOutput, error) {
		reply, err := auditH.ListTyped(ctx, toAuditListFilter(in))
		if err != nil {
			return nil, auditProblem(err)
		}
		return &auditListOutput{Body: newAuditEventListReply(reply)}, nil
	})
}

// toAuditListFilter — converts the typed huma input → domain handlers.AuditListFilter
// (ADR-054 §Pattern step 3, thin glue). huma sets a zero-time StartedAfter/Before
// when the parameter is omitted (it does not call parseInto) → the domain ListTyped treats
// IsZero as "no time boundary" (parity with the legacy `if param != ""`). huma already
// substituted the default Offset/Limit (0 / 50) when omitted — matching ParsePage.
// int32→int — a widening cast without loss (pagination ≤ int32 range); the bounds
// (offset≥0, limit∈[1,1000]) are checked by the domain CheckPageBounds → 400 (NOT huma).
func toAuditListFilter(in *auditListInput) handlers.AuditListFilter {
	return handlers.AuditListFilter{
		Types:         in.Types,
		Sources:       in.Sources,
		ArchonAID:     in.ArchonAID,
		CorrelationID: in.CorrelationID,
		PayloadHerald: in.PayloadHerald,
		PayloadVoyage: in.PayloadVoyage,
		StartedAfter:  in.StartedAfter,
		StartedBefore: in.StartedBefore,
		Offset:        int(in.Offset),
		Limit:         int(in.Limit),
	}
}

// auditProblem delivers a ListTyped error through huma as problem+json. A domain
// *handlers.problemError → humaProblemError (its Details, status from the table: invalid
// source → 422; DB failure → 500). Non-problem (an unexpected path) → 500 internal (parity
// roleProblem / cadenceProblem).
func auditProblem(err error) huma.StatusError {
	if d, ok := handlers.AsProblemDetails(err); ok {
		return humaProblemError{Details: d}
	}
	return humaProblemError{Details: problem.New(problem.TypeInternalError, "", "internal error")}
}

// HumaAuditSpecYAML builds the OpenAPI fragment of the huma-migrated GET /v1/audit as
// a YAML string, WITHOUT mounting on the real router. A hook for the rollout spec-merge target
// and a guard test. Delegates to the generic [humaDumpSpec], registering the operation via the same
// registerHumaAuditList (a single register path — no dump-vs-mount duplication): the handler is
// not called during dump. Returns a 3.1.0 spec (huma default).
func HumaAuditSpecYAML() (string, error) {
	return humaDumpSpec(func(api huma.API) error {
		registerHumaAuditList(api, handlers.AuditSpecStub())
		return nil
	})
}
