package mcp

import (
	"context"
	"encoding/json"
	"log/slog"
	"time"

	"github.com/souls-guild/soul-stack/keeper/internal/jwt"
	"github.com/souls-guild/soul-stack/keeper/internal/oracle"
	"github.com/souls-guild/soul-stack/shared/audit"
)

// oracleNotConfigured — public-detail nil-guard-а oracle-tools. OracleSvc — опц.
// поле HandlerDeps (production-wire-up передаёт тот же *oracle.Service, что REST):
// при nil oracle-tools диспатчатся, но возвращают internal-error «не
// сконфигурировано» (паттерн AugurSvc/ServiceSvc).
const oracleNotConfigured = "oracle registry is not configured"

// vigilView — output-проекция Vigil-а для oracle-tools (schemaVigilView). 1:1 с
// REST vigilResponse / [oracle.Vigil].
type vigilView struct {
	Name         string          `json:"name"`
	Coven        []string        `json:"coven,omitempty"`
	SID          *string         `json:"sid,omitempty"`
	Interval     string          `json:"interval"`
	Check        string          `json:"check"`
	Params       json.RawMessage `json:"params"`
	Enabled      bool            `json:"enabled"`
	CreatedByAID *string         `json:"created_by_aid,omitempty"`
	CreatedAt    string          `json:"created_at"`
	UpdatedAt    string          `json:"updated_at"`
}

func toVigilView(v *oracle.Vigil) vigilView {
	params := v.Params
	if len(params) == 0 {
		params = json.RawMessage("{}")
	}
	return vigilView{
		Name:         v.Name,
		Coven:        v.Coven,
		SID:          v.SID,
		Interval:     v.IntervalSpec,
		Check:        v.CheckAddr,
		Params:       params,
		Enabled:      v.Enabled,
		CreatedByAID: v.CreatedByAID,
		CreatedAt:    v.CreatedAt.UTC().Format(time.RFC3339),
		UpdatedAt:    v.UpdatedAt.UTC().Format(time.RFC3339),
	}
}

// vigilCreateArgs — arguments tool-а keeper.oracle.vigil.create. subject — XOR
// coven/sid; enabled опц. (опущено → true).
type vigilCreateArgs struct {
	Name     string          `json:"name"`
	Coven    []string        `json:"coven"`
	SID      *string         `json:"sid"`
	Interval string          `json:"interval"`
	Check    string          `json:"check"`
	Params   json.RawMessage `json:"params"`
	Enabled  *bool           `json:"enabled"`
}

// callOracleVigilCreate — mutating-tool keeper.oracle.vigil.create. Транспорт
// поверх [oracle.Service.CreateVigil]: вся валидация (name / interval / check /
// XOR-субъект) — в Service; tool маппит sentinel-ы в MCP-коды и пишет audit
// vigil.created.
//
// RBAC — vigil.create без селектора (rbac.md §Oracle: NoSelector).
func (h *Handler) callOracleVigilCreate(ctx context.Context, claims *jwt.Claims, req jsonRPCRequest, args json.RawMessage) jsonRPCResponse {
	const toolName = "keeper.oracle.vigil.create"

	if h.deps.OracleSvc == nil {
		return h.toolError(req.ID, toolName, mcpCodeInternalError, oracleNotConfigured)
	}

	// RBAC ДО unmarshal/валидации (least-disclosure): неавторизованный оператор
	// не получает validation-feedback по телу. Контекст nil — право не зависит
	// от тела запроса.
	if err := h.deps.RBAC.Check(claims.Subject, "vigil", "create", nil); err != nil {
		return h.toolError(req.ID, toolName, mcpCodeForbidden,
			"operator lacks required permission vigil.create")
	}

	var a vigilCreateArgs
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

	callerAID := claims.Subject
	v, err := h.deps.OracleSvc.CreateVigil(ctx, oracle.CreateVigilInput{
		Name:      a.Name,
		Coven:     a.Coven,
		SID:       a.SID,
		Interval:  a.Interval,
		Check:     a.Check,
		Params:    a.Params,
		Enabled:   enabled,
		CallerAID: &callerAID,
	})
	if err != nil {
		code, detail := mapOracleErrorToMCP(err)
		if code == mcpCodeInternalError {
			h.deps.Logger.Error("mcp: oracle.vigil.create failed",
				slog.String("name", a.Name), slog.String("by_aid", callerAID), slog.Any("error", err))
		}
		return h.toolError(req.ID, toolName, code, detail)
	}

	// Audit — параллельно REST-handler-у: payload {name, check, interval,
	// subject, created_by_aid}. params в payload НЕ кладётся.
	h.writeAudit(audit.EventVigilCreated, callerAID, map[string]any{
		"name":           v.Name,
		"check":          v.CheckAddr,
		"interval":       v.IntervalSpec,
		"subject":        vigilSubjectView(v),
		"created_by_aid": callerAID,
	})

	return h.toolResult(req.ID, toVigilView(v))
}

// vigilListOutput — output keeper.oracle.vigil.list: реестр Vigil-ов под
// `vigils` (паритет REST GET /v1/vigils items).
type vigilListOutput struct {
	Vigils []vigilView `json:"vigils"`
	Total  int         `json:"total"`
}

// vigilListArgs — arguments keeper.oracle.vigil.list (опц. offset/limit).
type vigilListArgs struct {
	Offset *int `json:"offset"`
	Limit  *int `json:"limit"`
}

// callOracleVigilList — read-tool keeper.oracle.vigil.list (read-only, не
// аудируется). RBAC — vigil.list без селектора.
func (h *Handler) callOracleVigilList(ctx context.Context, claims *jwt.Claims, req jsonRPCRequest, args json.RawMessage) jsonRPCResponse {
	const toolName = "keeper.oracle.vigil.list"

	if h.deps.OracleSvc == nil {
		return h.toolError(req.ID, toolName, mcpCodeInternalError, oracleNotConfigured)
	}

	if err := h.deps.RBAC.Check(claims.Subject, "vigil", "list", nil); err != nil {
		return h.toolError(req.ID, toolName, mcpCodeForbidden,
			"operator lacks required permission vigil.list")
	}

	var a vigilListArgs
	if len(args) > 0 {
		if err := strictUnmarshal(args, &a); err != nil {
			return h.toolError(req.ID, toolName, mcpCodeMalformedRequest, "invalid arguments: "+err.Error())
		}
	}
	offset, limit := 0, listDefaultLimit
	if a.Offset != nil {
		offset = *a.Offset
	}
	if a.Limit != nil {
		limit = *a.Limit
	}
	// Upper-limit на limit (security-fix паритет omen.list): неограниченный
	// limit — DoS-вектор (один запрос материализует весь реестр).
	if offset < 0 || limit < 1 || limit > listMaxLimit {
		return h.toolError(req.ID, toolName, mcpCodeValidationFailed,
			"offset must be >= 0 and limit must be between 1 and 1000")
	}

	vigils, total, err := h.deps.OracleSvc.ListVigils(ctx, offset, limit)
	if err != nil {
		h.deps.Logger.Error("mcp: oracle.vigil.list failed",
			slog.String("by_aid", claims.Subject), slog.Any("error", err))
		return h.toolError(req.ID, toolName, mcpCodeInternalError, "internal error")
	}

	out := vigilListOutput{Vigils: make([]vigilView, 0, len(vigils)), Total: total}
	for _, v := range vigils {
		out.Vigils = append(out.Vigils, toVigilView(v))
	}
	return h.toolResult(req.ID, out)
}

// vigilDeleteArgs — arguments keeper.oracle.vigil.delete.
type vigilDeleteArgs struct {
	Name string `json:"name"`
}

// callOracleVigilDelete — mutating-tool keeper.oracle.vigil.delete. RBAC —
// vigil.delete без селектора.
func (h *Handler) callOracleVigilDelete(ctx context.Context, claims *jwt.Claims, req jsonRPCRequest, args json.RawMessage) jsonRPCResponse {
	const toolName = "keeper.oracle.vigil.delete"

	if h.deps.OracleSvc == nil {
		return h.toolError(req.ID, toolName, mcpCodeInternalError, oracleNotConfigured)
	}

	if err := h.deps.RBAC.Check(claims.Subject, "vigil", "delete", nil); err != nil {
		return h.toolError(req.ID, toolName, mcpCodeForbidden,
			"operator lacks required permission vigil.delete")
	}

	var a vigilDeleteArgs
	if len(args) > 0 {
		if err := strictUnmarshal(args, &a); err != nil {
			return h.toolError(req.ID, toolName, mcpCodeMalformedRequest, "invalid arguments: "+err.Error())
		}
	}
	if a.Name == "" {
		return h.toolError(req.ID, toolName, mcpCodeValidationFailed, "field 'name' is required")
	}

	if err := h.deps.OracleSvc.DeleteVigil(ctx, a.Name); err != nil {
		code, detail := mapOracleErrorToMCP(err)
		if code == mcpCodeInternalError {
			h.deps.Logger.Error("mcp: oracle.vigil.delete failed",
				slog.String("name", a.Name), slog.String("by_aid", claims.Subject), slog.Any("error", err))
		}
		return h.toolError(req.ID, toolName, code, detail)
	}

	h.writeAudit(audit.EventVigilDeleted, claims.Subject, map[string]any{
		"name": a.Name,
	})

	// REST возвращает 204 No Content; MCP-эквивалент — пустой output-объект.
	return h.toolResult(req.ID, struct{}{})
}

// vigilSubjectView — человекочитаемая форма субъекта Vigil-а для audit-payload
// (`coven=<v1,v2>` / `sid=<v>`). XOR гарантирован валидацией.
func vigilSubjectView(v *oracle.Vigil) string { return oracleSubjectLabel(v.Coven, v.SID) }
