package mcp

import (
	"context"
	"encoding/json"
	"log/slog"

	"github.com/souls-guild/soul-stack/keeper/internal/incarnation"
	"github.com/souls-guild/soul-stack/keeper/internal/jwt"
	"github.com/souls-guild/soul-stack/shared/audit"
)

// incarnationListArgs — arguments tool-а keeper.incarnation.list
// (schemaIncarnationListInput): все поля опциональны. service / status —
// фильтры (паритет REST query-params), offset / limit — pagination.
//
// offset / limit — указатели: отличить «оператор не передал» (→ дефолты
// 0 / 50, как REST [sharedapi.ParsePage]) от явного нуля. strictUnmarshal
// отвергает unknown-поля; типы integer валидируются JSON-decoder-ом.
type incarnationListArgs struct {
	Service string `json:"service"`
	Status  string `json:"status"`
	Offset  *int   `json:"offset"`
	Limit   *int   `json:"limit"`
}

// listDefaultLimit / listMaxLimit — дефолт и потолок pagination, паритет с
// REST [sharedapi.ParsePage] (offset≥0, 1≤limit≤1000, дефолт 50). MCP-tool
// принимает page-params напрямую (без url.Values), поэтому валидация — здесь.
const (
	listDefaultLimit = 50
	listMaxLimit     = 1000
)

// incarnationListOutput — output keeper.incarnation.list. Симметричен REST
// PagedResponse[incarnationDTO]: items + offset/limit/total. Каждый элемент —
// тот же masked-DTO, что keeper.incarnation.get.
type incarnationListOutput struct {
	Items  []incarnationGetOutput `json:"items"`
	Offset int                    `json:"offset"`
	Limit  int                    `json:"limit"`
	Total  int                    `json:"total"`
}

// callIncarnationList — read-tool keeper.incarnation.list. Паритет REST
// IncarnationHandler.List: фильтр service/status + pagination → SelectAll →
// массив masked-DTO.
//
// RBAC-context — nil (list без таргетинга по имени, incarnationRBACContext("")
// = nil, паритет с REST NoSelector). reads НЕ аудируются.
func (h *Handler) callIncarnationList(ctx context.Context, claims *jwt.Claims, req jsonRPCRequest, args json.RawMessage) jsonRPCResponse {
	const toolName = "keeper.incarnation.list"

	var a incarnationListArgs
	if len(args) > 0 {
		if err := strictUnmarshal(args, &a); err != nil {
			return h.toolError(req.ID, toolName, mcpCodeMalformedRequest,
				"invalid arguments: "+err.Error())
		}
	}

	offset := 0
	if a.Offset != nil {
		offset = *a.Offset
	}
	limit := listDefaultLimit
	if a.Limit != nil {
		limit = *a.Limit
	}
	if offset < 0 {
		return h.toolError(req.ID, toolName, mcpCodeValidationFailed, "field 'offset' must be >= 0")
	}
	if limit < 1 || limit > listMaxLimit {
		return h.toolError(req.ID, toolName, mcpCodeValidationFailed, "field 'limit' must be between 1 and 1000")
	}

	var filter incarnation.ListFilter
	filter.Service = a.Service
	if a.Status != "" {
		st := incarnation.Status(a.Status)
		if !incarnation.ValidStatus(st) {
			return h.toolError(req.ID, toolName, mcpCodeValidationFailed,
				"field 'status' must be one of ready/applying/error_locked/migration_failed")
		}
		filter.Status = st
	}

	if err := h.deps.RBAC.Check(claims.Subject, "incarnation", "list", incarnationRBACContext("")); err != nil {
		return h.toolError(req.ID, toolName, mcpCodeForbidden,
			"operator lacks required permission incarnation.list")
	}

	// scope Unrestricted: MCP incarnation.list пока не применяет per-operator
	// scoped-видимость (ADR-047 S3b-3 покрыл HTTP-List/Get; MCP — отдельный
	// follow-up sweep). Поведение идентично прежнему (без scope-фильтра).
	items, total, err := incarnation.SelectAll(ctx, h.deps.IncarnationDB, filter, incarnation.ListScope{Unrestricted: true}, offset, limit)
	if err != nil {
		code, detail := mapIncarnationErrorToMCP(err)
		if code == mcpCodeInternalError {
			h.deps.Logger.Error("mcp: incarnation.list select failed",
				slog.Any("filter", filter),
				slog.String("by_aid", claims.Subject),
				slog.Any("error", err),
			)
		}
		return h.toolError(req.ID, toolName, code, detail)
	}

	out := incarnationListOutput{
		Items:  make([]incarnationGetOutput, 0, len(items)),
		Offset: offset,
		Limit:  limit,
		Total:  total,
	}
	for _, inc := range items {
		out.Items = append(out.Items, incarnationGetOutput{
			Name:               inc.Name,
			Service:            inc.Service,
			ServiceVersion:     inc.ServiceVersion,
			StateSchemaVersion: inc.StateSchemaVersion,
			Spec:               audit.MaskSecrets(inc.Spec),
			State:              audit.MaskSecrets(inc.State),
			Status:             string(inc.Status),
			StatusDetails:      inc.StatusDetails,
			CreatedByAID:       inc.CreatedByAID,
			CreatedAt:          inc.CreatedAt,
			UpdatedAt:          inc.UpdatedAt,
		})
	}
	return h.toolResult(req.ID, out)
}
