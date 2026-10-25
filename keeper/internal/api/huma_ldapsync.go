package api

// Registration and spec-dump of the LDAP-SYNC domain (ADR-058 amendment
// NIM-1053) on huma full-typed. GET — read (no audit); PUT — WRITE+AUDIT
// (variant B, event ldap_sync.settings_changed).

import (
	"context"

	"github.com/danielgtaylor/huma/v2"

	"github.com/souls-guild/soul-stack/keeper/internal/api/handlers"
	apimiddleware "github.com/souls-guild/soul-stack/keeper/internal/api/middleware"
)

// registerHumaLDAPSyncGet mounts GET /v1/ldap-sync. h nil → no-op. RBAC
// setting.read — on the group.
func registerHumaLDAPSyncGet(humaAPI huma.API, h *handlers.LDAPSyncHandler) {
	if h == nil {
		return
	}
	huma.Register(humaAPI, ldapSyncGetOperation(), func(ctx context.Context, _ *ldapSyncGetInput) (*ldapSyncGetOutput, error) {
		v, err := h.GetTyped(ctx)
		if err != nil {
			return nil, provisioningProblem(err)
		}
		return &ldapSyncGetOutput{Body: newLDAPSyncReply(v)}, nil
	})
}

// registerHumaLDAPSyncPut mounts PUT /v1/ldap-sync. h nil → no-op. RBAC
// setting.update — on the group; audit — the group's huma middleware.
func registerHumaLDAPSyncPut(humaAPI huma.API, h *handlers.LDAPSyncHandler) {
	if h == nil {
		return
	}
	huma.Register(humaAPI, ldapSyncPutOperation(), func(ctx context.Context, in *ldapSyncPutInput) (*ldapSyncPutOutput, error) {
		claims, ok := apimiddleware.ClaimsFromContext(ctx)
		if !ok {
			return nil, provisioningMissingClaims()
		}
		reply, err := h.PutTyped(ctx, claims, toLDAPSyncUpdateInput(in.Body))
		if err != nil {
			return nil, provisioningProblem(err)
		}
		apimiddleware.SetHumaAuditPayload(ctx, apimiddleware.AuditPayload(reply.AuditPayload()))
		return &ldapSyncPutOutput{Status: 200, Body: newLDAPSyncReply(reply.Body)}, nil
	})
}

// HumaLDAPSyncSpecYAML assembles the OpenAPI fragment of the ldap-sync routes
// WITHOUT mounting on a real router.
func HumaLDAPSyncSpecYAML() (string, error) {
	return humaDumpSpec(func(api huma.API) error {
		stub := handlers.LDAPSyncSpecStub()
		registerHumaLDAPSyncGet(api, stub)
		registerHumaLDAPSyncPut(api, stub)
		return nil
	})
}
