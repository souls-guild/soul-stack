package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"

	"github.com/souls-guild/soul-stack/keeper/internal/jwt"
	"github.com/souls-guild/soul-stack/keeper/internal/pushorch"
)

// pushNotConfigured — public-detail nil-guard-а push-tool-ов. PushRun — опц.
// поле HandlerDeps (production-wire-up передаёт *pushorch.PushRun только при
// поднятом SshDispatcher): при nil tool диспатчится, но возвращает
// internal-error «не сконфигурировано» (паттерн RBACRoles/SigilSvc).
const pushNotConfigured = "push orchestrator is not configured"

// pushApplyArgs — arguments tool-а keeper.push.apply (schemaPushApplyInput):
// inventory + destiny обязательны, остальное опционально. cleanup_stale_versions
// маппится в request.CleanupStale (короткое имя в pushorch.ApplyRequest).
type pushApplyArgs struct {
	Inventory            []string       `json:"inventory"`
	Destiny              string         `json:"destiny"`
	Input                map[string]any `json:"input,omitempty"`
	SSHProvider          string         `json:"ssh_provider,omitempty"`
	CleanupStaleVersions bool           `json:"cleanup_stale_versions,omitempty"`
}

// callPushApply — mutating-tool keeper.push.apply. Транспорт поверх
// [pushorch.PushRun.Apply]: вся бизнес-логика (parse destiny, Insert(pending),
// async-execute) — в orchestrator-е; tool декодирует input, проверяет
// permission, маппит sentinel-ы в MCP-коды. Audit (push.applied) пишется
// orchestrator-ом — здесь дубль не нужен.
//
// RBAC — push.apply без селектора (push не имеет таргетинга по
// incarnation/coven в MVP — селекторы service/coven/incarnation/host из
// closed enum не покрывают push.* в текущем slice). Контекст nil — право не
// зависит от тела запроса.
func (h *Handler) callPushApply(ctx context.Context, claims *jwt.Claims, req jsonRPCRequest, args json.RawMessage) jsonRPCResponse {
	const toolName = "keeper.push.apply"

	if h.deps.PushRun == nil {
		return h.toolError(req.ID, toolName, mcpCodeInternalError, pushNotConfigured)
	}

	// RBAC ДО unmarshal/валидации (least-disclosure): неавторизованный
	// оператор не получает validation-feedback по телу. Контекст nil — право
	// не зависит от тела запроса.
	if err := h.deps.RBAC.Check(claims.Subject, "push", "apply", nil); err != nil {
		return h.toolError(req.ID, toolName, mcpCodeForbidden,
			"operator lacks required permission push.apply")
	}

	var a pushApplyArgs
	if len(args) > 0 {
		if err := strictUnmarshal(args, &a); err != nil {
			return h.toolError(req.ID, toolName, mcpCodeMalformedRequest,
				"invalid arguments: "+err.Error())
		}
	}
	if len(a.Inventory) == 0 {
		return h.toolError(req.ID, toolName, mcpCodeValidationFailed,
			"field 'inventory' is required and must be non-empty")
	}
	if a.Destiny == "" {
		return h.toolError(req.ID, toolName, mcpCodeValidationFailed,
			"field 'destiny' is required")
	}

	applyID, err := h.deps.PushRun.Apply(ctx, pushorch.ApplyRequest{
		InventorySIDs: a.Inventory,
		DestinyRef:    a.Destiny,
		SSHProvider:   a.SSHProvider,
		Input:         a.Input,
		CleanupStale:  a.CleanupStaleVersions,
		StartedByAID:  claims.Subject,
	})
	if err != nil {
		if errors.Is(err, pushorch.ErrInvalidDestinyRef) {
			return h.toolError(req.ID, toolName, mcpCodeValidationFailed, err.Error())
		}
		h.deps.Logger.Error("mcp: push.apply orchestrator accept failed",
			slog.String("destiny", a.Destiny),
			slog.String("by_aid", claims.Subject),
			slog.Any("error", err))
		return h.toolError(req.ID, toolName, mcpCodeInternalError, "push apply failed")
	}

	// schemaApplyIDOutput → `{ "_apply_id": "<ULID>" }`. Тег JSON _apply_id —
	// контракт schemaApplyIDOutput (см. manifest.go).
	return h.toolResult(req.ID, struct {
		ApplyID string `json:"_apply_id"`
	}{ApplyID: applyID})
}
