package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"time"

	"github.com/souls-guild/soul-stack/keeper/internal/herald"
	"github.com/souls-guild/soul-stack/keeper/internal/jwt"
	"github.com/souls-guild/soul-stack/shared/audit"
)

// keeper.herald.* / keeper.tiding.* — parity with REST POST/GET/PUT/DELETE
// /v1/heralds* and /v1/tidings* (HeraldHandler, ADR-052, S4). A thin MCP
// wrapper over the same herald.Service as REST. Permission mapping is 1:1
// (keeper.herald.<verb> ↔ herald.<verb>, keeper.tiding.<verb> ↔ tiding.<verb>),
// selector is NoSelector (omen.* / push-provider.* pattern). Error codes
// mirror REST (mapHeraldErrorToMCP / mapTidingErrorToMCP).

// heraldNotConfigured — public-detail nil-guard for herald/tiding-tools.
// HeraldSvc is an optional HandlerDeps field: when nil, tools still dispatch
// but return internal-error.
const heraldNotConfigured = "herald registry is not configured"

// --- Herald: output projections ----------------------------------------

// heraldView — output form of a Herald (same as REST toHeraldResponse).
type heraldView struct {
	ID string `json:"id"`
	// Label — display caption (ADR-0085); absent when the row carries none, and
	// a consumer then shows `name`. NOT the derived Vault `<entity>` segment.
	Label        *string        `json:"label,omitempty"`
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
		ID:           h.ID,
		Label:        h.Label,
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
	ID string `json:"id"`
	// Label — optional display caption (ADR-0085), free text; changed afterwards
	// by keeper.herald.label-set.
	Label     *string        `json:"label"`
	Type      string         `json:"type"`
	Config    map[string]any `json:"config"`
	SecretRef *string        `json:"secret_ref"`
	// Secret — optional plaintext webhook signing token (dual-mode, ADR-064);
	// XOR with secret_ref. Channel config secrets (bot_token/webhook_url/…) are
	// also dual-mode inside config. keeper writes plaintext to Vault; plaintext
	// is never persisted.
	Secret  *string `json:"secret"`
	Enabled *bool   `json:"enabled"`
}

type heraldUpdateArgs struct {
	ID        string         `json:"id"`
	Type      string         `json:"type"`
	Config    map[string]any `json:"config"`
	SecretRef *string        `json:"secret_ref"`
	Secret    *string        `json:"secret"` // dual-mode plaintext (ADR-064), XOR secret_ref
	Enabled   *bool          `json:"enabled"`
}

type heraldByIDArgs struct {
	ID string `json:"id"`
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

// --- Herald: call methods ---------------------------------------------

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
	if a.ID == "" {
		return h.toolError(req.ID, toolName, mcpCodeValidationFailed, "field 'id' is required")
	}
	enabled := true
	if a.Enabled != nil {
		enabled = *a.Enabled
	}
	created, err := h.deps.HeraldSvc.CreateHerald(ctx, &herald.Herald{
		ID:           a.ID,
		Label:        a.Label,
		Type:         herald.HeraldType(a.Type),
		Config:       a.Config,
		SecretRef:    a.SecretRef,
		Secret:       a.Secret,
		Enabled:      enabled,
		CreatedByAID: aidArgMCP(claims.Subject),
	})
	if err != nil {
		return h.heraldErr(req.ID, toolName, err, "herald.create", a.ID)
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
	if a.ID == "" {
		return h.toolError(req.ID, toolName, mcpCodeValidationFailed, "field 'id' is required")
	}
	enabled := true
	if a.Enabled != nil {
		enabled = *a.Enabled
	}
	updated, err := h.deps.HeraldSvc.UpdateHerald(ctx, &herald.Herald{
		ID:        a.ID,
		Type:      herald.HeraldType(a.Type),
		Config:    a.Config,
		SecretRef: a.SecretRef,
		Secret:    a.Secret,
		Enabled:   enabled,
	})
	if err != nil {
		return h.heraldErr(req.ID, toolName, err, "herald.update", a.ID)
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
	var a heraldByIDArgs
	if len(args) > 0 {
		if err := strictUnmarshal(args, &a); err != nil {
			return h.toolError(req.ID, toolName, mcpCodeMalformedRequest, "invalid arguments: "+err.Error())
		}
	}
	if a.ID == "" {
		return h.toolError(req.ID, toolName, mcpCodeValidationFailed, "field 'id' is required")
	}
	if err := h.deps.HeraldSvc.DeleteHerald(ctx, a.ID); err != nil {
		return h.heraldErr(req.ID, toolName, err, "herald.delete", a.ID)
	}
	h.writeAudit(audit.EventHeraldDeleted, claims.Subject, map[string]any{"id": a.ID})
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
	var a heraldByIDArgs
	if len(args) > 0 {
		if err := strictUnmarshal(args, &a); err != nil {
			return h.toolError(req.ID, toolName, mcpCodeMalformedRequest, "invalid arguments: "+err.Error())
		}
	}
	if a.ID == "" {
		return h.toolError(req.ID, toolName, mcpCodeValidationFailed, "field 'id' is required")
	}
	hr, err := h.deps.HeraldSvc.GetHerald(ctx, a.ID)
	if err != nil {
		return h.heraldErr(req.ID, toolName, err, "herald.read", a.ID)
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

// heraldErr — shared mapper of Herald errors to an MCP response; internal-error is logged.
func (h *Handler) heraldErr(reqID json.RawMessage, toolName string, err error, op, id string) jsonRPCResponse {
	code, detail := mapHeraldErrorToMCP(err)
	if code == mcpCodeInternalError {
		h.deps.Logger.Error("mcp: "+op+" failed", slog.String("id", id), slog.Any("error", err))
	}
	return h.toolError(reqID, toolName, code, detail)
}

func heraldAuditMCP(h *herald.Herald) map[string]any {
	p := map[string]any{"id": h.ID, "label": h.Label, "type": string(h.Type), "enabled": h.Enabled}
	if url, ok := h.Config["url"].(string); ok {
		p["url"] = url
	}
	if h.SecretRef != nil {
		p["secret_ref"] = *h.SecretRef
	}
	// plaintext_ingested — marker that keeper wrote the secret (ADR-064), no plaintext.
	if h.SecretWritten {
		p["plaintext_ingested"] = true
	}
	if h.CreatedByAID != nil {
		p["created_by_aid"] = *h.CreatedByAID
	}
	return p
}

// callHeraldSetLabel — keeper.herald.label-set, the MCP mirror of
// PUT /v1/heralds/{id}/label (ADR-0085). Narrower than keeper.herald.update,
// which replaces the channel including its secret_ref.
func (h *Handler) callHeraldSetLabel(ctx context.Context, claims *jwt.Claims, req jsonRPCRequest, args json.RawMessage) jsonRPCResponse {
	return callLabelSet(h, ctx, claims, req, args, labelSetSpec[heraldView]{
		tool:          "keeper.herald.label-set",
		resource:      "herald",
		configured:    h.deps.HeraldSvc != nil,
		notConfigured: heraldNotConfigured,
		validID:       herald.ValidID,
		idPattern:     herald.IDPattern,
		set: func(ctx context.Context, id string, label *string) (heraldView, *string, error) {
			updated, previous, err := h.deps.HeraldSvc.SetHeraldLabel(ctx, id, label)
			if err != nil {
				return heraldView{}, nil, err
			}
			return toHeraldView(updated), previous, nil
		},
		isNotFound: func(err error) bool { return errors.Is(err, herald.ErrHeraldNotFound) },
		notFoundf:  func(id string) string { return "herald " + id + " not found" },
		failMsg:    "set herald label failed",
		event:      audit.EventHeraldLabelChanged,
	})
}

// --- Tiding: output projections -----------------------------------------

type tidingView struct {
	ID string `json:"id"`
	// Label — display caption (ADR-0085); absent → a consumer shows `name`.
	Label        *string  `json:"label,omitempty"`
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
		ID:           t.ID,
		Label:        t.Label,
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
	ID string `json:"id"`
	// Label — optional display caption (ADR-0085), free text; changed afterwards
	// by keeper.tiding.label-set.
	Label        *string  `json:"label"`
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
	ID           string   `json:"id"`
	Herald       string   `json:"herald"`
	EventTypes   []string `json:"event_types"`
	OnlyFailures *bool    `json:"only_failures"`
	OnlyChanges  *bool    `json:"only_changes"`
	Incarnation  *string  `json:"incarnation"`
	Cadence      *string  `json:"cadence"`
	Task         *string  `json:"task"`
	Enabled      *bool    `json:"enabled"`
}

type tidingByIDArgs struct {
	ID string `json:"id"`
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

// --- Tiding: call methods ---------------------------------------------

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
	if a.ID == "" {
		return h.toolError(req.ID, toolName, mcpCodeValidationFailed, "field 'id' is required")
	}
	created, err := h.deps.HeraldSvc.CreateTiding(ctx, &herald.Tiding{
		ID:           a.ID,
		Label:        a.Label,
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
		return h.tidingErr(req.ID, toolName, err, "tiding.create", a.ID)
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
	if a.ID == "" {
		return h.toolError(req.ID, toolName, mcpCodeValidationFailed, "field 'id' is required")
	}
	updated, err := h.deps.HeraldSvc.UpdateTiding(ctx, &herald.Tiding{
		ID:           a.ID,
		Herald:       a.Herald,
		EventTypes:   a.EventTypes,
		OnlyFailures: boolOrMCP(a.OnlyFailures, false),
		OnlyChanges:  boolOrMCP(a.OnlyChanges, false),
		Incarnation:  a.Incarnation,
		Cadence:      a.Cadence,
		// PUT/update replace: nil task = clear (omit==clear, like REST).
		Task:    a.Task,
		Enabled: boolOrMCP(a.Enabled, true),
	})
	if err != nil {
		return h.tidingErr(req.ID, toolName, err, "tiding.update", a.ID)
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
	var a tidingByIDArgs
	if len(args) > 0 {
		if err := strictUnmarshal(args, &a); err != nil {
			return h.toolError(req.ID, toolName, mcpCodeMalformedRequest, "invalid arguments: "+err.Error())
		}
	}
	if a.ID == "" {
		return h.toolError(req.ID, toolName, mcpCodeValidationFailed, "field 'id' is required")
	}
	if err := h.deps.HeraldSvc.DeleteTiding(ctx, a.ID); err != nil {
		return h.tidingErr(req.ID, toolName, err, "tiding.delete", a.ID)
	}
	h.writeAudit(audit.EventTidingDeleted, claims.Subject, map[string]any{"id": a.ID})
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
	var a tidingByIDArgs
	if len(args) > 0 {
		if err := strictUnmarshal(args, &a); err != nil {
			return h.toolError(req.ID, toolName, mcpCodeMalformedRequest, "invalid arguments: "+err.Error())
		}
	}
	if a.ID == "" {
		return h.toolError(req.ID, toolName, mcpCodeValidationFailed, "field 'id' is required")
	}
	tg, err := h.deps.HeraldSvc.GetTiding(ctx, a.ID)
	if err != nil {
		return h.tidingErr(req.ID, toolName, err, "tiding.read", a.ID)
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

// tidingErr — shared mapper of Tiding errors to an MCP response; internal-error is logged.
func (h *Handler) tidingErr(reqID json.RawMessage, toolName string, err error, op, id string) jsonRPCResponse {
	code, detail := mapTidingErrorToMCP(err)
	if code == mcpCodeInternalError {
		h.deps.Logger.Error("mcp: "+op+" failed", slog.String("id", id), slog.Any("error", err))
	}
	return h.toolError(reqID, toolName, code, detail)
}

// callTidingSetLabel — keeper.tiding.label-set, the MCP mirror of
// PUT /v1/tidings/{id}/label (ADR-0085).
func (h *Handler) callTidingSetLabel(ctx context.Context, claims *jwt.Claims, req jsonRPCRequest, args json.RawMessage) jsonRPCResponse {
	return callLabelSet(h, ctx, claims, req, args, labelSetSpec[tidingView]{
		tool:          "keeper.tiding.label-set",
		resource:      "tiding",
		configured:    h.deps.HeraldSvc != nil,
		notConfigured: heraldNotConfigured,
		validID:       herald.ValidID,
		idPattern:     herald.IDPattern,
		set: func(ctx context.Context, id string, label *string) (tidingView, *string, error) {
			updated, previous, err := h.deps.HeraldSvc.SetTidingLabel(ctx, id, label)
			if err != nil {
				return tidingView{}, nil, err
			}
			return toTidingView(updated), previous, nil
		},
		isNotFound: func(err error) bool { return errors.Is(err, herald.ErrTidingNotFound) },
		notFoundf:  func(id string) string { return "tiding " + id + " not found" },
		failMsg:    "set tiding label failed",
		event:      audit.EventTidingLabelChanged,
	})
}

func tidingAuditMCP(t *herald.Tiding) map[string]any {
	p := map[string]any{
		"id":            t.ID,
		"label":         t.Label,
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

// aidArgMCP — empty AID → nil (NULL created_by_aid). boolOrMCP — *bool with a default.
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
