package api

// Registration and spec-dump of the PROVISIONING-POLICY domain (runtime policy for the
// methods of CREATING operators, ADR-058 Part B) on huma full-typed. GET — read (no audit);
// PUT — WRITE+AUDIT (variant B, huma-audit-middleware, event
// provisioning.policy_changed). The domain *Typed functions (handlers/provisioning.go)
// carry the business logic; the register func projects their result into a native wire-DTO.

import (
	"context"
	"log/slog"

	"github.com/danielgtaylor/huma/v2"
	"github.com/go-chi/chi/v5"

	"github.com/souls-guild/soul-stack/keeper/internal/api/handlers"
	apimiddleware "github.com/souls-guild/soul-stack/keeper/internal/api/middleware"
	"github.com/souls-guild/soul-stack/keeper/internal/api/problem"
	"github.com/souls-guild/soul-stack/shared/audit"
)

// registerHumaProvisioningPolicyGet mounts GET /v1/provisioning-policy via huma
// (READ, no audit). h nil → no-op. Handler: GetTyped → typed envelope output. RBAC
// provisioning.read — on the group.
func registerHumaProvisioningPolicyGet(humaAPI huma.API, h *handlers.ProvisioningPolicyHandler) {
	if h == nil {
		return
	}
	huma.Register(humaAPI, provisioningPolicyGetOperation(), func(_ context.Context, _ *provisioningPolicyGetInput) (*provisioningPolicyGetOutput, error) {
		return &provisioningPolicyGetOutput{Body: newProvisioningPolicyReply(h.GetTyped())}, nil
	})
}

// registerHumaProvisioningPolicyPut mounts PUT /v1/provisioning-policy via huma
// (WRITE+AUDIT variant B — event provisioning.policy_changed). h nil → no-op. Handler:
// claims → PutTyped (validation + SetSetting + invalidate) → audit-payload → 200 WITH BODY.
func registerHumaProvisioningPolicyPut(humaAPI huma.API, h *handlers.ProvisioningPolicyHandler) {
	if h == nil {
		return
	}
	huma.Register(humaAPI, provisioningPolicyPutOperation(), func(ctx context.Context, in *provisioningPolicyPutInput) (*provisioningPolicyPutOutput, error) {
		claims, ok := apimiddleware.ClaimsFromContext(ctx)
		if !ok {
			return nil, provisioningMissingClaims()
		}
		reply, err := h.PutTyped(ctx, claims, handlers.ProvisioningPolicyUpdateInput{
			AllowedMethods: in.Body.AllowedMethods,
		})
		if err != nil {
			return nil, provisioningProblem(err)
		}
		apimiddleware.SetHumaAuditPayload(ctx, apimiddleware.AuditPayload(reply.AuditPayload()))
		return &provisioningPolicyPutOutput{Status: 200, Body: newProvisioningPolicyReply(reply.Body)}, nil
	})
}

// provisioningMissingClaims — defensive response when claims are absent (unreachable:
// RequireJWT sets claims before huma). parity with serviceMissingClaims.
func provisioningMissingClaims() huma.StatusError {
	return humaProblemError{Details: problem.New(problem.TypeInternalError, "", "missing claims")}
}

// provisioningProblem delivers a *Typed-function error through huma as problem+json
// (parity with serviceProblem). Non-problem → 500.
func provisioningProblem(err error) huma.StatusError {
	if d, ok := handlers.AsProblemDetails(err); ok {
		return humaProblemError{Details: d}
	}
	return humaProblemError{Details: problem.New(problem.TypeInternalError, "", "internal error")}
}

// newHumaProvisioningAPI builds a huma.API over a chi group with huma-audit-middleware
// (variant B) under the given event type (parity with newHumaServiceAPI). The PUT route
// is mounted on its OWN chi group with its own event type.
func newHumaProvisioningAPI(r chi.Router, writer audit.Writer, evt audit.EventType, logger *slog.Logger) huma.API {
	return newHumaAuditAPI(r, writer, evt, logger)
}

// HumaProvisioningSpecYAML assembles the OpenAPI fragment of the provisioning-policy routes
// as a YAML string, WITHOUT mounting on a real router (hook for the spec-merge target +
// guard-test; parity with HumaServiceSpecYAML).
func HumaProvisioningSpecYAML() (string, error) {
	return humaDumpSpec(func(api huma.API) error {
		stub := handlers.ProvisioningPolicySpecStub()
		registerHumaProvisioningPolicyGet(api, stub)
		registerHumaProvisioningPolicyPut(api, stub)
		return nil
	})
}
