package mcp

// SettingsStore tools (ADR-0073): the same catalog / override / revert surface
// the Operator API serves, over MCP. The business logic is the SAME
// *handlers.SettingsHandler instance REST uses — single source of truth, so the
// write-gate (field-registry parse + range bounds + the cross-field dry-run
// merge) cannot differ between the two transports.

import (
	"context"
	"encoding/json"
	"log/slog"

	"github.com/souls-guild/soul-stack/keeper/internal/api/handlers"
	"github.com/souls-guild/soul-stack/keeper/internal/jwt"
	"github.com/souls-guild/soul-stack/shared/audit"
)

const settingsNotConfigured = "settings store is not configured"

// settingView is the output projection of one catalog entry
// (schemaSettingListOutput). Type/Bounds/Default describe the field so an agent
// can construct a valid value without guessing; Value/Source describe the
// here-and-now on the answering instance.
type settingView struct {
	Key         string `json:"key"`
	YAMLPath    string `json:"yaml_path"`
	Type        string `json:"type"`
	Bounds      string `json:"bounds"`
	Default     any    `json:"default"`
	Value       any    `json:"value"`
	Source      string `json:"source"`
	Description string `json:"description"`

	// Set only when this instance's keeper.yml shadows a cluster override
	// (ADR-0073(b), amended — the file wins): an agent must not read a written
	// value back as "in effect here".
	ClusterValue      any  `json:"cluster_value,omitempty"`
	OverriddenLocally bool `json:"overridden_locally,omitempty"`
}

type settingListOutput struct {
	Settings []settingView `json:"settings"`
}

type settingUpdateArgs struct {
	Key   string `json:"key"`
	Value string `json:"value"`
}

type settingDeleteArgs struct {
	Key string `json:"key"`
}

func toSettingView(v handlers.SettingView) settingView {
	return settingView{
		Key:         v.Key,
		YAMLPath:    v.YAMLPath,
		Type:        v.Type,
		Bounds:      v.Bounds,
		Default:     v.Default,
		Value:       v.Value,
		Source:      v.Source,
		Description: v.Description,

		ClusterValue:      v.ClusterValue,
		OverriddenLocally: v.OverriddenLocally,
	}
}

// callSettingList is the read-tool keeper.setting.list. Reads are NOT audited
// (the provisioning.read / audit.read precedent).
//
// RBAC: setting.read, cluster-level (nil context — settings have no selector).
func (h *Handler) callSettingList(_ context.Context, claims *jwt.Claims, req jsonRPCRequest, args json.RawMessage) jsonRPCResponse {
	const toolName = "keeper.setting.list"

	if h.deps.Settings == nil {
		return h.toolError(req.ID, toolName, mcpCodeInternalError, settingsNotConfigured)
	}
	if len(args) > 0 {
		var empty struct{}
		if err := strictUnmarshal(args, &empty); err != nil {
			return h.toolError(req.ID, toolName, mcpCodeMalformedRequest,
				"invalid arguments: "+err.Error())
		}
	}
	if err := h.deps.RBAC.Check(claims.Subject, "setting", "read", nil); err != nil {
		return h.toolError(req.ID, toolName, mcpCodeForbidden,
			"operator lacks required permission setting.read")
	}

	views := h.deps.Settings.ListTyped()
	out := settingListOutput{Settings: make([]settingView, 0, len(views))}
	for _, v := range views {
		out.Settings = append(out.Settings, toSettingView(v))
	}
	return h.toolResult(req.ID, out)
}

// callSettingUpdate is keeper.setting.update: set a cluster-wide override. The
// value travels in its text form and goes through the same gate as PUT — an
// out-of-range value, or one that would break a cross-field invariant, is
// rejected and `keeper_settings` is untouched.
func (h *Handler) callSettingUpdate(ctx context.Context, claims *jwt.Claims, req jsonRPCRequest, args json.RawMessage) jsonRPCResponse {
	const toolName = "keeper.setting.update"

	if h.deps.Settings == nil {
		return h.toolError(req.ID, toolName, mcpCodeInternalError, settingsNotConfigured)
	}
	// RBAC BEFORE unmarshal (least-disclosure): an unauthorized operator gets
	// no validation feedback about the body.
	if err := h.deps.RBAC.Check(claims.Subject, "setting", "update", nil); err != nil {
		return h.toolError(req.ID, toolName, mcpCodeForbidden,
			"operator lacks required permission setting.update")
	}

	var a settingUpdateArgs
	if len(args) > 0 {
		if err := strictUnmarshal(args, &a); err != nil {
			return h.toolError(req.ID, toolName, mcpCodeMalformedRequest,
				"invalid arguments: "+err.Error())
		}
	}
	if a.Key == "" {
		return h.toolError(req.ID, toolName, mcpCodeValidationFailed, "field 'key' is required")
	}
	if a.Value == "" {
		return h.toolError(req.ID, toolName, mcpCodeValidationFailed, "field 'value' is required")
	}

	reply, err := h.deps.Settings.PutTyped(ctx, claims, a.Key, a.Value)
	if err != nil {
		code, detail := mapSettingErrorToMCP(err)
		if code == mcpCodeInternalError {
			h.deps.Logger.Error("mcp: setting.update failed",
				slog.String("key", a.Key),
				slog.String("by_aid", claims.Subject),
				slog.Any("error", err),
			)
		}
		return h.toolError(req.ID, toolName, code, detail)
	}

	h.writeAudit(audit.EventSettingUpdated, claims.Subject, map[string]any(reply.AuditPayload()))
	return h.toolResult(req.ID, toSettingView(reply.Body))
}

// callSettingDelete is keeper.setting.delete: drop the override, so the file
// value (or the built-in default) is back in effect cluster-wide.
func (h *Handler) callSettingDelete(ctx context.Context, claims *jwt.Claims, req jsonRPCRequest, args json.RawMessage) jsonRPCResponse {
	const toolName = "keeper.setting.delete"

	if h.deps.Settings == nil {
		return h.toolError(req.ID, toolName, mcpCodeInternalError, settingsNotConfigured)
	}
	if err := h.deps.RBAC.Check(claims.Subject, "setting", "delete", nil); err != nil {
		return h.toolError(req.ID, toolName, mcpCodeForbidden,
			"operator lacks required permission setting.delete")
	}

	var a settingDeleteArgs
	if len(args) > 0 {
		if err := strictUnmarshal(args, &a); err != nil {
			return h.toolError(req.ID, toolName, mcpCodeMalformedRequest,
				"invalid arguments: "+err.Error())
		}
	}
	if a.Key == "" {
		return h.toolError(req.ID, toolName, mcpCodeValidationFailed, "field 'key' is required")
	}

	reply, err := h.deps.Settings.DeleteTyped(ctx, a.Key)
	if err != nil {
		code, detail := mapSettingErrorToMCP(err)
		if code == mcpCodeInternalError {
			h.deps.Logger.Error("mcp: setting.delete failed",
				slog.String("key", a.Key),
				slog.String("by_aid", claims.Subject),
				slog.Any("error", err),
			)
		}
		return h.toolError(req.ID, toolName, code, detail)
	}

	h.writeAudit(audit.EventSettingDeleted, claims.Subject, map[string]any(reply.AuditPayload()))
	return h.toolResult(req.ID, toSettingView(reply.Body))
}

// mapSettingErrorToMCP translates the handler's problem+json details into MCP
// codes, so both transports refuse the same things for the same reasons.
func mapSettingErrorToMCP(err error) (string, string) {
	d, ok := handlers.AsProblemDetails(err)
	if !ok {
		return mcpCodeInternalError, "settings operation failed"
	}
	switch d.Status {
	case 404:
		return mcpCodeNotFound, d.Detail
	case 422:
		return mcpCodeValidationFailed, d.Detail
	default:
		return mcpCodeInternalError, d.Detail
	}
}
