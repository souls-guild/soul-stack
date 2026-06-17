package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/souls-guild/soul-stack/keeper/internal/api/handlers"
	"github.com/souls-guild/soul-stack/keeper/internal/api/middleware"
	"github.com/souls-guild/soul-stack/keeper/internal/jwt"
	"github.com/souls-guild/soul-stack/shared/audit"
)

// voyageNotConfigured — public-detail nil-guard-а voyage-tool-ов. Зависимости
// (VoyageDB/VoyageScenarioResolver/VoyageCommandResolver) — опц. поля
// HandlerDeps; при nil любого tool диспатчится, но возвращает internal-error
// (паттерн errandRunNotConfigured).
const voyageNotConfigured = "voyage orchestrator is not configured"

// ensureVoyageHandler — ленивая сборка [handlers.VoyageHandler] поверх existing
// HandlerDeps. MCP-tool-ы переиспользуют HTTP-handler-логику целиком (validate/
// resolve/RBAC-by-kind/insert/audit одинаковые между HTTP и MCP) через in-memory
// httptest-инфраструктуру — дешевле, чем дублировать резолв + RBAC второй раз.
//
// enforcer=h.deps.RBAC (тот же, что REST) — RBAC-by-kind guard живёт ВНУТРИ
// VoyageHandler.Create/Cancel, поэтому MCP-вызов получает ту же fail-closed
// проверку без дубля (в отличие от errand_run, где pre-check дублируется).
// IncarnationDB — per-incarnation scope-check scenario-create-а.
func (h *Handler) ensureVoyageHandler() *handlers.VoyageHandler {
	if h.deps.VoyageDB == nil || h.deps.VoyageScenarioResolver == nil ||
		h.deps.VoyageCommandResolver == nil {
		return nil
	}
	return handlers.NewVoyageHandler(
		h.deps.VoyageDB,
		h.deps.VoyageScenarioResolver,
		h.deps.VoyageCommandResolver,
		h.deps.IncarnationDB,
		h.deps.RBAC,
		h.deps.PurviewResolver, // scoper: target ∩ Purview command-пути (ADR-047 S4); тот же rbac.Holder, что REST
		h.deps.AuditWriter,
		// tidingInvalidator: тот же *herald.Service, что REST (single source of
		// truth) — после commit voyage-tx с ephemeral-notify сбрасывает TTL-снимок
		// dispatcher-а (ADR-052(g) race-fix). nil → no-op.
		h.deps.HeraldSvc,
		h.deps.VoyageMaxScope,
		h.deps.VoyageMaxBatchSize,
		h.deps.Logger,
	)
}

// callVoyageStart — keeper.voyage.start (POST /v1/voyages). RBAC-by-kind делает
// сам handler (kind виден только из тела); MCP-путь pre-check НЕ дублирует.
func (h *Handler) callVoyageStart(ctx context.Context, claims *jwt.Claims, req jsonRPCRequest, args json.RawMessage) jsonRPCResponse {
	const toolName = "keeper.voyage.start"
	hh := h.ensureVoyageHandler()
	if hh == nil {
		return h.toolError(req.ID, toolName, mcpCodeInternalError, voyageNotConfigured)
	}
	body := args
	if len(body) == 0 {
		body = []byte("{}")
	}
	// Source=MCP проставляется в ctx (parity callIncarnationRun); REST-handler
	// Voyage.Create читает его через middleware.ScenarioInvocationSource.
	ctx = middleware.WithScenarioInvocationSource(ctx, audit.SourceMCP)
	rec, status := h.invokeVoyageHandler(ctx, claims, hh.Create, "/v1/voyages", http.MethodPost, "", body)
	if status >= 400 {
		return h.toolFromVoyageProblem(req.ID, toolName, rec, status)
	}
	var parsed any
	_ = json.Unmarshal(rec.Body.Bytes(), &parsed)
	return h.toolResult(req.ID, parsed)
}

// callVoyageList — keeper.voyage.list (GET /v1/voyages). RBAC incarnation.history
// (parity REST router-gate). MCP-путь pre-check-ит её сам (router-middleware
// обходится).
func (h *Handler) callVoyageList(ctx context.Context, claims *jwt.Claims, req jsonRPCRequest, args json.RawMessage) jsonRPCResponse {
	const toolName = "keeper.voyage.list"
	hh := h.ensureVoyageHandler()
	if hh == nil {
		return h.toolError(req.ID, toolName, mcpCodeInternalError, voyageNotConfigured)
	}
	if err := h.deps.RBAC.Check(claims.Subject, "incarnation", "history", nil); err != nil {
		return h.toolError(req.ID, toolName, mcpCodeForbidden,
			"operator lacks required permission incarnation.history")
	}
	query := voyageQueryFromArgs(args)
	url := "/v1/voyages"
	if query != "" {
		url = url + "?" + query
	}
	rec, status := h.invokeVoyageHandler(ctx, claims, hh.List, url, http.MethodGet, "", nil)
	if status >= 400 {
		return h.toolFromVoyageProblem(req.ID, toolName, rec, status)
	}
	var parsed any
	_ = json.Unmarshal(rec.Body.Bytes(), &parsed)
	return h.toolResult(req.ID, parsed)
}

// callVoyageGet — keeper.voyage.get (GET /v1/voyages/{id}). RBAC incarnation.history.
func (h *Handler) callVoyageGet(ctx context.Context, claims *jwt.Claims, req jsonRPCRequest, args json.RawMessage) jsonRPCResponse {
	const toolName = "keeper.voyage.get"
	hh := h.ensureVoyageHandler()
	if hh == nil {
		return h.toolError(req.ID, toolName, mcpCodeInternalError, voyageNotConfigured)
	}
	if err := h.deps.RBAC.Check(claims.Subject, "incarnation", "history", nil); err != nil {
		return h.toolError(req.ID, toolName, mcpCodeForbidden,
			"operator lacks required permission incarnation.history")
	}
	id, perr := voyageIDFromArgs(args)
	if perr != "" {
		return h.toolError(req.ID, toolName, mcpCodeValidationFailed, perr)
	}
	rec, status := h.invokeVoyageHandler(ctx, claims, hh.Get, "/v1/voyages/"+id, http.MethodGet, id, nil)
	if status >= 400 {
		return h.toolFromVoyageProblem(req.ID, toolName, rec, status)
	}
	var parsed any
	_ = json.Unmarshal(rec.Body.Bytes(), &parsed)
	return h.toolResult(req.ID, parsed)
}

// callVoyageCancel — keeper.voyage.cancel (DELETE /v1/voyages/{id}). RBAC-by-kind
// делает сам handler (kind из загруженной строки); MCP-путь pre-check НЕ дублирует.
func (h *Handler) callVoyageCancel(ctx context.Context, claims *jwt.Claims, req jsonRPCRequest, args json.RawMessage) jsonRPCResponse {
	const toolName = "keeper.voyage.cancel"
	hh := h.ensureVoyageHandler()
	if hh == nil {
		return h.toolError(req.ID, toolName, mcpCodeInternalError, voyageNotConfigured)
	}
	id, perr := voyageIDFromArgs(args)
	if perr != "" {
		return h.toolError(req.ID, toolName, mcpCodeValidationFailed, perr)
	}
	// Source=MCP в ctx — REST-handler Voyage.Cancel читает его при emitCancelled.
	ctx = middleware.WithScenarioInvocationSource(ctx, audit.SourceMCP)
	rec, status := h.invokeVoyageHandler(ctx, claims, hh.Cancel, "/v1/voyages/"+id, http.MethodDelete, id, nil)
	if status >= 400 {
		return h.toolFromVoyageProblem(req.ID, toolName, rec, status)
	}
	var parsed any
	_ = json.Unmarshal(rec.Body.Bytes(), &parsed)
	return h.toolResult(req.ID, parsed)
}

// --- helpers ---

// voyageIDFromArgs декодирует {voyage_id} из MCP-arguments.
func voyageIDFromArgs(args json.RawMessage) (string, string) {
	if len(args) == 0 {
		return "", "field 'voyage_id' is required"
	}
	var a struct {
		VoyageID string `json:"voyage_id"`
	}
	if err := strictUnmarshal(args, &a); err != nil {
		return "", "invalid arguments: " + err.Error()
	}
	if a.VoyageID == "" {
		return "", "field 'voyage_id' is required"
	}
	return a.VoyageID, ""
}

// voyageQueryFromArgs строит query-string для GET /v1/voyages из MCP arguments
// (kind/status[]/offset/limit).
func voyageQueryFromArgs(args json.RawMessage) string {
	if len(args) == 0 {
		return ""
	}
	var a struct {
		Kind   string   `json:"kind,omitempty"`
		Status []string `json:"status,omitempty"`
		Offset *int     `json:"offset,omitempty"`
		Limit  *int     `json:"limit,omitempty"`
	}
	if err := json.Unmarshal(args, &a); err != nil {
		return ""
	}
	var parts []string
	if a.Kind != "" {
		parts = append(parts, "kind="+a.Kind)
	}
	for _, s := range a.Status {
		parts = append(parts, "status="+s)
	}
	if a.Offset != nil {
		parts = append(parts, "offset="+strconv.Itoa(*a.Offset))
	}
	if a.Limit != nil {
		parts = append(parts, "limit="+strconv.Itoa(*a.Limit))
	}
	return strings.Join(parts, "&")
}

// invokeVoyageHandler — общий wrapper httptest.Request + Recorder + claims-ctx +
// path-{id} (parity invokeErrandRunHandler).
func (h *Handler) invokeVoyageHandler(
	ctx context.Context, claims *jwt.Claims,
	handlerFn func(http.ResponseWriter, *http.Request),
	url, method, id string,
	body any,
) (*httptest.ResponseRecorder, int) {
	var bodyReader io.Reader
	switch b := body.(type) {
	case nil:
		bodyReader = http.NoBody
	case []byte:
		bodyReader = bytes.NewReader(b)
	case json.RawMessage:
		bodyReader = bytes.NewReader(b)
	default:
		buf, _ := json.Marshal(b)
		bodyReader = bytes.NewReader(buf)
	}
	r := httptest.NewRequest(method, url, bodyReader)
	r = r.WithContext(middleware.WithClaims(ctx, claims))
	if id != "" {
		rctx := chi.NewRouteContext()
		rctx.URLParams.Add("id", id)
		r = r.WithContext(context.WithValue(r.Context(), chi.RouteCtxKey, rctx))
	}
	rec := httptest.NewRecorder()
	handlerFn(rec, r)
	return rec, rec.Code
}

// toolFromVoyageProblem конвертирует problem+json HTTP-handler-а в MCP-tool-error
// (parity toolFromProblem; 409 voyage_*_terminal / running_cancel → not-cancellable).
func (h *Handler) toolFromVoyageProblem(id json.RawMessage, toolName string, rec *httptest.ResponseRecorder, status int) jsonRPCResponse {
	body := rec.Body.Bytes()
	var p struct {
		Detail string `json:"detail"`
	}
	_ = json.Unmarshal(body, &p)
	detail := p.Detail
	if detail == "" {
		detail = "request failed"
	}
	var code string
	switch {
	case status == http.StatusUnauthorized:
		code = mcpCodeUnauthenticated
	case status == http.StatusForbidden:
		code = mcpCodeForbidden
	case status == http.StatusNotFound:
		code = mcpCodeNotFound
	case status == http.StatusConflict:
		code = mcpCodeErrandNotCancellable
	case status == http.StatusUnprocessableEntity:
		code = mcpCodeValidationFailed
	case status >= http.StatusInternalServerError:
		code = mcpCodeInternalError
	default:
		code = mcpCodeMalformedRequest
	}
	return h.toolError(id, toolName, code, detail)
}
