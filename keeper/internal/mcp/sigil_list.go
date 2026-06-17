package mcp

import (
	"context"
	"encoding/json"
	"log/slog"
	"time"

	"github.com/souls-guild/soul-stack/keeper/internal/jwt"
)

// sigilView — output-проекция активного допуска для keeper.plugin.list
// (schemaPluginListOutput). 1:1 с REST sigilItem / [sigil.SigilView]: каталожные
// поля БЕЗ signature/manifest (крипто-материал / крупный JSONB — не лента
// allow-list-а).
type sigilView struct {
	Namespace    string     `json:"namespace"`
	Name         string     `json:"name"`
	Ref          string     `json:"ref"`
	SHA256       string     `json:"sha256"`
	AllowedByAID string     `json:"allowed_by_aid"`
	AllowedAt    time.Time  `json:"allowed_at"`
	RevokedAt    *time.Time `json:"revoked_at"`
}

// pluginListOutput — output keeper.plugin.list: лента активных допусков под
// ключом sigils (паритет REST GET /v1/plugins/sigils items).
type pluginListOutput struct {
	Sigils []sigilView `json:"sigils"`
}

// callPluginList — read-tool keeper.plugin.list. Транспорт поверх
// [sigil.Service.List] (read-only). reads НЕ аудируются.
//
// RBAC — plugin.list без селектора (rbac.md: NoSelector). arguments — пустой
// объект (schemaEmptyObject); strictUnmarshal отвергает лишние поля.
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
			Namespace:    v.Namespace,
			Name:         v.Name,
			Ref:          v.Ref,
			SHA256:       v.SHA256,
			AllowedByAID: v.AllowedByAID,
			AllowedAt:    v.AllowedAt,
			RevokedAt:    v.RevokedAt,
		})
	}
	return h.toolResult(req.ID, out)
}
