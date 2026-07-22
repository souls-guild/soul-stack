package mcp

import (
	"context"
	"encoding/json"
	"log/slog"
	"regexp"

	"github.com/souls-guild/soul-stack/keeper/internal/jwt"
	"github.com/souls-guild/soul-stack/shared/audit"
)

// reSigilSegment — closed charset for the Sigil triple (namespace / name /
// ref). 1:1 with the REST handler (api/handlers/sigil.go): kebab-case + dots
// (tags like v1.0.0) + underscore, NO slashes or `..`. Branch-refs with a
// slash aren't supported in MVP (variant C: ref is a stable allow-list label).
var reSigilSegment = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)

// validateSigilTriple checks the (namespace, name, ref) triple against
// [reSigilSegment]. Returns (human-readable msg, false) for the first invalid
// part, ("", true) if all are valid. Symmetric with REST validateSigilTriple.
func validateSigilTriple(namespace, name, ref string) (string, bool) {
	switch {
	case namespace == "":
		return "field 'namespace' is required", false
	case !reSigilSegment.MatchString(namespace):
		return "field 'namespace' must match " + reSigilSegment.String(), false
	case name == "":
		return "field 'name' is required", false
	case !reSigilSegment.MatchString(name):
		return "field 'name' must match " + reSigilSegment.String(), false
	case ref == "":
		return "field 'ref' is required", false
	case !reSigilSegment.MatchString(ref):
		return "field 'ref' must match " + reSigilSegment.String() + " (branch-refs with '/' are not supported in MVP)", false
	}
	return "", true
}

// pluginRevokeArgs — arguments for the keeper.plugin.revoke tool
// (schemaPluginRevokeInput): namespace + name + ref are required.
type pluginRevokeArgs struct {
	Namespace string `json:"namespace"`
	Name      string `json:"name"`
	Ref       string `json:"ref"`
}

// callPluginRevoke — mutating tool keeper.plugin.revoke. Transport over
// [sigil.Service.Revoke]: revoking the active allow-list entry (namespace,
// name, ref) lives in Service; the tool validates the triple, checks the
// permission, maps sentinels, and writes audit plugin.revoked.
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
	if msg, valid := validateSigilTriple(a.Namespace, a.Name, a.Ref); !valid {
		return h.toolError(req.ID, toolName, mcpCodeValidationFailed, msg)
	}

	err := h.deps.SigilSvc.Revoke(ctx, a.Namespace, a.Name, a.Ref, claims.Subject)
	if err != nil {
		code, detail := mapSigilErrorToMCP(err)
		if code == mcpCodeInternalError {
			h.deps.Logger.Error("mcp: plugin.revoke failed",
				slog.String("namespace", a.Namespace),
				slog.String("name", a.Name),
				slog.String("ref", a.Ref),
				slog.String("by_aid", claims.Subject),
				slog.Any("error", err),
			)
		}
		return h.toolError(req.ID, toolName, code, detail)
	}

	// Audit — parallel to the REST handler: payload {namespace, name, ref}.
	h.writeAudit(audit.EventPluginRevoked, claims.Subject, map[string]any{
		"namespace": a.Namespace,
		"name":      a.Name,
		"ref":       a.Ref,
	})

	// REST returns 204 No Content; the MCP equivalent is an empty output object.
	return h.toolResult(req.ID, struct{}{})
}
