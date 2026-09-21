package api

// Registration and spec-dump of the SETTINGS domain (Keeper runtime settings
// overlay, ADR-0073) on huma full-typed. GET — read (no audit); PUT/DELETE —
// WRITE+AUDIT (variant B, huma-audit-middleware, events setting.updated /
// setting.deleted). The domain *Typed functions (handlers/settings.go) carry the
// business logic; the register func projects their result into a native
// wire-DTO.

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

// registerHumaSettingsList mounts GET /v1/settings (READ, no audit). h nil →
// no-op. RBAC setting.read — on the group.
func registerHumaSettingsList(humaAPI huma.API, h *handlers.SettingsHandler) {
	if h == nil {
		return
	}
	huma.Register(humaAPI, settingsListOperation(), func(_ context.Context, _ *settingsListInput) (*settingsListOutput, error) {
		return &settingsListOutput{Body: newSettingsCatalogReply(h.ListTyped())}, nil
	})
}

// registerHumaSettingPut mounts PUT /v1/settings/{key} (WRITE+AUDIT — event
// setting.updated). h nil → no-op.
func registerHumaSettingPut(humaAPI huma.API, h *handlers.SettingsHandler) {
	if h == nil {
		return
	}
	huma.Register(humaAPI, settingPutOperation(), func(ctx context.Context, in *settingPutInput) (*settingPutOutput, error) {
		claims, ok := apimiddleware.ClaimsFromContext(ctx)
		if !ok {
			return nil, settingsMissingClaims()
		}
		reply, err := h.PutTyped(ctx, claims, in.Key, in.Body.Value)
		if err != nil {
			return nil, settingsProblem(err)
		}
		apimiddleware.SetHumaAuditPayload(ctx, apimiddleware.AuditPayload(reply.AuditPayload()))
		return &settingPutOutput{Status: 200, Body: newSettingReply(reply.Body)}, nil
	})
}

// registerHumaSettingDelete mounts DELETE /v1/settings/{key} (WRITE+AUDIT —
// event setting.deleted). h nil → no-op.
func registerHumaSettingDelete(humaAPI huma.API, h *handlers.SettingsHandler) {
	if h == nil {
		return
	}
	huma.Register(humaAPI, settingDeleteOperation(), func(ctx context.Context, in *settingDeleteInput) (*settingDeleteOutput, error) {
		reply, err := h.DeleteTyped(ctx, in.Key)
		if err != nil {
			return nil, settingsProblem(err)
		}
		apimiddleware.SetHumaAuditPayload(ctx, apimiddleware.AuditPayload(reply.AuditPayload()))
		return &settingDeleteOutput{Status: 200, Body: newSettingReply(reply.Body)}, nil
	})
}

// settingsMissingClaims — defensive response when claims are absent
// (unreachable: RequireJWT sets claims before huma).
func settingsMissingClaims() huma.StatusError {
	return humaProblemError{Details: problem.New(problem.TypeInternalError, "", "missing claims")}
}

// settingsProblem delivers a *Typed-function error through huma as
// problem+json. Non-problem → 500.
func settingsProblem(err error) huma.StatusError {
	if d, ok := handlers.AsProblemDetails(err); ok {
		return humaProblemError{Details: d}
	}
	return humaProblemError{Details: problem.New(problem.TypeInternalError, "", "internal error")}
}

// newHumaSettingsAPI builds a huma.API over a chi group with
// huma-audit-middleware (variant B) under the given event type. Each mutating
// route gets its OWN group with its own event type.
func newHumaSettingsAPI(r chi.Router, writer audit.Writer, evt audit.EventType, logger *slog.Logger) huma.API {
	return newHumaAuditAPI(r, writer, evt, logger)
}

// HumaSettingsSpecYAML assembles the OpenAPI fragment of the settings routes as
// a YAML string, WITHOUT mounting on a real router (hook for the spec-merge
// target + guard-test).
func HumaSettingsSpecYAML() (string, error) {
	return humaDumpSpec(func(api huma.API) error {
		stub := handlers.SettingsSpecStub()
		registerHumaSettingsList(api, stub)
		registerHumaSettingPut(api, stub)
		registerHumaSettingDelete(api, stub)
		return nil
	})
}
