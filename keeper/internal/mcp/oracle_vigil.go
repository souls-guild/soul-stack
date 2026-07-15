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

// oracleNotConfigured — public-detail of oracle-tools' nil-guard. OracleSvc is
// an optional HandlerDeps field (production wire-up passes the same
// *oracle.Service as REST): when nil, oracle-tools dispatch but return
// internal-error "not configured" (same pattern as AugurSvc/ServiceSvc).
const oracleNotConfigured = "oracle registry is not configured"

// vigilView — output projection of a Vigil for oracle-tools (schemaVigilView).
// 1:1 with REST vigilResponse / [oracle.Vigil].
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

// vigilCreateArgs — arguments for the keeper.oracle.vigil.create tool.
// subject is XOR coven/sid; enabled is optional (omitted → true).
type vigilCreateArgs struct {
	Name     string          `json:"name"`
	Coven    []string        `json:"coven"`
	SID      *string         `json:"sid"`
	Interval string          `json:"interval"`
	Check    string          `json:"check"`
	Params   json.RawMessage `json:"params"`
	Enabled  *bool           `json:"enabled"`
}

// callOracleVigilCreate — mutating-tool keeper.oracle.vigil.create. A
// transport layer over [oracle.Service.CreateVigil]: all validation (name /
// interval / check / XOR-subject) lives in Service; the tool maps sentinels
// to MCP codes and writes the vigil.created audit event.
//
// RBAC — vigil.create without a selector (rbac.md §Oracle: NoSelector).
func (h *Handler) callOracleVigilCreate(ctx context.Context, claims *jwt.Claims, req jsonRPCRequest, args json.RawMessage) jsonRPCResponse {
	const toolName = "keeper.oracle.vigil.create"

	if h.deps.OracleSvc == nil {
		return h.toolError(req.ID, toolName, mcpCodeInternalError, oracleNotConfigured)
	}

	// RBAC BEFORE unmarshal/validation (least-disclosure): an unauthorized
	// operator gets no validation feedback about the body. Context is nil —
	// the permission doesn't depend on the request body.
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

	// Audit — mirrors the REST handler: payload {name, check, interval,
	// subject, created_by_aid}. params is NOT included in the payload.
	h.writeAudit(audit.EventVigilCreated, callerAID, map[string]any{
		"name":           v.Name,
		"check":          v.CheckAddr,
		"interval":       v.IntervalSpec,
		"subject":        vigilSubjectView(v),
		"created_by_aid": callerAID,
	})

	return h.toolResult(req.ID, toVigilView(v))
}

// vigilListOutput — output of keeper.oracle.vigil.list: registry of Vigils
// under `vigils` (parity with REST GET /v1/vigils items).
type vigilListOutput struct {
	Vigils []vigilView `json:"vigils"`
	Total  int         `json:"total"`
}

// vigilListArgs — arguments for keeper.oracle.vigil.list (optional offset/limit).
type vigilListArgs struct {
	Offset *int `json:"offset"`
	Limit  *int `json:"limit"`
}

// callOracleVigilList — read-tool keeper.oracle.vigil.list (read-only, not
// audited). RBAC — vigil.list without a selector.
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
	// Upper limit on limit (security-fix parity with omen.list): an unbounded
	// limit is a DoS vector (one request would materialize the whole registry).
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
// vigil.delete without a selector.
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

	// REST returns 204 No Content; the MCP equivalent is an empty output object.
	return h.toolResult(req.ID, struct{}{})
}

// vigilSubjectView — human-readable form of a Vigil's subject for the audit
// payload (`coven=<v1,v2>` / `sid=<v>`). XOR is guaranteed by validation.
func vigilSubjectView(v *oracle.Vigil) string { return oracleSubjectLabel(v.Coven, v.SID) }
