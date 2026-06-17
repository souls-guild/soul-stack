package mcp

import (
	"context"
	"encoding/json"
	"log/slog"
	"regexp"
	"time"

	"github.com/souls-guild/soul-stack/keeper/internal/jwt"
	"github.com/souls-guild/soul-stack/shared/audit"
)

// MCP-tools ротации trust-anchor-ключей подписи Sigil (ADR-026(h), R3-S7) —
// паритет REST /v1/sigil/keys*. Транспорт поверх [sigil.KeyService]; tool
// валидирует input, проверяет permission, маппит sentinel-ы и пишет audit.
//
// 3-сегментный tool-name keeper.sigil.key.<verb> ↔ 2-сегментная permission
// sigil.key-<verb> (RBAC-грамматика — ровно <resource>.<action>).
//
// БЕЗОПАСНОСТЬ: приватник НИКОГДА не в output (KeyService его не возвращает) и
// не в логах (логируем только key_id / by_aid).

// sigilKeyNotConfigured — public-detail nil-guard-а sigil.key-tools (SigilKeySvc
// nil при выключенном Sigil; симметрично sigilNotConfigured plugin-tools).
const sigilKeyNotConfigured = "sigil is not configured"

// reSigilKeyIDMCP — формат key_id (64 нижних hex-символа), 1:1 с REST reSigilKeyID.
var reSigilKeyIDMCP = regexp.MustCompile(`^[0-9a-f]{64}$`)

// sigilKeyIntroduceArgs — arguments keeper.sigil.key.introduce.
type sigilKeyIntroduceArgs struct {
	MakePrimary bool `json:"make_primary"`
}

// sigilKeyIntroduceOutput — output keeper.sigil.key.introduce. БЕЗ приватника
// (паритет 201 REST POST /v1/sigil/keys).
type sigilKeyIntroduceOutput struct {
	KeyID        string    `json:"key_id"`
	PubkeyPEM    string    `json:"pubkey_pem"`
	IsPrimary    bool      `json:"is_primary"`
	Status       string    `json:"status"`
	IntroducedAt time.Time `json:"introduced_at"`
}

// callSigilKeyIntroduce — mutating-tool keeper.sigil.key.introduce.
func (h *Handler) callSigilKeyIntroduce(ctx context.Context, claims *jwt.Claims, req jsonRPCRequest, args json.RawMessage) jsonRPCResponse {
	const toolName = "keeper.sigil.key.introduce"

	if h.deps.SigilKeySvc == nil {
		return h.toolError(req.ID, toolName, mcpCodeInternalError, sigilKeyNotConfigured)
	}

	var a sigilKeyIntroduceArgs
	if len(args) > 0 {
		if err := strictUnmarshal(args, &a); err != nil {
			return h.toolError(req.ID, toolName, mcpCodeMalformedRequest,
				"invalid arguments: "+err.Error())
		}
	}

	if err := h.deps.RBAC.Check(claims.Subject, "sigil", "key-introduce", nil); err != nil {
		return h.toolError(req.ID, toolName, mcpCodeForbidden,
			"operator lacks required permission sigil.key-introduce")
	}

	res, err := h.deps.SigilKeySvc.Introduce(ctx, a.MakePrimary, claims.Subject)
	if err != nil {
		code, detail := mapSigilKeyErrorToMCP(err)
		if code == mcpCodeInternalError {
			h.deps.Logger.Error("mcp: sigil.key.introduce failed",
				slog.String("by_aid", claims.Subject), slog.Any("error", err))
		}
		return h.toolError(req.ID, toolName, code, detail)
	}

	// Audit — паритет REST: payload {key_id, is_primary, introduced_by_aid}.
	// Приватник (в Vault) НЕ пишется.
	h.writeAudit(audit.EventSigilKeyIntroduced, claims.Subject, map[string]any{
		"key_id":            res.KeyID,
		"is_primary":        res.IsPrimary,
		"introduced_by_aid": claims.Subject,
	})

	return h.toolResult(req.ID, sigilKeyIntroduceOutput{
		KeyID:        res.KeyID,
		PubkeyPEM:    res.PubkeyPEM,
		IsPrimary:    res.IsPrimary,
		Status:       res.Status,
		IntroducedAt: res.IntroducedAt,
	})
}

// sigilKeyListItem — одна запись output-а keeper.sigil.key.list.
type sigilKeyListItem struct {
	KeyID        string    `json:"key_id"`
	IsPrimary    bool      `json:"is_primary"`
	Status       string    `json:"status"`
	IntroducedAt time.Time `json:"introduced_at"`
}

// sigilKeyListOutput — output keeper.sigil.key.list.
type sigilKeyListOutput struct {
	Keys []sigilKeyListItem `json:"keys"`
}

// callSigilKeyList — read-only tool keeper.sigil.key.list. Без audit.
func (h *Handler) callSigilKeyList(ctx context.Context, claims *jwt.Claims, req jsonRPCRequest, _ json.RawMessage) jsonRPCResponse {
	const toolName = "keeper.sigil.key.list"

	if h.deps.SigilKeySvc == nil {
		return h.toolError(req.ID, toolName, mcpCodeInternalError, sigilKeyNotConfigured)
	}
	if err := h.deps.RBAC.Check(claims.Subject, "sigil", "key-list", nil); err != nil {
		return h.toolError(req.ID, toolName, mcpCodeForbidden,
			"operator lacks required permission sigil.key-list")
	}

	keys, err := h.deps.SigilKeySvc.List(ctx)
	if err != nil {
		h.deps.Logger.Error("mcp: sigil.key.list failed", slog.Any("error", err))
		return h.toolError(req.ID, toolName, mcpCodeInternalError, "internal error")
	}

	items := make([]sigilKeyListItem, 0, len(keys))
	for _, k := range keys {
		items = append(items, sigilKeyListItem{
			KeyID:        k.KeyID,
			IsPrimary:    k.IsPrimary,
			Status:       k.Status,
			IntroducedAt: k.IntroducedAt,
		})
	}
	return h.toolResult(req.ID, sigilKeyListOutput{Keys: items})
}

// sigilKeyIDArgs — arguments tool-ов set-primary / retire (общий: только key_id).
type sigilKeyIDArgs struct {
	KeyID string `json:"key_id"`
}

// callSigilKeySetPrimary — mutating-tool keeper.sigil.key.set-primary.
func (h *Handler) callSigilKeySetPrimary(ctx context.Context, claims *jwt.Claims, req jsonRPCRequest, args json.RawMessage) jsonRPCResponse {
	const toolName = "keeper.sigil.key.set-primary"

	if h.deps.SigilKeySvc == nil {
		return h.toolError(req.ID, toolName, mcpCodeInternalError, sigilKeyNotConfigured)
	}

	// RBAC ДО unmarshal/regex-валидации (паритет introduce/list и REST-middleware):
	// факт валидности key_id не утекает оператору без права key-set-primary.
	if err := h.deps.RBAC.Check(claims.Subject, "sigil", "key-set-primary", nil); err != nil {
		return h.toolError(req.ID, toolName, mcpCodeForbidden,
			"operator lacks required permission sigil.key-set-primary")
	}

	var a sigilKeyIDArgs
	if len(args) > 0 {
		if err := strictUnmarshal(args, &a); err != nil {
			return h.toolError(req.ID, toolName, mcpCodeMalformedRequest,
				"invalid arguments: "+err.Error())
		}
	}
	if !reSigilKeyIDMCP.MatchString(a.KeyID) {
		return h.toolError(req.ID, toolName, mcpCodeValidationFailed,
			"field 'key_id' must match "+reSigilKeyIDMCP.String())
	}

	if err := h.deps.SigilKeySvc.SetPrimary(ctx, a.KeyID, claims.Subject); err != nil {
		code, detail := mapSigilKeyErrorToMCP(err)
		if code == mcpCodeInternalError {
			h.deps.Logger.Error("mcp: sigil.key.set-primary failed",
				slog.String("key_id", a.KeyID), slog.String("by_aid", claims.Subject), slog.Any("error", err))
		}
		return h.toolError(req.ID, toolName, code, detail)
	}

	h.writeAudit(audit.EventSigilKeyPrimarySet, claims.Subject, map[string]any{
		"key_id":     a.KeyID,
		"set_by_aid": claims.Subject,
	})
	return h.toolResult(req.ID, struct{}{})
}

// callSigilKeyRetire — mutating-tool keeper.sigil.key.retire.
func (h *Handler) callSigilKeyRetire(ctx context.Context, claims *jwt.Claims, req jsonRPCRequest, args json.RawMessage) jsonRPCResponse {
	const toolName = "keeper.sigil.key.retire"

	if h.deps.SigilKeySvc == nil {
		return h.toolError(req.ID, toolName, mcpCodeInternalError, sigilKeyNotConfigured)
	}

	// RBAC ДО unmarshal/regex-валидации (паритет introduce/list и REST-middleware):
	// факт валидности key_id не утекает оператору без права key-retire.
	if err := h.deps.RBAC.Check(claims.Subject, "sigil", "key-retire", nil); err != nil {
		return h.toolError(req.ID, toolName, mcpCodeForbidden,
			"operator lacks required permission sigil.key-retire")
	}

	var a sigilKeyIDArgs
	if len(args) > 0 {
		if err := strictUnmarshal(args, &a); err != nil {
			return h.toolError(req.ID, toolName, mcpCodeMalformedRequest,
				"invalid arguments: "+err.Error())
		}
	}
	if !reSigilKeyIDMCP.MatchString(a.KeyID) {
		return h.toolError(req.ID, toolName, mcpCodeValidationFailed,
			"field 'key_id' must match "+reSigilKeyIDMCP.String())
	}

	if err := h.deps.SigilKeySvc.Retire(ctx, a.KeyID, claims.Subject); err != nil {
		code, detail := mapSigilKeyErrorToMCP(err)
		if code == mcpCodeInternalError {
			h.deps.Logger.Error("mcp: sigil.key.retire failed",
				slog.String("key_id", a.KeyID), slog.String("by_aid", claims.Subject), slog.Any("error", err))
		}
		return h.toolError(req.ID, toolName, code, detail)
	}

	h.writeAudit(audit.EventSigilKeyRetired, claims.Subject, map[string]any{
		"key_id":         a.KeyID,
		"retired_by_aid": claims.Subject,
	})
	return h.toolResult(req.ID, struct{}{})
}
