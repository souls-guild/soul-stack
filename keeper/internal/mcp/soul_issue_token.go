package mcp

import (
	"context"
	"encoding/json"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/souls-guild/soul-stack/keeper/internal/bootstraptoken"
	"github.com/souls-guild/soul-stack/keeper/internal/jwt"
	"github.com/souls-guild/soul-stack/keeper/internal/soul"
	"github.com/souls-guild/soul-stack/shared/audit"
)

// keeper.soul.issue-token — паритет REST POST /v1/souls/{sid}/issue-token
// (SoulHandler.IssueToken). Повторная выписка bootstrap-токена для
// существующей Soul (transport=agent). force=true истекает активный токен и
// выписывает новый (в REST это `?force=true` query-param).
//
// Логика (select soul → проверка transport → опц. expire active → insert)
// воспроизводится над тем же [handlers.SoulPool] и функциями
// soul.* / bootstraptoken.* — паритет по DB-границе, без новой абстракции.

type soulIssueTokenArgs struct {
	SID   string `json:"sid"`
	Force bool   `json:"force,omitempty"`
}

// soulIssueTokenOutput — паритет REST SoulIssueTokenReply. json-тег
// `expires_at` (не token_expires_at) синхронен REST и openapi.yaml.
type soulIssueTokenOutput struct {
	SID            string `json:"sid"`
	BootstrapToken string `json:"bootstrap_token"`
	TokenExpiresAt string `json:"expires_at"`
}

func (h *Handler) callSoulIssueToken(ctx context.Context, claims *jwt.Claims, req jsonRPCRequest, args json.RawMessage) jsonRPCResponse {
	const toolName = "keeper.soul.issue-token"

	if h.deps.SoulDB == nil {
		return h.toolError(req.ID, toolName, mcpCodeInternalError, "soul DB is not configured")
	}

	var a soulIssueTokenArgs
	if len(args) > 0 {
		if err := strictUnmarshal(args, &a); err != nil {
			return h.toolError(req.ID, toolName, mcpCodeMalformedRequest,
				"invalid arguments: "+err.Error())
		}
	}
	if a.SID == "" {
		return h.toolError(req.ID, toolName, mcpCodeValidationFailed, "field 'sid' is required")
	}
	if !soul.ValidSID(a.SID) {
		return h.toolError(req.ID, toolName, mcpCodeValidationFailed,
			"field 'sid' must match "+soul.SIDPattern)
	}

	// RBAC-check — `soul.issue-token` с селектором `host=<sid>` (REST:
	// SoulSIDSelector). RBAC может ограничить ре-выписку по конкретному хосту.
	if err := h.deps.RBAC.Check(claims.Subject, "soul", "issue-token", map[string]string{"host": a.SID}); err != nil {
		return h.toolError(req.ID, toolName, mcpCodeForbidden,
			"operator lacks required permission soul.issue-token")
	}

	tx, err := h.deps.SoulDB.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		h.deps.Logger.Error("mcp: soul.issue-token begin tx failed",
			slog.String("sid", a.SID), slog.Any("error", err))
		return h.toolError(req.ID, toolName, mcpCodeInternalError, "issue token failed")
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback(context.Background())
		}
	}()

	s, err := soul.SelectBySID(ctx, tx, a.SID)
	if err != nil {
		code, detail := mapSoulErrorToMCP(err)
		if code == mcpCodeInternalError {
			h.deps.Logger.Error("mcp: soul.issue-token select failed",
				slog.String("sid", a.SID), slog.Any("error", err))
		}
		return h.toolError(req.ID, toolName, code, detail)
	}
	if s.Transport != soul.TransportAgent {
		return h.toolError(req.ID, toolName, mcpCodeValidationFailed,
			"soul "+a.SID+" has transport "+string(s.Transport)+"; bootstrap tokens are only issued for transport=agent")
	}

	creator := claims.Subject
	var expiredPrevious bool
	if a.Force {
		_, expired, err := bootstraptoken.ExpireActiveBySID(ctx, tx, a.SID, bootstraptoken.SystemKIDForceReissue)
		if err != nil {
			h.deps.Logger.Error("mcp: soul.issue-token expire active failed",
				slog.String("sid", a.SID), slog.Any("error", err))
			return h.toolError(req.ID, toolName, mcpCodeInternalError, "issue token failed")
		}
		expiredPrevious = expired
	}

	plain, err := bootstraptoken.Generate()
	if err != nil {
		h.deps.Logger.Error("mcp: soul.issue-token generate failed",
			slog.String("sid", a.SID), slog.Any("error", err))
		return h.toolError(req.ID, toolName, mcpCodeInternalError, "issue token failed")
	}
	rec, err := bootstraptoken.Insert(ctx, tx, a.SID, plain.Hash(), bootstraptoken.DefaultTokenTTL, &creator)
	if err != nil {
		code, detail := mapSoulErrorToMCP(err)
		if code == mcpCodeInternalError {
			h.deps.Logger.Error("mcp: soul.issue-token insert failed",
				slog.String("sid", a.SID), slog.Any("error", err))
		}
		return h.toolError(req.ID, toolName, code, detail)
	}

	if err := tx.Commit(ctx); err != nil {
		h.deps.Logger.Error("mcp: soul.issue-token commit failed",
			slog.String("sid", a.SID), slog.Any("error", err))
		return h.toolError(req.ID, toolName, mcpCodeInternalError, "issue token failed")
	}
	committed = true

	// Audit — паритет REST: non-sensitive факты под именами без `token`-substring
	// (audit secret-mask редактирует любой ключ с `token`). Сам токен не пишем.
	h.writeAudit(audit.EventSoulTokenIssued, creator, map[string]any{
		"sid":              a.SID,
		"force":            a.Force,
		"expired_previous": expiredPrevious,
		"expires_at":       rec.ExpiresAt.UTC().Format(time.RFC3339),
	})

	return h.toolResult(req.ID, soulIssueTokenOutput{
		SID:            a.SID,
		BootstrapToken: plain.Reveal(),
		TokenExpiresAt: rec.ExpiresAt.UTC().Format(time.RFC3339),
	})
}
