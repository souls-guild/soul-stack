package mcp

import (
	"context"
	"encoding/json"
	"log/slog"

	"github.com/souls-guild/soul-stack/keeper/internal/incarnation"
	"github.com/souls-guild/soul-stack/keeper/internal/jwt"
	"github.com/souls-guild/soul-stack/shared/audit"
)

// incarnationListArgs — arguments for keeper.incarnation.list
// (schemaIncarnationListInput): all fields optional. service / status are
// filters (parity with REST query params), offset / limit are pagination.
//
// offset / limit are pointers: distinguishes "operator didn't pass it" (→
// defaults 0 / 50, like REST [sharedapi.ParsePage]) from an explicit zero.
// strictUnmarshal rejects unknown fields; integer types are validated by
// the JSON decoder.
type incarnationListArgs struct {
	Service string `json:"service"`
	Status  string `json:"status"`
	Offset  *int   `json:"offset"`
	Limit   *int   `json:"limit"`
}

// listDefaultLimit / listMaxLimit — pagination default and ceiling, parity
// with REST [sharedapi.ParsePage] (offset≥0, 1≤limit≤1000, default 50). The
// MCP tool takes page params directly (no url.Values), so validation lives here.
const (
	listDefaultLimit = 50
	listMaxLimit     = 1000
)

// incarnationListOutput — output of keeper.incarnation.list. Mirrors REST
// PagedResponse[incarnationDTO]: items + offset/limit/total. Each element is
// the same masked DTO as keeper.incarnation.get.
type incarnationListOutput struct {
	Items  []incarnationGetOutput `json:"items"`
	Offset int                    `json:"offset"`
	Limit  int                    `json:"limit"`
	Total  int                    `json:"total"`
}

// callIncarnationList — read tool keeper.incarnation.list. Parity with REST
// IncarnationHandler.List: service/status filter + pagination → SelectAll →
// array of masked DTOs.
//
// RBAC-context — nil (list has no name targeting, incarnationRBACContext("")
// = nil, parity with REST NoSelector). Reads are not audited.
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

	// scope Unrestricted: MCP incarnation.list doesn't yet apply per-operator
	// scoped visibility (ADR-047 S3b-3 covered HTTP List/Get; MCP is a
	// separate follow-up sweep). Behavior is unchanged (no scope filter).
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
