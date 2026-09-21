package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"regexp"

	"github.com/souls-guild/soul-stack/keeper/internal/jwt"
	"github.com/souls-guild/soul-stack/keeper/internal/sigil"
	"github.com/souls-guild/soul-stack/shared/audit"
)

// reSigilRef — closed charset for a `ref` label. 1:1 with the REST handler
// (api/handlers/sigil.go): kebab-case + dots (tags like v1.0.0) + underscore, NO
// slashes or `..`. Branch-refs with a slash aren't supported in MVP (variant C: ref is
// a stable allow-list label).
var reSigilRef = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)

// mcpMaxSourceLen bounds the `source` argument, mirroring the REST handler. A git
// remote is a URL, so it gets a length bound rather than a charset — the scheme
// allow-list that gates egress lives in the resolver, and a second opinion here could
// disagree with it.
const mcpMaxSourceLen = 2048

// validateSigilAllowArgs checks an allow request field by field, returning
// (human-readable msg, false) at the first invalid one. Symmetric with the REST
// validateAllowInput — including the alias check, which goes through
// [sigil.ValidateAlias] so the reserved list is consulted in ONE place for both
// transports.
func validateSigilAllowArgs(a pluginAllowArgs) (string, bool) {
	if err := sigil.ValidateAlias(a.Alias); err != nil {
		return err.Error(), false
	}
	switch {
	case a.Source == "":
		return "field 'source' is required", false
	case len(a.Source) > mcpMaxSourceLen:
		return fmt.Sprintf("field 'source' must be at most %d characters", mcpMaxSourceLen), false
	case a.Ref == "":
		return "field 'ref' is required", false
	case !reSigilRef.MatchString(a.Ref):
		return "field 'ref' must match " + reSigilRef.String() + " (branch-refs with '/' are not supported in MVP)", false
	}
	return "", true
}

// pluginRevokeArgs — arguments for the keeper.plugin.revoke tool
// (schemaPluginRevokeInput): the registration alias, which names exactly one active
// grant.
type pluginRevokeArgs struct {
	Alias string `json:"alias"`
}

// callPluginRevoke — mutating tool keeper.plugin.revoke. Transport over
// [sigil.Service.Revoke]: revoking the active grant under an alias lives in Service;
// the tool validates the alias, checks the permission, maps sentinels, and writes audit
// plugin.revoked.
//
// RBAC — plugin.revoke has no selector (rbac.md: NoSelector).
func (h *Handler) callPluginRevoke(ctx context.Context, claims *jwt.Claims, req jsonRPCRequest, args json.RawMessage) jsonRPCResponse {
	const toolName = "keeper.plugin.revoke"

	if h.deps.SigilSvc == nil {
		return h.toolError(req.ID, toolName, mcpCodeInternalError, sigilNotConfigured)
	}

	// RBAC BEFORE unmarshal/validation (least-disclosure): an unauthorized
	// operator gets no validation feedback about the body. Context nil — the
	// permission doesn't depend on the request body.
	if err := h.deps.RBAC.Check(claims.Subject, "plugin", "revoke", nil); err != nil {
		return h.toolError(req.ID, toolName, mcpCodeForbidden,
			"operator lacks required permission plugin.revoke")
	}

	var a pluginRevokeArgs
	if len(args) > 0 {
		if err := strictUnmarshal(args, &a); err != nil {
			return h.toolError(req.ID, toolName, mcpCodeMalformedRequest,
				"invalid arguments: "+err.Error())
		}
	}
	if err := sigil.ValidateAlias(a.Alias); err != nil {
		return h.toolError(req.ID, toolName, mcpCodeValidationFailed, err.Error())
	}

	err := h.deps.SigilSvc.Revoke(ctx, a.Alias, claims.Subject)
	if err != nil {
		code, detail := mapSigilErrorToMCP(err)
		if code == mcpCodeInternalError {
			h.deps.Logger.Error("mcp: plugin.revoke failed",
				slog.String("alias", a.Alias),
				slog.String("by_aid", claims.Subject),
				slog.Any("error", err),
			)
		}
		return h.toolError(req.ID, toolName, code, detail)
	}

	// Audit — parallel to the REST handler: payload {alias}.
	h.writeAudit(audit.EventPluginRevoked, claims.Subject, map[string]any{
		"alias": a.Alias,
	})

	// REST returns 204 No Content; the MCP equivalent is an empty output object.
	return h.toolResult(req.ID, struct{}{})
}
