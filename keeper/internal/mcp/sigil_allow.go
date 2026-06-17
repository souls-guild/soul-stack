package mcp

import (
	"context"
	"encoding/json"
	"log/slog"

	"github.com/souls-guild/soul-stack/keeper/internal/jwt"
	"github.com/souls-guild/soul-stack/keeper/internal/sigil"
	"github.com/souls-guild/soul-stack/shared/audit"
)

// sigilNotConfigured — public-detail nil-guard-а plugin-tools. SigilSvc — опц.
// поле HandlerDeps (production-wire-up передаёт *sigil.Service ТОЛЬКО при
// заданном sigil.signing_key_ref): при nil plugin-tools диспатчатся, но
// возвращают internal-error «не сконфигурировано» (симметрично RBACRoles-guard-у
// role-tools).
const sigilNotConfigured = "sigil is not configured"

// pluginAllowArgs — arguments tool-а keeper.plugin.allow (schemaPluginAllowInput):
// namespace + name + ref обязательны. Тройка валидируется reSigilSegment
// (closed-charset, как path-сегменты REST DELETE).
type pluginAllowArgs struct {
	Namespace string `json:"namespace"`
	Name      string `json:"name"`
	Ref       string `json:"ref"`
}

// pluginAllowOutput — output keeper.plugin.allow: эхо тройки + sha256
// допущенного бинаря (паритет 201-ответа REST POST /v1/plugins/sigils).
type pluginAllowOutput struct {
	Namespace string `json:"namespace"`
	Name      string `json:"name"`
	Ref       string `json:"ref"`
	SHA256    string `json:"sha256"`
}

// callPluginAllow — mutating-tool keeper.plugin.allow. Транспорт поверх
// [sigil.Service.Allow]: чтение слота кеша, подпись, вставка записи — в Service;
// tool декодирует input, валидирует тройку, проверяет permission, маппит
// sentinel-ы в MCP-коды и пишет audit plugin.allowed.
//
// RBAC — plugin.allow без селектора (rbac.md: NoSelector, как operator.*/role.*).
func (h *Handler) callPluginAllow(ctx context.Context, claims *jwt.Claims, req jsonRPCRequest, args json.RawMessage) jsonRPCResponse {
	const toolName = "keeper.plugin.allow"

	if h.deps.SigilSvc == nil {
		return h.toolError(req.ID, toolName, mcpCodeInternalError, sigilNotConfigured)
	}

	// RBAC ДО unmarshal/валидации (least-disclosure): неавторизованный оператор
	// не получает validation-feedback по телу. Контекст nil — право не зависит
	// от тела запроса.
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

	// Audit — параллельно REST-handler-у (supply-chain-контроль, ADR-022):
	// payload {namespace, name, ref, sha256, allowed_by_aid}. signature/manifest
	// (крипто-материал / крупный JSONB) НЕ пишем — как REST.
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
