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

// errandNotConfigured — public-detail nil-guard-а errand-tool-ов.
// ErrandDispatcher/ErrandStore — опц. поля HandlerDeps; при nil tool
// диспатчится, но возвращает internal-error (паттерн pushNotConfigured).
const errandNotConfigured = "errand orchestrator is not configured"

// errandRunArgs — arguments tool-а keeper.soul.errand.run (schemaErrandRunInput).
// 1:1 с JSON body POST /v1/souls/{sid}/exec + явный sid в args (в HTTP он
// в path-param, тут — поле объекта).
type errandRunArgs struct {
	SID            string         `json:"sid"`
	Module         string         `json:"module"`
	Input          map[string]any `json:"input,omitempty"`
	TimeoutSeconds int            `json:"timeout_seconds,omitempty"`
	DryRun         bool           `json:"dry_run,omitempty"`
}

// errandRunOutput — структурный output keeper.soul.errand.run. Зеркало
// errandResultResponse handler-а (поля snake_case, паритет REST 200/202).
// Async=true → status="running" + только errand_id; вызов вернёт его как
// async-полезный результат, оператор дожимает через keeper.errand.get.
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

// callSoulErrandRun — mutating-tool keeper.soul.errand.run. Транспорт поверх
// [errand.Dispatcher.Dispatch]: вся бизнес-логика (validate, INSERT, send,
// wait-loop, async-escalation, audit) — в dispatcher-е; tool декодирует input,
// проверяет permission, маппит sentinel-ы в MCP-коды.
//
// RBAC — errand.run с селектором `host=<sid>` (rbac.md §Errand), симметрично
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

	// RBAC: errand.run, селектор host=<sid> (rbac.md §Errand).
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

// errandRow — JSON-форма строки errand-а для list/get output (зеркало
// errandResultResponse handler-а). finished_at — pointer, чтобы для running
// серьёзно отсутствовать в output (omitempty).
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

// errandListOutput — paged-list output keeper.errand.list. Поля 1:1 со
// schemaPaginatedListOutput.
type errandListOutput struct {
	Items  []errandRow `json:"items"`
	Offset int         `json:"offset"`
	Limit  int         `json:"limit"`
	Total  int         `json:"total"`
}

// callErrandList — read-only tool keeper.errand.list. Транспорт над
// [errand.Store.List]. Pagination clamp совпадает с api.ParsePage default
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

	// RBAC: errand.list без селектора (NoSelector). Per-row фильтр по host/
	// coven — отдельный slice (см. handlers/errand.go::List), здесь дублировать
	// нет смысла (read-only).
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

// callErrandGet — read-only tool keeper.errand.get. Транспорт над
// [errand.Store.Get]. Для running-строки возвращает status="running" без
// stdout/exit_code (паритет REST 202).
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

	// RBAC: errand.list (read-permission покрывает и get, и list — rbac.md §Errand).
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
// Транспорт поверх [errand.Dispatcher.Cancel]: бизнес-логика (lookup,
// terminal-check, send cancel local/remote, audit) — в dispatcher-е.
//
// RBAC — errand.cancel без селектора (NoSelector): SID известен только после
// lookup-а строки errand-а, что несовместимо с pre-check-ом. Симметрично
// REST DELETE /v1/errands/{errand_id}.
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

	// 204-эквивалент в JSON-RPC: возвращаем минимальный ack-объект, чтобы у tool
	// был structured output (схема — schemaErrandCancelOutput).
	return h.toolResult(req.ID, map[string]any{
		"errand_id": a.ErrandID,
		"cancelled": true,
	})
}

// mapErrandCancelError маппит sentinel-ы errand.Dispatcher.Cancel в MCP-tool-error.
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

// mapErrandDispatchError маппит sentinel-ы dispatcher-а в MCP-tool-error.
// Симметрично handlers/errand.go::writeDispatchError.
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

// rowToMCP — конвертация errand.Row → errandRow (MCP JSON-форма).
// Параллель handlers/errand.go::rowToResponse — JSON-теги те же, типы те же.
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

// validErrandStatusForMCP — closed enum для query-фильтра. Совпадает с
// validErrandStatus handler-а (handlers/errand.go) — отдельная копия, чтобы
// MCP-пакет не зависел от api/handlers (паттерн остальных tool-ов).
func validErrandStatusForMCP(s string) bool {
	switch errand.Status(s) {
	case errand.StatusRunning, errand.StatusSuccess, errand.StatusFailed,
		errand.StatusTimedOut, errand.StatusCancelled, errand.StatusModuleNotAllowed:
		return true
	}
	return false
}

// clampErrandPage — clamp offset/limit под schemaPaginatedListOutput
// (limit ∈ [1, 1000], default 50; offset ≥ 0, default 0). Минимальный
// helper без зависимости от shared/api.ParsePage (она парсит url.Values).
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
