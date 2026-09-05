package mcp

import (
	"context"
	"encoding/json"
	"log/slog"
	"time"

	"github.com/souls-guild/soul-stack/keeper/internal/jwt"
)

// sigilView — output projection of an active allow-list entry for
// keeper.plugin.list (schemaPluginListOutput). 1:1 with the REST PluginSigilView /
// [sigil.SigilView]: catalog fields WITHOUT the signature or the schema (crypto
// material / a large document; neither is part of the allow-list feed).
type sigilView struct {
	Alias  string `json:"alias"`
	Source string `json:"source"`
	Ref    string `json:"ref"`
	Kind   string `json:"kind"`
	// Artifacts is the whole approved release, not this Keeper's platform: an
	// operator auditing the allow-list has to be able to see every digest that
	// approval covers.
	Artifacts    []pluginArtifactOutput `json:"artifacts"`
	AllowedByAID string                 `json:"allowed_by_aid"`
	AllowedAt    time.Time              `json:"allowed_at"`
	RevokedAt    *time.Time             `json:"revoked_at"`
}

// pluginListOutput — output of keeper.plugin.list: the feed of active
// allow-list entries under the sigils key (parity with REST GET
// /v1/plugins/sigils items).
type pluginListOutput struct {
	Sigils []sigilView `json:"sigils"`
}

// callPluginList — read tool keeper.plugin.list. Transport over
// [sigil.Service.List] (read-only). Reads are NOT audited.
//
// RBAC — plugin.list has no selector (rbac.md: NoSelector). arguments — an
// empty object (schemaEmptyObject); strictUnmarshal rejects extra fields.
func (h *Handler) callPluginList(ctx context.Context, claims *jwt.Claims, req jsonRPCRequest, args json.RawMessage) jsonRPCResponse {
	const toolName = "keeper.plugin.list"

	if h.deps.SigilSvc == nil {
		return h.toolError(req.ID, toolName, mcpCodeInternalError, sigilNotConfigured)
	}

	if len(args) > 0 {
		var empty struct{}
		if err := strictUnmarshal(args, &empty); err != nil {
			return h.toolError(req.ID, toolName, mcpCodeMalformedRequest,
				"invalid arguments: "+err.Error())
		}
	}

	if err := h.deps.RBAC.Check(claims.Subject, "plugin", "list", nil); err != nil {
		return h.toolError(req.ID, toolName, mcpCodeForbidden,
			"operator lacks required permission plugin.list")
	}

	views, err := h.deps.SigilSvc.List(ctx)
	if err != nil {
		h.deps.Logger.Error("mcp: plugin.list failed",
			slog.String("by_aid", claims.Subject),
			slog.Any("error", err),
		)
		return h.toolError(req.ID, toolName, mcpCodeInternalError, "internal error")
	}

	out := pluginListOutput{Sigils: make([]sigilView, 0, len(views))}
	for _, v := range views {
		out.Sigils = append(out.Sigils, sigilView{
			Alias:        v.Alias,
			Source:       v.Source,
			Ref:          v.Ref,
			Kind:         v.Kind,
			Artifacts:    artifactOutputsOf(v.Artifacts),
			AllowedByAID: v.AllowedByAID,
			AllowedAt:    v.AllowedAt,
			RevokedAt:    v.RevokedAt,
		})
	}
	return h.toolResult(req.ID, out)
}
