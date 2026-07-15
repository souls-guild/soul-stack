package mcp

import (
	"context"
	"encoding/json"
	"log/slog"

	"github.com/souls-guild/soul-stack/keeper/internal/jwt"
	"github.com/souls-guild/soul-stack/keeper/internal/sigil"
	"github.com/souls-guild/soul-stack/shared/audit"
)

// sigilNotConfigured — public detail for the plugin-tools nil-guard. SigilSvc
// is an optional HandlerDeps field (production wire-up passes *sigil.Service
// ONLY when sigil.signing_key_ref is set): with nil, plugin-tools still
// dispatch but return internal-error "not configured" (mirrors the
// role-tools RBACRoles guard).
const sigilNotConfigured = "sigil is not configured"

// pluginAllowArgs — arguments for keeper.plugin.allow (schemaPluginAllowInput):
// namespace + name + ref are required. The triple is validated by
// reSigilSegment (closed charset, like REST DELETE path segments).
type pluginAllowArgs struct {
	Namespace string `json:"namespace"`
	Name      string `json:"name"`
	Ref       string `json:"ref"`
}

// pluginAllowOutput — output of keeper.plugin.allow: echoes the triple +
// sha256 of the allowed binary (parity with REST POST /v1/plugins/sigils
// 201 response).
type pluginAllowOutput struct {
	Namespace string `json:"namespace"`
	Name      string `json:"name"`
	Ref       string `json:"ref"`
	SHA256    string `json:"sha256"`
}

// callPluginAllow — mutating tool keeper.plugin.allow. A transport over
// [sigil.Service.Allow]: reading the cache slot, signing, inserting the
// record all live in Service; the tool decodes input, validates the triple,
// checks the permission, maps sentinels to MCP codes, and writes the
// plugin.allowed audit event.
//
// RBAC — plugin.allow with no selector (rbac.md: NoSelector, like
// operator.*/role.*).
func (h *Handler) callPluginAllow(ctx context.Context, claims *jwt.Claims, req jsonRPCRequest, args json.RawMessage) jsonRPCResponse {
	const toolName = "keeper.plugin.allow"

	if h.deps.SigilSvc == nil {
		return h.toolError(req.ID, toolName, mcpCodeInternalError, sigilNotConfigured)
	}

	// RBAC BEFORE unmarshal/validation (least-disclosure): an unauthorized
	// operator gets no validation feedback about the body. Context nil — the
	// permission doesn't depend on the request body.
	if err := h.deps.RBAC.Check(claims.Subject, "plugin", "allow", nil); err != nil {
		return h.toolError(req.ID, toolName, mcpCodeForbidden,
			"operator lacks required permission plugin.allow")
	}

	var a pluginAllowArgs
	if len(args) > 0 {
		if err := strictUnmarshal(args, &a); err != nil {
			return h.toolError(req.ID, toolName, mcpCodeMalformedRequest,
				"invalid arguments: "+err.Error())
		}
	}
	if msg, valid := validateSigilTriple(a.Namespace, a.Name, a.Ref); !valid {
		return h.toolError(req.ID, toolName, mcpCodeValidationFailed, msg)
	}

	sha256, err := h.deps.SigilSvc.Allow(ctx, sigil.AllowInput{
		Namespace: a.Namespace,
		Name:      a.Name,
		Ref:       a.Ref,
		CallerAID: claims.Subject,
	})
	if err != nil {
		code, detail := mapSigilErrorToMCP(err)
		if code == mcpCodeInternalError {
			h.deps.Logger.Error("mcp: plugin.allow failed",
				slog.String("namespace", a.Namespace),
				slog.String("name", a.Name),
				slog.String("ref", a.Ref),
				slog.String("by_aid", claims.Subject),
				slog.Any("error", err),
			)
		}
		return h.toolError(req.ID, toolName, code, detail)
	}

	// Audit — mirrors the REST handler (supply-chain control, ADR-022):
	// payload {namespace, name, ref, sha256, allowed_by_aid}. signature/manifest
	// (crypto material / large JSONB) are NOT written — same as REST.
	h.writeAudit(audit.EventPluginAllowed, claims.Subject, map[string]any{
		"namespace":      a.Namespace,
		"name":           a.Name,
		"ref":            a.Ref,
		"sha256":         sha256,
		"allowed_by_aid": claims.Subject,
	})

	return h.toolResult(req.ID, pluginAllowOutput{
		Namespace: a.Namespace,
		Name:      a.Name,
		Ref:       a.Ref,
		SHA256:    sha256,
	})
}
