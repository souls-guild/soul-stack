package mcp

import (
	"context"
	"encoding/json"
	"log/slog"
	"time"

	"github.com/souls-guild/soul-stack/keeper/internal/herald"
	"github.com/souls-guild/soul-stack/keeper/internal/jwt"
	"github.com/souls-guild/soul-stack/shared/audit"
)

// keeper.herald.* / keeper.tiding.* — паритет REST POST/GET/PUT/DELETE
// /v1/heralds* и /v1/tidings* (HeraldHandler, ADR-052, S4). Тонкая MCP-обёртка
// над тем же herald.Service, что REST. Permission-маппинг 1:1
// (keeper.herald.<verb> ↔ herald.<verb>, keeper.tiding.<verb> ↔ tiding.<verb>),
// селектор — NoSelector (паттерн omen.* / push-provider.*). Коды ошибок зеркалят
// REST (mapHeraldErrorToMCP / mapTidingErrorToMCP).

// heraldNotConfigured — public-detail nil-guard-а herald/tiding-tools. HeraldSvc —
// опц. поле HandlerDeps: при nil tools диспатчатся, но возвращают internal-error.
const heraldNotConfigured = "herald registry is not configured"

// --- Herald: output-проекции ----------------------------------------

// heraldView — output-форма Herald-а (та же, что REST toHeraldResponse).
type heraldView struct {
	Name         string         `json:"name"`
	Type         string         `json:"type"`
	Config       map[string]any `json:"config"`
	SecretRef    *string        `json:"secret_ref,omitempty"`
	Enabled      bool           `json:"enabled"`
	CreatedAt    string         `json:"created_at"`
	UpdatedAt    string         `json:"updated_at"`
	CreatedByAID *string        `json:"created_by_aid,omitempty"`
}

func toHeraldView(h *herald.Herald) heraldView {
	config := h.Config
	if config == nil {
		config = map[string]any{}
	}
	return heraldView{
		Name:         h.Name,
		Type:         string(h.Type),
		Config:       config,
		SecretRef:    h.SecretRef,
		Enabled:      h.Enabled,
		CreatedAt:    h.CreatedAt.UTC().Format(time.RFC3339),
		UpdatedAt:    h.UpdatedAt.UTC().Format(time.RFC3339),
		CreatedByAID: h.CreatedByAID,
	}
}

// --- Herald: args ----------------------------------------------------

type heraldCreateArgs struct {
	Name      string         `json:"name"`
	Type      string         `json:"type"`
	Config    map[string]any `json:"config"`
	SecretRef *string        `json:"secret_ref"`
	Enabled   *bool          `json:"enabled"`
}

type heraldUpdateArgs struct {
	Name      string         `json:"name"`
	Type      string         `json:"type"`
	Config    map[string]any `json:"config"`
	SecretRef *string        `json:"secret_ref"`
	Enabled   *bool          `json:"enabled"`
}

type heraldByNameArgs struct {
	Name string `json:"name"`
}

type heraldListArgs struct {
	Offset int `json:"offset"`
	Limit  int `json:"limit"`
}

type heraldListOut struct {
	Items  []heraldView `json:"items"`
	Offset int          `json:"offset"`
	Limit  int          `json:"limit"`
	Total  int          `json:"total"`
}

// --- Herald: call-методы ---------------------------------------------

func (h *Handler) callHeraldCreate(ctx context.Context, claims *jwt.Claims, req jsonRPCRequest, args json.RawMessage) jsonRPCResponse {
	const toolName = "keeper.herald.create"
	if h.deps.HeraldSvc == nil {
		return h.toolError(req.ID, toolName, mcpCodeInternalError, heraldNotConfigured)
	}
	if err := h.deps.RBAC.Check(claims.Subject, "herald", "create", nil); err != nil {
		return h.toolError(req.ID, toolName, mcpCodeForbidden, "operator lacks required permission herald.create")
	}
	var a heraldCreateArgs
	if len(args) > 0 {
		if err := strictUnmarshal(args, &a); err != nil {
			return h.toolError(req.ID, toolName, mcpCodeMalformedRequest, "invalid arguments: "+err.Error())
		}
	}
	if a.Name == "" {
		return h.toolError(req.ID, toolName, mcpCodeValidationFailed, "field 'name' is required")
	}
	enabled := true
	if a.Enabled != nil {
		enabled = *a.Enabled
	}
	created, err := h.deps.HeraldSvc.CreateHerald(ctx, &herald.Herald{
		Name:         a.Name,
		Type:         herald.HeraldType(a.Type),
		Config:       a.Config,
		SecretRef:    a.SecretRef,
		Enabled:      enabled,
		CreatedByAID: aidArgMCP(claims.Subject),
	})
	if err != nil {
		return h.heraldErr(req.ID, toolName, err, "herald.create", a.Name)
	}
	h.writeAudit(audit.EventHeraldCreated, claims.Subject, heraldAuditMCP(created))
	return h.toolResult(req.ID, toHeraldView(created))
}

func (h *Handler) callHeraldUpdate(ctx context.Context, claims *jwt.Claims, req jsonRPCRequest, args json.RawMessage) jsonRPCResponse {
	const toolName = "keeper.herald.update"
	if h.deps.HeraldSvc == nil {
		return h.toolError(req.ID, toolName, mcpCodeInternalError, heraldNotConfigured)
	}
	if err := h.deps.RBAC.Check(claims.Subject, "herald", "update", nil); err != nil {
		return h.toolError(req.ID, toolName, mcpCodeForbidden, "operator lacks required permission herald.update")
	}
	var a heraldUpdateArgs
	if len(args) > 0 {
		if err := strictUnmarshal(args, &a); err != nil {
			return h.toolError(req.ID, toolName, mcpCodeMalformedRequest, "invalid arguments: "+err.Error())
		}
	}
	if a.Name == "" {
		return h.toolError(req.ID, toolName, mcpCodeValidationFailed, "field 'name' is required")
	}
	enabled := true
	if a.Enabled != nil {
		enabled = *a.Enabled
	}
	updated, err := h.deps.HeraldSvc.UpdateHerald(ctx, &herald.Herald{
		Name:      a.Name,
		Type:      herald.HeraldType(a.Type),
		Config:    a.Config,
		SecretRef: a.SecretRef,
		Enabled:   enabled,
	})
	if err != nil {
		return h.heraldErr(req.ID, toolName, err, "herald.update", a.Name)
	}
	h.writeAudit(audit.EventHeraldUpdated, claims.Subject, heraldAuditMCP(updated))
	return h.toolResult(req.ID, toHeraldView(updated))
}

func (h *Handler) callHeraldDelete(ctx context.Context, claims *jwt.Claims, req jsonRPCRequest, args json.RawMessage) jsonRPCResponse {
	const toolName = "keeper.herald.delete"
	if h.deps.HeraldSvc == nil {
		return h.toolError(req.ID, toolName, mcpCodeInternalError, heraldNotConfigured)
	}
	if err := h.deps.RBAC.Check(claims.Subject, "herald", "delete", nil); err != nil {
		return h.toolError(req.ID, toolName, mcpCodeForbidden, "operator lacks required permission herald.delete")
	}
	var a heraldByNameArgs
	if len(args) > 0 {
		if err := strictUnmarshal(args, &a); err != nil {
			return h.toolError(req.ID, toolName, mcpCodeMalformedRequest, "invalid arguments: "+err.Error())
		}
	}
	if a.Name == "" {
		return h.toolError(req.ID, toolName, mcpCodeValidationFailed, "field 'name' is required")
	}
	if err := h.deps.HeraldSvc.DeleteHerald(ctx, a.Name); err != nil {
		return h.heraldErr(req.ID, toolName, err, "herald.delete", a.Name)
	}
	h.writeAudit(audit.EventHeraldDeleted, claims.Subject, map[string]any{"name": a.Name})
	return h.toolResult(req.ID, struct{}{})
}

func (h *Handler) callHeraldRead(ctx context.Context, claims *jwt.Claims, req jsonRPCRequest, args json.RawMessage) jsonRPCResponse {
	const toolName = "keeper.herald.read"
	if h.deps.HeraldSvc == nil {
		return h.toolError(req.ID, toolName, mcpCodeInternalError, heraldNotConfigured)
	}
	if err := h.deps.RBAC.Check(claims.Subject, "herald", "read", nil); err != nil {
		return h.toolError(req.ID, toolName, mcpCodeForbidden, "operator lacks required permission herald.read")
	}
	var a heraldByNameArgs
	if len(args) > 0 {
		if err := strictUnmarshal(args, &a); err != nil {
			return h.toolError(req.ID, toolName, mcpCodeMalformedRequest, "invalid arguments: "+err.Error())
		}
	}
	if a.Name == "" {
		return h.toolError(req.ID, toolName, mcpCodeValidationFailed, "field 'name' is required")
	}
	hr, err := h.deps.HeraldSvc.GetHerald(ctx, a.Name)
	if err != nil {
		return h.heraldErr(req.ID, toolName, err, "herald.read", a.Name)
	}
	return h.toolResult(req.ID, toHeraldView(hr))
}

func (h *Handler) callHeraldList(ctx context.Context, claims *jwt.Claims, req jsonRPCRequest, args json.RawMessage) jsonRPCResponse {
	const toolName = "keeper.herald.list"
	if h.deps.HeraldSvc == nil {
		return h.toolError(req.ID, toolName, mcpCodeInternalError, heraldNotConfigured)
	}
	if err := h.deps.RBAC.Check(claims.Subject, "herald", "list", nil); err != nil {
		return h.toolError(req.ID, toolName, mcpCodeForbidden, "operator lacks required permission herald.list")
	}
	var a heraldListArgs
	if len(args) > 0 {
		if err := strictUnmarshal(args, &a); err != nil {
			return h.toolError(req.ID, toolName, mcpCodeMalformedRequest, "invalid arguments: "+err.Error())
		}
	}
	if a.Limit <= 0 {
		a.Limit = 100
	}
	items, total, err := h.deps.HeraldSvc.ListHeralds(ctx, a.Offset, a.Limit)
	if err != nil {
		h.deps.Logger.Error("mcp: herald.list failed", slog.Any("error", err))
		return h.toolError(req.ID, toolName, mcpCodeInternalError, "list heralds failed")
	}
	out := make([]heraldView, 0, len(items))
	for _, hr := range items {
		out = append(out, toHeraldView(hr))
	}
	return h.toolResult(req.ID, heraldListOut{Items: out, Offset: a.Offset, Limit: a.Limit, Total: total})
}

// heraldErr — общий маппер Herald-ошибок в MCP-ответ; internal-error логируется.
func (h *Handler) heraldErr(id json.RawMessage, toolName string, err error, op, name string) jsonRPCResponse {
	code, detail := mapHeraldErrorToMCP(err)
	if code == mcpCodeInternalError {
		h.deps.Logger.Error("mcp: "+op+" failed", slog.String("name", name), slog.Any("error", err))
	}
	return h.toolError(id, toolName, code, detail)
}

func heraldAuditMCP(h *herald.Herald) map[string]any {
	p := map[string]any{"name": h.Name, "type": string(h.Type), "enabled": h.Enabled}
	if url, ok := h.Config["url"].(string); ok {
		p["url"] = url
	}
	if h.SecretRef != nil {
		p["secret_ref"] = *h.SecretRef
	}
	if h.CreatedByAID != nil {
		p["created_by_aid"] = *h.CreatedByAID
	}
	return p
}

// --- Tiding: output-проекции -----------------------------------------

type tidingView struct {
	Name         string   `json:"name"`
	Herald       string   `json:"herald"`
	EventTypes   []string `json:"event_types"`
	OnlyFailures bool     `json:"only_failures"`
	OnlyChanges  bool     `json:"only_changes"`
	Incarnation  *string  `json:"incarnation,omitempty"`
	Cadence      *string  `json:"cadence,omitempty"`
	Task         *string  `json:"task,omitempty"`
	Enabled      bool     `json:"enabled"`
	CreatedAt    string   `json:"created_at"`
	UpdatedAt    string   `json:"updated_at"`
	CreatedByAID *string  `json:"created_by_aid,omitempty"`
}

func toTidingView(t *herald.Tiding) tidingView {
	eventTypes := t.EventTypes
	if eventTypes == nil {
		eventTypes = []string{}
	}
	return tidingView{
		Name:         t.Name,
		Herald:       t.Herald,
		EventTypes:   eventTypes,
		OnlyFailures: t.OnlyFailures,
		OnlyChanges:  t.OnlyChanges,
		Incarnation:  t.Incarnation,
		Cadence:      t.Cadence,
		Task:         t.Task,
		Enabled:      t.Enabled,
		CreatedAt:    t.CreatedAt.UTC().Format(time.RFC3339),
		UpdatedAt:    t.UpdatedAt.UTC().Format(time.RFC3339),
		CreatedByAID: t.CreatedByAID,
	}
}

// --- Tiding: args ----------------------------------------------------

type tidingCreateArgs struct {
	Name         string   `json:"name"`
	Herald       string   `json:"herald"`
	EventTypes   []string `json:"event_types"`
	OnlyFailures *bool    `json:"only_failures"`
	OnlyChanges  *bool    `json:"only_changes"`
	Incarnation  *string  `json:"incarnation"`
	Cadence      *string  `json:"cadence"`
	Task         *string  `json:"task"`
	Enabled      *bool    `json:"enabled"`
}

type tidingUpdateArgs struct {
	Name         string   `json:"name"`
	Herald       string   `json:"herald"`
	EventTypes   []string `json:"event_types"`
	OnlyFailures *bool    `json:"only_failures"`
	OnlyChanges  *bool    `json:"only_changes"`
	Incarnation  *string  `json:"incarnation"`
	Cadence      *string  `json:"cadence"`
	Task         *string  `json:"task"`
	Enabled      *bool    `json:"enabled"`
}

type tidingByNameArgs struct {
	Name string `json:"name"`
}

type tidingListArgs struct {
	IncludeEphemeral bool `json:"include_ephemeral"`
	Offset           int  `json:"offset"`
	Limit            int  `json:"limit"`
}

type tidingListOut struct {
	Items  []tidingView `json:"items"`
	Offset int          `json:"offset"`
	Limit  int          `json:"limit"`
	Total  int          `json:"total"`
}

// --- Tiding: call-методы ---------------------------------------------

func (h *Handler) callTidingCreate(ctx context.Context, claims *jwt.Claims, req jsonRPCRequest, args json.RawMessage) jsonRPCResponse {
	const toolName = "keeper.tiding.create"
	if h.deps.HeraldSvc == nil {
		return h.toolError(req.ID, toolName, mcpCodeInternalError, heraldNotConfigured)
	}
	if err := h.deps.RBAC.Check(claims.Subject, "tiding", "create", nil); err != nil {
		return h.toolError(req.ID, toolName, mcpCodeForbidden, "operator lacks required permission tiding.create")
	}
	var a tidingCreateArgs
	if len(args) > 0 {
		if err := strictUnmarshal(args, &a); err != nil {
			return h.toolError(req.ID, toolName, mcpCodeMalformedRequest, "invalid arguments: "+err.Error())
		}
	}
	if a.Name == "" {
		return h.toolError(req.ID, toolName, mcpCodeValidationFailed, "field 'name' is required")
	}
	created, err := h.deps.HeraldSvc.CreateTiding(ctx, &herald.Tiding{
		Name:         a.Name,
		Herald:       a.Herald,
		EventTypes:   a.EventTypes,
		OnlyFailures: boolOrMCP(a.OnlyFailures, false),
		OnlyChanges:  boolOrMCP(a.OnlyChanges, false),
		Incarnation:  a.Incarnation,
		Cadence:      a.Cadence,
		Task:         a.Task,
		Enabled:      boolOrMCP(a.Enabled, true),
		CreatedByAID: aidArgMCP(claims.Subject),
	})
	if err != nil {
		return h.tidingErr(req.ID, toolName, err, "tiding.create", a.Name)
	}
	h.writeAudit(audit.EventTidingCreated, claims.Subject, tidingAuditMCP(created))
	return h.toolResult(req.ID, toTidingView(created))
}

func (h *Handler) callTidingUpdate(ctx context.Context, claims *jwt.Claims, req jsonRPCRequest, args json.RawMessage) jsonRPCResponse {
	const toolName = "keeper.tiding.update"
	if h.deps.HeraldSvc == nil {
		return h.toolError(req.ID, toolName, mcpCodeInternalError, heraldNotConfigured)
	}
	if err := h.deps.RBAC.Check(claims.Subject, "tiding", "update", nil); err != nil {
		return h.toolError(req.ID, toolName, mcpCodeForbidden, "operator lacks required permission tiding.update")
	}
	var a tidingUpdateArgs
	if len(args) > 0 {
		if err := strictUnmarshal(args, &a); err != nil {
			return h.toolError(req.ID, toolName, mcpCodeMalformedRequest, "invalid arguments: "+err.Error())
		}
	}
	if a.Name == "" {
		return h.toolError(req.ID, toolName, mcpCodeValidationFailed, "field 'name' is required")
	}
	updated, err := h.deps.HeraldSvc.UpdateTiding(ctx, &herald.Tiding{
		Name:         a.Name,
		Herald:       a.Herald,
		EventTypes:   a.EventTypes,
		OnlyFailures: boolOrMCP(a.OnlyFailures, false),
		OnlyChanges:  boolOrMCP(a.OnlyChanges, false),
		Incarnation:  a.Incarnation,
		Cadence:      a.Cadence,
		// PUT/update replace: nil task = очистка (omit==clear, как REST).
		Task:    a.Task,
		Enabled: boolOrMCP(a.Enabled, true),
	})
	if err != nil {
		return h.tidingErr(req.ID, toolName, err, "tiding.update", a.Name)
	}
	h.writeAudit(audit.EventTidingUpdated, claims.Subject, tidingAuditMCP(updated))
	return h.toolResult(req.ID, toTidingView(updated))
}

func (h *Handler) callTidingDelete(ctx context.Context, claims *jwt.Claims, req jsonRPCRequest, args json.RawMessage) jsonRPCResponse {
	const toolName = "keeper.tiding.delete"
	if h.deps.HeraldSvc == nil {
		return h.toolError(req.ID, toolName, mcpCodeInternalError, heraldNotConfigured)
	}
	if err := h.deps.RBAC.Check(claims.Subject, "tiding", "delete", nil); err != nil {
		return h.toolError(req.ID, toolName, mcpCodeForbidden, "operator lacks required permission tiding.delete")
	}
	var a tidingByNameArgs
	if len(args) > 0 {
		if err := strictUnmarshal(args, &a); err != nil {
			return h.toolError(req.ID, toolName, mcpCodeMalformedRequest, "invalid arguments: "+err.Error())
		}
	}
	if a.Name == "" {
		return h.toolError(req.ID, toolName, mcpCodeValidationFailed, "field 'name' is required")
	}
	if err := h.deps.HeraldSvc.DeleteTiding(ctx, a.Name); err != nil {
		return h.tidingErr(req.ID, toolName, err, "tiding.delete", a.Name)
	}
	h.writeAudit(audit.EventTidingDeleted, claims.Subject, map[string]any{"name": a.Name})
	return h.toolResult(req.ID, struct{}{})
}

func (h *Handler) callTidingRead(ctx context.Context, claims *jwt.Claims, req jsonRPCRequest, args json.RawMessage) jsonRPCResponse {
	const toolName = "keeper.tiding.read"
	if h.deps.HeraldSvc == nil {
		return h.toolError(req.ID, toolName, mcpCodeInternalError, heraldNotConfigured)
	}
	if err := h.deps.RBAC.Check(claims.Subject, "tiding", "read", nil); err != nil {
		return h.toolError(req.ID, toolName, mcpCodeForbidden, "operator lacks required permission tiding.read")
	}
	var a tidingByNameArgs
	if len(args) > 0 {
		if err := strictUnmarshal(args, &a); err != nil {
			return h.toolError(req.ID, toolName, mcpCodeMalformedRequest, "invalid arguments: "+err.Error())
		}
	}
	if a.Name == "" {
		return h.toolError(req.ID, toolName, mcpCodeValidationFailed, "field 'name' is required")
	}
	tg, err := h.deps.HeraldSvc.GetTiding(ctx, a.Name)
	if err != nil {
		return h.tidingErr(req.ID, toolName, err, "tiding.read", a.Name)
	}
	return h.toolResult(req.ID, toTidingView(tg))
}

func (h *Handler) callTidingList(ctx context.Context, claims *jwt.Claims, req jsonRPCRequest, args json.RawMessage) jsonRPCResponse {
	const toolName = "keeper.tiding.list"
	if h.deps.HeraldSvc == nil {
		return h.toolError(req.ID, toolName, mcpCodeInternalError, heraldNotConfigured)
	}
	if err := h.deps.RBAC.Check(claims.Subject, "tiding", "list", nil); err != nil {
		return h.toolError(req.ID, toolName, mcpCodeForbidden, "operator lacks required permission tiding.list")
	}
	var a tidingListArgs
	if len(args) > 0 {
		if err := strictUnmarshal(args, &a); err != nil {
			return h.toolError(req.ID, toolName, mcpCodeMalformedRequest, "invalid arguments: "+err.Error())
		}
	}
	if a.Limit <= 0 {
		a.Limit = 100
	}
	items, total, err := h.deps.HeraldSvc.ListTidings(ctx, a.IncludeEphemeral, a.Offset, a.Limit)
	if err != nil {
		h.deps.Logger.Error("mcp: tiding.list failed", slog.Any("error", err))
		return h.toolError(req.ID, toolName, mcpCodeInternalError, "list tidings failed")
	}
	out := make([]tidingView, 0, len(items))
	for _, tg := range items {
		out = append(out, toTidingView(tg))
	}
	return h.toolResult(req.ID, tidingListOut{Items: out, Offset: a.Offset, Limit: a.Limit, Total: total})
}

// tidingErr — общий маппер Tiding-ошибок в MCP-ответ; internal-error логируется.
func (h *Handler) tidingErr(id json.RawMessage, toolName string, err error, op, name string) jsonRPCResponse {
	code, detail := mapTidingErrorToMCP(err)
	if code == mcpCodeInternalError {
		h.deps.Logger.Error("mcp: "+op+" failed", slog.String("name", name), slog.Any("error", err))
	}
	return h.toolError(id, toolName, code, detail)
}

func tidingAuditMCP(t *herald.Tiding) map[string]any {
	p := map[string]any{
		"name":          t.Name,
		"herald":        t.Herald,
		"event_types":   t.EventTypes,
		"only_failures": t.OnlyFailures,
		"only_changes":  t.OnlyChanges,
		"enabled":       t.Enabled,
	}
	if t.Incarnation != nil {
		p["incarnation"] = *t.Incarnation
	}
	if t.Cadence != nil {
		p["cadence"] = *t.Cadence
	}
	if t.Task != nil {
		p["task"] = *t.Task
	}
	if t.CreatedByAID != nil {
		p["created_by_aid"] = *t.CreatedByAID
	}
	return p
}

// aidArgMCP — пустой AID → nil (NULL created_by_aid). boolOrMCP — *bool с дефолтом.
func aidArgMCP(aid string) *string {
	if aid == "" {
		return nil
	}
	return &aid
}

func boolOrMCP(p *bool, def bool) bool {
	if p == nil {
		return def
	}
	return *p
}
