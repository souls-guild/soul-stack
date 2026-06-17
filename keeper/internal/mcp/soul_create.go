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

// keeper.soul.create — паритет REST POST /v1/souls (SoulHandler.Create).
//
// Логика онбординга (souls-row + bootstrap-токен атомарно одной транзакцией)
// живёт в REST-handler-е, а не в переиспользуемом сервисе; здесь она
// воспроизводится над тем же [handlers.SoulPool] и теми же функциями
// soul.* / bootstraptoken.* (single source of truth по DB-границе, без новой
// абстракции — паритет, не новьё).

type soulCreateArgs struct {
	SID       string   `json:"sid"`
	Transport string   `json:"transport"`
	Covens    []string `json:"covens,omitempty"`
	Note      string   `json:"note,omitempty"`
}

// soulCreateOutput — паритет REST SoulCreateReply. bootstrap_token /
// expires_at присутствуют только для transport=agent. json-тег `expires_at`
// (не token_expires_at) синхронен REST и openapi.yaml — REST↔MCP не разъезжаются.
type soulCreateOutput struct {
	SID            string   `json:"sid"`
	Transport      string   `json:"transport"`
	Status         string   `json:"status"`
	Covens         []string `json:"covens"`
	RegisteredAt   string   `json:"registered_at"`
	CreatedByAID   string   `json:"created_by_aid"`
	BootstrapToken string   `json:"bootstrap_token,omitempty"`
	TokenExpiresAt string   `json:"expires_at,omitempty"`
}

func (h *Handler) callSoulCreate(ctx context.Context, claims *jwt.Claims, req jsonRPCRequest, args json.RawMessage) jsonRPCResponse {
	const toolName = "keeper.soul.create"

	if h.deps.SoulDB == nil {
		return h.toolError(req.ID, toolName, mcpCodeInternalError, "soul DB is not configured")
	}

	// RBAC ДО unmarshal/валидации (least-disclosure): неавторизованный оператор
	// не получает validation-feedback по телу. `soul.create` без селектора
	// (REST: NoSelector) — право не зависит от тела запроса.
	if err := h.deps.RBAC.Check(claims.Subject, "soul", "create", nil); err != nil {
		return h.toolError(req.ID, toolName, mcpCodeForbidden,
			"operator lacks required permission soul.create")
	}

	var a soulCreateArgs
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
	transport, ok := parseSoulTransport(a.Transport)
	if !ok {
		return h.toolError(req.ID, toolName, mcpCodeValidationFailed,
			"field 'transport' is required and must be one of: agent, ssh")
	}
	for _, label := range a.Covens {
		if !soul.ValidCoven(label) {
			return h.toolError(req.ID, toolName, mcpCodeValidationFailed,
				"coven label "+label+" must match "+soul.CovenPattern)
		}
	}

	creator := claims.Subject
	s := &soul.Soul{
		SID:          a.SID,
		Transport:    transport,
		Status:       soul.StatusPending,
		Coven:        a.Covens,
		CreatedByAID: &creator,
		Note:         a.Note,
	}

	// ssh-хост: только souls-row, без bootstrap-токена.
	issueToken := transport == soul.TransportAgent

	tx, err := h.deps.SoulDB.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		h.deps.Logger.Error("mcp: soul.create begin tx failed",
			slog.String("sid", a.SID), slog.Any("error", err))
		return h.toolError(req.ID, toolName, mcpCodeInternalError, "create soul failed")
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback(context.Background())
		}
	}()

	if err := soul.Insert(ctx, tx, s); err != nil {
		code, detail := mapSoulErrorToMCP(err)
		if code == mcpCodeInternalError {
			h.deps.Logger.Error("mcp: soul.create insert failed",
				slog.String("sid", a.SID),
				slog.String("by_aid", creator),
				slog.Any("error", err),
			)
		}
		return h.toolError(req.ID, toolName, code, detail)
	}

	out := soulCreateOutput{
		SID:          s.SID,
		Transport:    string(s.Transport),
		Status:       string(s.Status),
		Covens:       coalesceCoven(s.Coven),
		RegisteredAt: s.RegisteredAt.UTC().Format(time.RFC3339),
		CreatedByAID: creator,
	}

	if issueToken {
		plain, err := bootstraptoken.Generate()
		if err != nil {
			h.deps.Logger.Error("mcp: soul.create token generate failed",
				slog.String("sid", a.SID), slog.Any("error", err))
			return h.toolError(req.ID, toolName, mcpCodeInternalError, "create soul failed")
		}
		rec, err := bootstraptoken.Insert(ctx, tx, a.SID, plain.Hash(), bootstraptoken.DefaultTokenTTL, &creator)
		if err != nil {
			code, detail := mapSoulErrorToMCP(err)
			if code == mcpCodeInternalError {
				h.deps.Logger.Error("mcp: soul.create token insert failed",
					slog.String("sid", a.SID), slog.Any("error", err))
			}
			return h.toolError(req.ID, toolName, code, detail)
		}
		out.BootstrapToken = plain.Reveal()
		out.TokenExpiresAt = rec.ExpiresAt.UTC().Format(time.RFC3339)
	}

	if err := tx.Commit(ctx); err != nil {
		h.deps.Logger.Error("mcp: soul.create commit failed",
			slog.String("sid", a.SID), slog.Any("error", err))
		return h.toolError(req.ID, toolName, mcpCodeInternalError, "create soul failed")
	}
	committed = true

	// Audit — паритет REST: payload {sid, transport, covens, created_by_aid,
	// token_issued}. Сам bootstrap-токен (sensitive) не пишем.
	h.writeAudit(audit.EventSoulCreated, creator, map[string]any{
		"sid":            s.SID,
		"transport":      string(s.Transport),
		"covens":         out.Covens,
		"created_by_aid": creator,
		"token_issued":   issueToken,
	})

	return h.toolResult(req.ID, out)
}

// parseSoulTransport маппит JSON-строку transport в [soul.Transport].
// Возвращает ok=false на пустую/неизвестную строку (паритет
// handlers.parseTransport — последний приватен для пакета handlers).
func parseSoulTransport(v string) (soul.Transport, bool) {
	switch soul.Transport(v) {
	case soul.TransportAgent:
		return soul.TransportAgent, true
	case soul.TransportSSH:
		return soul.TransportSSH, true
	default:
		return "", false
	}
}

// coalesceCoven нормализует nil-slice в пустой — для JSON `[]` вместо `null`
// (covens объявлен non-nullable, паритет handlers.coalesceCoven, приватного
// для пакета handlers).
func coalesceCoven(c []string) []string {
	if c == nil {
		return []string{}
	}
	return c
}
