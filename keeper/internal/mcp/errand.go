package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"time"

	"github.com/souls-guild/soul-stack/keeper/internal/errand"
	"github.com/souls-guild/soul-stack/keeper/internal/jwt"
	"github.com/souls-guild/soul-stack/keeper/internal/soul"
)

// errandNotConfigured — public-detail nil-guard for errand-tools.
// ErrandDispatcher/ErrandStore are optional HandlerDeps fields; when nil the
// tool still dispatches but returns internal-error (pushNotConfigured pattern).
const errandNotConfigured = "errand orchestrator is not configured"

// errandRunArgs — arguments for keeper.soul.errand.run (schemaErrandRunInput).
// 1:1 with the JSON body of POST /v1/souls/{sid}/exec + an explicit sid in
// args (in HTTP it's a path-param, here it's an object field).
type errandRunArgs struct {
	SID            string         `json:"sid"`
	Module         string         `json:"module"`
	Input          map[string]any `json:"input,omitempty"`
	TimeoutSeconds int            `json:"timeout_seconds,omitempty"`
	DryRun         bool           `json:"dry_run,omitempty"`
}

// errandRunOutput — structured output of keeper.soul.errand.run. Mirrors the
// handler's errandResultResponse (snake_case fields, parity with REST
// 200/202). Async=true → status="running" + errand_id only; the call returns
// it as the async payload, the operator follows up via keeper.errand.get.
type errandRunOutput struct {
	ErrandID        string         `json:"errand_id"`
	SID             string         `json:"sid"`
	Module          string         `json:"module"`
	Status          string         `json:"status"`
	Async           bool           `json:"async"`
	ExitCode        *int32         `json:"exit_code,omitempty"`
	Stdout          string         `json:"stdout,omitempty"`
	Stderr          string         `json:"stderr,omitempty"`
	StdoutTruncated bool           `json:"stdout_truncated,omitempty"`
	StderrTruncated bool           `json:"stderr_truncated,omitempty"`
	DurationMs      *int64         `json:"duration_ms,omitempty"`
	ErrorMessage    string         `json:"error_message,omitempty"`
	Output          map[string]any `json:"output,omitempty"`
}

// callSoulErrandRun — mutating tool keeper.soul.errand.run. Transport over
// [errand.Dispatcher.Dispatch]: all business logic (validate, INSERT, send,
// wait-loop, async-escalation, audit) lives in the dispatcher; the tool
// decodes input, checks permission, maps sentinels to MCP codes.
//
// RBAC — errand.run with selector `host=<sid>` (rbac.md §Errand), mirrors
// REST middleware.RequirePermission(errand, run, ErrandSIDSelector).
func (h *Handler) callSoulErrandRun(ctx context.Context, claims *jwt.Claims, req jsonRPCRequest, args json.RawMessage) jsonRPCResponse {
	const toolName = "keeper.soul.errand.run"

	if h.deps.ErrandDispatcher == nil {
		return h.toolError(req.ID, toolName, mcpCodeInternalError, errandNotConfigured)
	}

	var a errandRunArgs
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
	if a.Module == "" {
		return h.toolError(req.ID, toolName, mcpCodeValidationFailed, "field 'module' is required")
	}

	// RBAC: errand.run, selector host=<sid> (rbac.md §Errand).
	if err := h.deps.RBAC.Check(claims.Subject, "errand", "run", map[string]string{"host": a.SID}); err != nil {
		return h.toolError(req.ID, toolName, mcpCodeForbidden,
			"operator lacks required permission errand.run")
	}

	res, err := h.deps.ErrandDispatcher.Dispatch(ctx, errand.DispatchRequest{
		SID:          a.SID,
		Module:       a.Module,
		Input:        a.Input,
		TimeoutSec:   a.TimeoutSeconds,
		DryRun:       a.DryRun,
		StartedByAID: claims.Subject,
	})
	if err != nil {
		return h.mapErrandDispatchError(req.ID, toolName, err)
	}

	out := errandRunOutput{
		ErrandID:        res.ErrandID,
		SID:             a.SID,
		Module:          a.Module,
		Status:          string(res.Status),
		Async:           res.Async,
		ExitCode:        res.ExitCode,
		Stdout:          res.Stdout,
		Stderr:          res.Stderr,
		StdoutTruncated: res.StdoutTruncated,
		StderrTruncated: res.StderrTruncated,
		DurationMs:      res.DurationMs,
		ErrorMessage:    res.ErrorMessage,
		Output:          res.Output,
	}
	return h.toolResult(req.ID, out)
}

// errandListArgs — arguments keeper.errand.list (schemaErrandListInput).
type errandListArgs struct {
	SID          string `json:"sid,omitempty"`
	Status       string `json:"status,omitempty"`
	StartedAfter string `json:"started_after,omitempty"`
	Offset       int    `json:"offset,omitempty"`
	Limit        int    `json:"limit,omitempty"`
}

// errandRow — JSON form of an errand row for list/get output (mirrors the
// handler's errandResultResponse). finished_at is a pointer so it can be
// properly absent from the output for running errands (omitempty).
type errandRow struct {
	ErrandID        string         `json:"errand_id"`
	SID             string         `json:"sid"`
	Module          string         `json:"module"`
	Status          string         `json:"status"`
	ExitCode        *int32         `json:"exit_code,omitempty"`
	Stdout          string         `json:"stdout,omitempty"`
	Stderr          string         `json:"stderr,omitempty"`
	StdoutTruncated bool           `json:"stdout_truncated,omitempty"`
	StderrTruncated bool           `json:"stderr_truncated,omitempty"`
	DurationMs      *int64         `json:"duration_ms,omitempty"`
	ErrorMessage    string         `json:"error_message,omitempty"`
	Output          map[string]any `json:"output,omitempty"`
	StartedByAID    string         `json:"started_by_aid"`
	StartedAt       string         `json:"started_at"`
	FinishedAt      string         `json:"finished_at,omitempty"`
}

// errandListOutput — paged-list output for keeper.errand.list. Fields 1:1
// with schemaPaginatedListOutput.
type errandListOutput struct {
	Items  []errandRow `json:"items"`
	Offset int         `json:"offset"`
	Limit  int         `json:"limit"`
	Total  int         `json:"total"`
}

// callErrandList — read-only tool keeper.errand.list. Transport over
// [errand.Store.List]. Pagination clamp matches api.ParsePage default
// limit=50, max=1000.
func (h *Handler) callErrandList(ctx context.Context, claims *jwt.Claims, req jsonRPCRequest, args json.RawMessage) jsonRPCResponse {
	const toolName = "keeper.errand.list"

	if h.deps.ErrandStore == nil {
		return h.toolError(req.ID, toolName, mcpCodeInternalError, errandNotConfigured)
	}

	var a errandListArgs
	if len(args) > 0 {
		if err := strictUnmarshal(args, &a); err != nil {
			return h.toolError(req.ID, toolName, mcpCodeMalformedRequest,
				"invalid arguments: "+err.Error())
		}
	}

	// RBAC: errand.list without a selector (NoSelector). Per-row filtering by
	// host/coven is a separate slice (see handlers/errand.go::List); no point
	// duplicating it here (read-only).
	if err := h.deps.RBAC.Check(claims.Subject, "errand", "list", nil); err != nil {
		return h.toolError(req.ID, toolName, mcpCodeForbidden,
			"operator lacks required permission errand.list")
	}

	var filter errand.ListFilter
	if a.SID != "" {
		if !soul.ValidSID(a.SID) {
			return h.toolError(req.ID, toolName, mcpCodeValidationFailed,
				"field 'sid' must match "+soul.SIDPattern)
		}
		filter.SID = a.SID
	}
	if a.Status != "" {
		if !validErrandStatusForMCP(a.Status) {
			return h.toolError(req.ID, toolName, mcpCodeValidationFailed,
				"field 'status' must be one of running/success/failed/timed_out/cancelled/module_not_allowed")
		}
		filter.Status = errand.Status(a.Status)
	}
	if a.StartedAfter != "" {
		ts, err := time.Parse(time.RFC3339, a.StartedAfter)
		if err != nil {
			return h.toolError(req.ID, toolName, mcpCodeValidationFailed,
				"field 'started_after' must be RFC3339 timestamp")
		}
		filter.StartedAfter = ts
	}

	offset, limit := clampErrandPage(a.Offset, a.Limit)
	rows, total, err := h.deps.ErrandStore.List(ctx, filter, offset, limit)
	if err != nil {
		h.deps.Logger.Error("mcp: errand.list store failed",
			slog.String("by_aid", claims.Subject),
			slog.Any("error", err))
		return h.toolError(req.ID, toolName, mcpCodeInternalError, "list errands failed")
	}

	items := make([]errandRow, 0, len(rows))
	for _, r := range rows {
		items = append(items, rowToMCP(r))
	}
	return h.toolResult(req.ID, errandListOutput{
		Items:  items,
		Offset: offset,
		Limit:  limit,
		Total:  total,
	})
}

// errandGetArgs — arguments keeper.errand.get.
type errandGetArgs struct {
	ErrandID string `json:"errand_id"`
}

// callErrandGet — read-only tool keeper.errand.get. Transport over
// [errand.Store.Get]. For a running row returns status="running" without
// stdout/exit_code (parity with REST 202).
func (h *Handler) callErrandGet(ctx context.Context, claims *jwt.Claims, req jsonRPCRequest, args json.RawMessage) jsonRPCResponse {
	const toolName = "keeper.errand.get"

	if h.deps.ErrandStore == nil {
		return h.toolError(req.ID, toolName, mcpCodeInternalError, errandNotConfigured)
	}

	var a errandGetArgs
	if len(args) > 0 {
		if err := strictUnmarshal(args, &a); err != nil {
			return h.toolError(req.ID, toolName, mcpCodeMalformedRequest,
				"invalid arguments: "+err.Error())
		}
	}
	if a.ErrandID == "" {
		return h.toolError(req.ID, toolName, mcpCodeValidationFailed,
			"field 'errand_id' is required")
	}

	// RBAC: errand.list (the read permission covers both get and list — rbac.md §Errand).
	if err := h.deps.RBAC.Check(claims.Subject, "errand", "list", nil); err != nil {
		return h.toolError(req.ID, toolName, mcpCodeForbidden,
			"operator lacks required permission errand.list")
	}

	row, err := h.deps.ErrandStore.Get(ctx, a.ErrandID)
	if err != nil {
		if errors.Is(err, errand.ErrNotFound) {
			return h.toolError(req.ID, toolName, mcpCodeNotFound,
				"errand "+a.ErrandID+" not found")
		}
		h.deps.Logger.Error("mcp: errand.get store failed",
			slog.String("errand_id", a.ErrandID),
			slog.String("by_aid", claims.Subject),
			slog.Any("error", err))
		return h.toolError(req.ID, toolName, mcpCodeInternalError, "get errand failed")
	}
	return h.toolResult(req.ID, rowToMCP(row))
}

// errandCancelArgs — arguments keeper.errand.cancel (schemaErrandCancelInput).
type errandCancelArgs struct {
	ErrandID string `json:"errand_id"`
}

// callErrandCancel — mutating tool keeper.errand.cancel (ADR-033 slice E5).
// Transport over [errand.Dispatcher.Cancel]: business logic (lookup,
// terminal-check, send cancel local/remote, audit) lives in the dispatcher.
//
// RBAC — errand.cancel without a selector (NoSelector): the SID is only
// known after looking up the errand row, which is incompatible with a
// pre-check. Mirrors REST DELETE /v1/errands/{errand_id}.
func (h *Handler) callErrandCancel(ctx context.Context, claims *jwt.Claims, req jsonRPCRequest, args json.RawMessage) jsonRPCResponse {
	const toolName = "keeper.errand.cancel"

	if h.deps.ErrandDispatcher == nil {
		return h.toolError(req.ID, toolName, mcpCodeInternalError, errandNotConfigured)
	}

	var a errandCancelArgs
	if len(args) > 0 {
		if err := strictUnmarshal(args, &a); err != nil {
			return h.toolError(req.ID, toolName, mcpCodeMalformedRequest,
				"invalid arguments: "+err.Error())
		}
	}
	if a.ErrandID == "" {
		return h.toolError(req.ID, toolName, mcpCodeValidationFailed,
			"field 'errand_id' is required")
	}

	if err := h.deps.RBAC.Check(claims.Subject, "errand", "cancel", nil); err != nil {
		return h.toolError(req.ID, toolName, mcpCodeForbidden,
			"operator lacks required permission errand.cancel")
	}

	if err := h.deps.ErrandDispatcher.Cancel(ctx, errand.CancelRequest{
		ErrandID:    a.ErrandID,
		RequestedBy: claims.Subject,
	}); err != nil {
		return h.mapErrandCancelError(req.ID, toolName, a.ErrandID, err)
	}

	// 204 equivalent in JSON-RPC: return a minimal ack object so the tool has
	// structured output (schema — schemaErrandCancelOutput).
	return h.toolResult(req.ID, map[string]any{
		"errand_id": a.ErrandID,
		"cancelled": true,
	})
}

// mapErrandCancelError maps errand.Dispatcher.Cancel sentinels to an MCP tool error.
func (h *Handler) mapErrandCancelError(id json.RawMessage, toolName, errandID string, err error) jsonRPCResponse {
	switch {
	case errors.Is(err, errand.ErrEmptyErrandID):
		return h.toolError(id, toolName, mcpCodeValidationFailed,
			"field 'errand_id' is required")
	case errors.Is(err, errand.ErrNotFound):
		return h.toolError(id, toolName, mcpCodeNotFound,
			"errand "+errandID+" not found")
	case errors.Is(err, errand.ErrErrandTerminal):
		return h.toolError(id, toolName, mcpCodeErrandNotCancellable,
			"errand "+errandID+" is already in a terminal state")
	case errors.Is(err, errand.ErrSoulNotConnected):
		return h.toolError(id, toolName, mcpCodeNotFound,
			"target soul is not connected to the cluster")
	default:
		h.deps.Logger.Error("mcp: errand.cancel failed",
			slog.String("errand_id", errandID),
			slog.Any("error", err))
		return h.toolError(id, toolName, mcpCodeInternalError, "errand cancel failed")
	}
}

// mapErrandDispatchError maps dispatcher sentinels to an MCP tool error.
// Mirrors handlers/errand.go::writeDispatchError.
func (h *Handler) mapErrandDispatchError(id json.RawMessage, toolName string, err error) jsonRPCResponse {
	switch {
	case errors.Is(err, errand.ErrSIDEmpty):
		return h.toolError(id, toolName, mcpCodeValidationFailed, "sid is empty")
	case errors.Is(err, errand.ErrModuleEmpty):
		return h.toolError(id, toolName, mcpCodeValidationFailed, "field 'module' is required")
	case errors.Is(err, errand.ErrTimeoutOutOfRange):
		return h.toolError(id, toolName, mcpCodeValidationFailed,
			"field 'timeout_seconds' must be in [1, 300]")
	case errors.Is(err, errand.ErrSoulNotConnected):
		return h.toolError(id, toolName, mcpCodeNotFound,
			"target soul is not connected to the cluster")
	default:
		h.deps.Logger.Error("mcp: errand.dispatch failed", slog.Any("error", err))
		return h.toolError(id, toolName, mcpCodeInternalError, "errand dispatch failed")
	}
}

// rowToMCP converts errand.Row → errandRow (MCP JSON form). Parallels
// handlers/errand.go::rowToResponse — same JSON tags, same types.
func rowToMCP(row *errand.Row) errandRow {
	out := errandRow{
		ErrandID:        row.ErrandID,
		SID:             row.SID,
		Module:          row.Module,
		Status:          string(row.Status),
		ExitCode:        row.ExitCode,
		Stdout:          row.Stdout,
		Stderr:          row.Stderr,
		StdoutTruncated: row.StdoutTruncated,
		StderrTruncated: row.StderrTruncated,
		DurationMs:      row.DurationMs,
		ErrorMessage:    row.ErrorMessage,
		Output:          row.Output,
		StartedByAID:    row.StartedByAID,
		StartedAt:       row.StartedAt.UTC().Format(time.RFC3339),
	}
	if row.FinishedAt != nil {
		out.FinishedAt = row.FinishedAt.UTC().Format(time.RFC3339)
	}
	return out
}

// validErrandStatusForMCP — closed enum for the query filter. Matches the
// handler's validErrandStatus (handlers/errand.go) — a separate copy so the
// MCP package doesn't depend on api/handlers (pattern shared by other tools).
func validErrandStatusForMCP(s string) bool {
	switch errand.Status(s) {
	case errand.StatusRunning, errand.StatusSuccess, errand.StatusFailed,
		errand.StatusTimedOut, errand.StatusCancelled, errand.StatusModuleNotAllowed:
		return true
	}
	return false
}

// clampErrandPage clamps offset/limit per schemaPaginatedListOutput
// (limit ∈ [1, 1000], default 50; offset ≥ 0, default 0). A minimal helper
// with no dependency on shared/api.ParsePage (which parses url.Values).
func clampErrandPage(offset, limit int) (int, int) {
	if offset < 0 {
		offset = 0
	}
	if limit <= 0 {
		limit = 50
	}
	if limit > 1000 {
		limit = 1000
	}
	return offset, limit
}
