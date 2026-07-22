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

// MCP tools for Sigil signing trust-anchor key rotation (ADR-026(h), R3-S7)
// — parity with REST /v1/sigil/keys*. Transport over [sigil.KeyService];
// each tool validates input, checks permission, maps sentinels, and writes
// audit.
//
// 3-segment tool name keeper.sigil.key.<verb> ↔ 2-segment permission
// sigil.key-<verb> (RBAC grammar is exactly <resource>.<action>).
//
// SECURITY: the private key is NEVER in output (KeyService never returns
// it) and never in logs (only key_id / by_aid are logged).

// sigilKeyNotConfigured is the public detail for the sigil.key-tools
// nil-guard (SigilKeySvc is nil when Sigil is disabled; mirrors
// sigilNotConfigured in the plugin tools).
const sigilKeyNotConfigured = "sigil is not configured"

// reSigilKeyIDMCP — key_id format (64 lowercase hex chars), 1:1 with REST reSigilKeyID.
var reSigilKeyIDMCP = regexp.MustCompile(`^[0-9a-f]{64}$`)

// sigilKeyIntroduceArgs — arguments keeper.sigil.key.introduce.
type sigilKeyIntroduceArgs struct {
	MakePrimary bool `json:"make_primary"`
}

// sigilKeyIntroduceOutput — output of keeper.sigil.key.introduce. No
// private key included (parity with REST 201 POST /v1/sigil/keys).
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

	// Audit — parity with REST: payload {key_id, is_primary, introduced_by_aid}.
	// The private key (in Vault) is NOT written.
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

// sigilKeyListItem — one entry in the keeper.sigil.key.list output.
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

// callSigilKeyList — read-only tool keeper.sigil.key.list. No audit.
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

// sigilKeyIDArgs — arguments shared by the set-primary / retire tools (just key_id).
type sigilKeyIDArgs struct {
	KeyID string `json:"key_id"`
}

// callSigilKeySetPrimary — mutating-tool keeper.sigil.key.set-primary.
func (h *Handler) callSigilKeySetPrimary(ctx context.Context, claims *jwt.Claims, req jsonRPCRequest, args json.RawMessage) jsonRPCResponse {
	const toolName = "keeper.sigil.key.set-primary"

	if h.deps.SigilKeySvc == nil {
		return h.toolError(req.ID, toolName, mcpCodeInternalError, sigilKeyNotConfigured)
	}

	// RBAC BEFORE unmarshal/regex validation (parity with introduce/list and
	// REST middleware): key_id validity must not leak to an operator
	// without key-set-primary permission.
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

	// RBAC BEFORE unmarshal/regex validation (parity with introduce/list and
	// REST middleware): key_id validity must not leak to an operator
	// without key-retire permission.
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
