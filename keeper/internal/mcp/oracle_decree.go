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

// decreeView — output projection of a Decree for oracle-tools (schemaDecreeView).
// 1:1 with REST decreeResponse / [oracle.Decree].
type decreeView struct {
	Name            string          `json:"name"`
	OnBeacon        string          `json:"on_beacon"`
	WhereCEL        *string         `json:"where,omitempty"`
	Coven           []string        `json:"coven,omitempty"`
	SID             *string         `json:"sid,omitempty"`
	IncarnationName string          `json:"incarnation_name"`
	ActionScenario  string          `json:"action_scenario"`
	ActionInput     json.RawMessage `json:"action_input"`
	Cooldown        string          `json:"cooldown"`
	Enabled         bool            `json:"enabled"`
	CreatedByAID    *string         `json:"created_by_aid,omitempty"`
	CreatedAt       string          `json:"created_at"`
	UpdatedAt       string          `json:"updated_at"`
}

func toDecreeView(d *oracle.Decree) decreeView {
	input := d.ActionInput
	if len(input) == 0 {
		input = json.RawMessage("{}")
	}
	return decreeView{
		Name:            d.Name,
		OnBeacon:        d.OnBeacon,
		WhereCEL:        d.WhereCEL,
		Coven:           d.SubjectCoven,
		SID:             d.SubjectSID,
		IncarnationName: d.IncarnationName,
		ActionScenario:  d.ActionScenario,
		ActionInput:     input,
		Cooldown:        d.Cooldown,
		Enabled:         d.Enabled,
		CreatedByAID:    d.CreatedByAID,
		CreatedAt:       d.CreatedAt.UTC().Format(time.RFC3339),
		UpdatedAt:       d.UpdatedAt.UTC().Format(time.RFC3339),
	}
}

// decreeCreateArgs — arguments for keeper.oracle.decree.create. subject is
// XOR coven/sid; where is an optional CEL predicate (compile-checked in
// Service); enabled is optional (omitted → true).
type decreeCreateArgs struct {
	Name            string          `json:"name"`
	OnBeacon        string          `json:"on_beacon"`
	WhereCEL        *string         `json:"where"`
	Coven           []string        `json:"coven"`
	SID             *string         `json:"sid"`
	IncarnationName string          `json:"incarnation_name"`
	ActionScenario  string          `json:"action_scenario"`
	ActionInput     json.RawMessage `json:"action_input"`
	Cooldown        string          `json:"cooldown"`
	Enabled         *bool           `json:"enabled"`
}

// callOracleDecreeCreate — mutating-tool keeper.oracle.decree.create.
// Transport over [oracle.Service.CreateDecree]: all validation (name /
// on_beacon / incarnation_name / action_scenario / XOR-subject / where-CEL
// compile-check / cooldown) lives in Service; the tool maps sentinels to
// MCP codes and writes the decree.created audit event.
//
// RBAC — decree.create has no selector (rbac.md §Oracle: NoSelector).
func (h *Handler) callOracleDecreeCreate(ctx context.Context, claims *jwt.Claims, req jsonRPCRequest, args json.RawMessage) jsonRPCResponse {
	const toolName = "keeper.oracle.decree.create"

	if h.deps.OracleSvc == nil {
		return h.toolError(req.ID, toolName, mcpCodeInternalError, oracleNotConfigured)
	}

	// RBAC BEFORE unmarshal/validation (least-disclosure): an unauthorized
	// operator gets no validation feedback about the body. nil context —
	// the permission doesn't depend on the request body.
	if err := h.deps.RBAC.Check(claims.Subject, "decree", "create", nil); err != nil {
		return h.toolError(req.ID, toolName, mcpCodeForbidden,
			"operator lacks required permission decree.create")
	}

	var a decreeCreateArgs
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
	d, err := h.deps.OracleSvc.CreateDecree(ctx, oracle.CreateDecreeInput{
		Name:            a.Name,
		OnBeacon:        a.OnBeacon,
		WhereCEL:        a.WhereCEL,
		Coven:           a.Coven,
		SID:             a.SID,
		IncarnationName: a.IncarnationName,
		ActionScenario:  a.ActionScenario,
		ActionInput:     a.ActionInput,
		Cooldown:        a.Cooldown,
		Enabled:         enabled,
		CallerAID:       &callerAID,
	})
	if err != nil {
		code, detail := mapOracleErrorToMCP(err)
		if code == mcpCodeInternalError {
			h.deps.Logger.Error("mcp: oracle.decree.create failed",
				slog.String("name", a.Name), slog.String("by_aid", callerAID), slog.Any("error", err))
		}
		return h.toolError(req.ID, toolName, code, detail)
	}

	// Audit mirrors the REST handler: payload {name, on_beacon, incarnation,
	// action_scenario, subject, created_by_aid}. where-CEL and action_input
	// are NOT put in the payload (action_input may carry a vault-ref in transit).
	h.writeAudit(audit.EventDecreeCreated, callerAID, map[string]any{
		"name":            d.Name,
		"on_beacon":       d.OnBeacon,
		"incarnation":     d.IncarnationName,
		"action_scenario": d.ActionScenario,
		"subject":         decreeSubjectView(d),
		"created_by_aid":  callerAID,
	})

	return h.toolResult(req.ID, toDecreeView(d))
}

// decreeListOutput — output of keeper.oracle.decree.list: registry of
// Decrees under `decrees` (parity with REST GET /v1/decrees items).
type decreeListOutput struct {
	Decrees []decreeView `json:"decrees"`
	Total   int          `json:"total"`
}

// decreeListArgs — arguments for keeper.oracle.decree.list (optional offset/limit).
type decreeListArgs struct {
	Offset *int `json:"offset"`
	Limit  *int `json:"limit"`
}

// callOracleDecreeList — read-tool keeper.oracle.decree.list (read-only, not
// audited). RBAC — decree.list has no selector.
func (h *Handler) callOracleDecreeList(ctx context.Context, claims *jwt.Claims, req jsonRPCRequest, args json.RawMessage) jsonRPCResponse {
	const toolName = "keeper.oracle.decree.list"

	if h.deps.OracleSvc == nil {
		return h.toolError(req.ID, toolName, mcpCodeInternalError, oracleNotConfigured)
	}

	if err := h.deps.RBAC.Check(claims.Subject, "decree", "list", nil); err != nil {
		return h.toolError(req.ID, toolName, mcpCodeForbidden,
			"operator lacks required permission decree.list")
	}

	var a decreeListArgs
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
	// Upper bound on limit (security-fix, parity with omen.list): an
	// unbounded limit is a DoS vector (one request could materialize the
	// entire registry).
	if offset < 0 || limit < 1 || limit > listMaxLimit {
		return h.toolError(req.ID, toolName, mcpCodeValidationFailed,
			"offset must be >= 0 and limit must be between 1 and 1000")
	}

	decrees, total, err := h.deps.OracleSvc.ListDecrees(ctx, offset, limit)
	if err != nil {
		h.deps.Logger.Error("mcp: oracle.decree.list failed",
			slog.String("by_aid", claims.Subject), slog.Any("error", err))
		return h.toolError(req.ID, toolName, mcpCodeInternalError, "internal error")
	}

	out := decreeListOutput{Decrees: make([]decreeView, 0, len(decrees)), Total: total}
	for _, d := range decrees {
		out.Decrees = append(out.Decrees, toDecreeView(d))
	}
	return h.toolResult(req.ID, out)
}

// decreeDeleteArgs — arguments for keeper.oracle.decree.delete.
type decreeDeleteArgs struct {
	Name string `json:"name"`
}

// callOracleDecreeDelete — mutating-tool keeper.oracle.decree.delete.
// Cascades cleanup of cooldown-state (oracle_fires). RBAC — decree.delete
// has no selector.
func (h *Handler) callOracleDecreeDelete(ctx context.Context, claims *jwt.Claims, req jsonRPCRequest, args json.RawMessage) jsonRPCResponse {
	const toolName = "keeper.oracle.decree.delete"

	if h.deps.OracleSvc == nil {
		return h.toolError(req.ID, toolName, mcpCodeInternalError, oracleNotConfigured)
	}

	if err := h.deps.RBAC.Check(claims.Subject, "decree", "delete", nil); err != nil {
		return h.toolError(req.ID, toolName, mcpCodeForbidden,
			"operator lacks required permission decree.delete")
	}

	var a decreeDeleteArgs
	if len(args) > 0 {
		if err := strictUnmarshal(args, &a); err != nil {
			return h.toolError(req.ID, toolName, mcpCodeMalformedRequest, "invalid arguments: "+err.Error())
		}
	}
	if a.Name == "" {
		return h.toolError(req.ID, toolName, mcpCodeValidationFailed, "field 'name' is required")
	}

	if err := h.deps.OracleSvc.DeleteDecree(ctx, a.Name); err != nil {
		code, detail := mapOracleErrorToMCP(err)
		if code == mcpCodeInternalError {
			h.deps.Logger.Error("mcp: oracle.decree.delete failed",
				slog.String("name", a.Name), slog.String("by_aid", claims.Subject), slog.Any("error", err))
		}
		return h.toolError(req.ID, toolName, code, detail)
	}

	h.writeAudit(audit.EventDecreeDeleted, claims.Subject, map[string]any{
		"name": a.Name,
	})

	// REST returns 204 No Content; MCP equivalent is an empty output object.
	return h.toolResult(req.ID, struct{}{})
}

// decreeSubjectView — human-readable form of a Decree's subject for the
// audit payload (`coven=<v1,v2>` / `sid=<v>`). XOR is guaranteed by validation.
func decreeSubjectView(d *oracle.Decree) string {
	return oracleSubjectLabel(d.SubjectCoven, d.SubjectSID)
}

// oracleSubjectLabel — shared subject formatter (coven-list XOR sid) for the
// Vigil / Decree audit payload (parallels REST handlers.subjectLabel).
func oracleSubjectLabel(coven []string, sid *string) string {
	if len(coven) > 0 {
		s := "coven="
		for i, c := range coven {
			if i > 0 {
				s += ","
			}
			s += c
		}
		return s
	}
	if sid != nil && *sid != "" {
		return "sid=" + *sid
	}
	return ""
}
