package mcp

import (
	"context"
	"encoding/json"
	"log/slog"
	"time"

	"github.com/souls-guild/soul-stack/keeper/internal/incarnation"
	"github.com/souls-guild/soul-stack/keeper/internal/jwt"
	"github.com/souls-guild/soul-stack/shared/audit"
)

// incarnationHistoryArgs — arguments for keeper.incarnation.history
// (schemaIncarnationHistoryInput): name is required; offset/limit/apply_id
// are optional. apply_id filters the page to one specific run (parity with
// the REST query param).
type incarnationHistoryArgs struct {
	Name    string `json:"name"`
	ApplyID string `json:"apply_id"`
	Offset  *int   `json:"offset"`
	Limit   *int   `json:"limit"`
}

// incarnationHistoryEntry — one element of the history output. Mirrors the
// REST historyDTO: state_before/state_after go through [audit.MaskSecrets]
// (defense-in-depth — history is a second external channel for reading state).
type incarnationHistoryEntry struct {
	HistoryID    string         `json:"history_id"`
	Scenario     string         `json:"scenario"`
	StateBefore  map[string]any `json:"state_before"`
	StateAfter   map[string]any `json:"state_after"`
	ChangedByAID string         `json:"changed_by_aid,omitempty"`
	ApplyID      string         `json:"apply_id"`
	At           time.Time      `json:"created_at"`
}

// incarnationHistoryOutput — output of keeper.incarnation.history. Mirrors
// REST PagedResponse[historyDTO].
type incarnationHistoryOutput struct {
	Items  []incarnationHistoryEntry `json:"items"`
	Offset int                       `json:"offset"`
	Limit  int                       `json:"limit"`
	Total  int                       `json:"total"`
}

// callIncarnationHistory — read-tool keeper.incarnation.history. Parity
// with REST IncarnationHandler.History: existence probe (404 for a
// non-existent name, otherwise an empty history is indistinguishable from
// an existing incarnation with no history) → HistorySelectByName → masked
// DTO.
//
// RBAC context — {"incarnation": name} (name-bound, parity with REST
// IncarnationNameSelector). Reads are NOT audited.
func (h *Handler) callIncarnationHistory(ctx context.Context, claims *jwt.Claims, req jsonRPCRequest, args json.RawMessage) jsonRPCResponse {
	const toolName = "keeper.incarnation.history"

	var a incarnationHistoryArgs
	if len(args) > 0 {
		if err := strictUnmarshal(args, &a); err != nil {
			return h.toolError(req.ID, toolName, mcpCodeMalformedRequest,
				"invalid arguments: "+err.Error())
		}
	}
	if a.Name == "" {
		return h.toolError(req.ID, toolName, mcpCodeValidationFailed, "field 'name' is required")
	}
	if !incarnation.ValidName(a.Name) {
		return h.toolError(req.ID, toolName, mcpCodeValidationFailed,
			"field 'name' must match "+incarnation.NamePattern)
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

	var filter incarnation.HistoryFilter
	if a.ApplyID != "" {
		if !audit.IsValidULID(a.ApplyID) {
			return h.toolError(req.ID, toolName, mcpCodeMalformedRequest,
				"field 'apply_id' must be a Crockford-base32 ULID (26 chars)")
		}
		filter.ApplyID = a.ApplyID
	}

	// Existence probe + source of coven/service scope: otherwise a
	// non-existent name yields an empty history (total=0), indistinguishable
	// from an existing incarnation with no history (parity with REST
	// History). inc is needed for the RBAC OR-check (covens ∪ {name}).
	inc, err := incarnation.SelectByName(ctx, h.deps.IncarnationDB, a.Name)
	if err != nil {
		// Fail-closed RBAC when incarnation lookup fails or is not found (parity with REST).
		if scopeErr := h.checkIncarnationScope(claims, "history", a.Name, "", nil); scopeErr != nil {
			return h.toolError(req.ID, toolName, mcpCodeForbidden,
				"operator lacks required permission incarnation.history")
		}
		code, detail := mapIncarnationErrorToMCP(err)
		if code == mcpCodeInternalError {
			h.deps.Logger.Error("mcp: incarnation.history existence-probe failed",
				slog.String("name", a.Name),
				slog.String("by_aid", claims.Subject),
				slog.Any("error", err),
			)
		}
		return h.toolError(req.ID, toolName, code, detail)
	}

	// RBAC OR-check over the incarnation's coven/service scope (covens ∪
	// {name}) — mirrors REST middleware, scope from inc.Service / inc.Covens.
	if err := h.checkIncarnationScope(claims, "history", inc.Name, inc.Service, inc.Covens); err != nil {
		return h.toolError(req.ID, toolName, mcpCodeForbidden,
			"operator lacks required permission incarnation.history")
	}

	items, total, err := incarnation.HistorySelectByName(ctx, h.deps.IncarnationDB, a.Name, filter, offset, limit)
	if err != nil {
		code, detail := mapIncarnationErrorToMCP(err)
		if code == mcpCodeInternalError {
			h.deps.Logger.Error("mcp: incarnation.history select failed",
				slog.String("name", a.Name),
				slog.String("apply_id", filter.ApplyID),
				slog.String("by_aid", claims.Subject),
				slog.Any("error", err),
			)
		}
		return h.toolError(req.ID, toolName, code, detail)
	}

	out := incarnationHistoryOutput{
		Items:  make([]incarnationHistoryEntry, 0, len(items)),
		Offset: offset,
		Limit:  limit,
		Total:  total,
	}
	for _, e := range items {
		entry := incarnationHistoryEntry{
			HistoryID:   e.HistoryID,
			Scenario:    e.Scenario,
			StateBefore: audit.MaskSecrets(e.StateBefore),
			StateAfter:  audit.MaskSecrets(e.StateAfter),
			ApplyID:     e.ApplyID,
			At:          e.At,
		}
		if e.ChangedByAID != nil {
			entry.ChangedByAID = *e.ChangedByAID
		}
		out.Items = append(out.Items, entry)
	}
	return h.toolResult(req.ID, out)
}
