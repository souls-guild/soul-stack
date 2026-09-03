package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"time"

	"github.com/souls-guild/soul-stack/keeper/internal/jwt"
	"github.com/souls-guild/soul-stack/keeper/internal/oracle"
	"github.com/souls-guild/soul-stack/shared/audit"
)

// callOracleVigilSetLabel — keeper.oracle.vigil.label-set, the MCP mirror of
// PUT /v1/vigils/{id}/label (ADR-0085). The registry's only operator mutation.
func (h *Handler) callOracleVigilSetLabel(ctx context.Context, claims *jwt.Claims, req jsonRPCRequest, args json.RawMessage) jsonRPCResponse {
	return callLabelSet(h, ctx, claims, req, args, labelSetSpec[vigilView]{
		tool:          "keeper.oracle.vigil.label-set",
		resource:      "vigil",
		configured:    h.deps.OracleSvc != nil,
		notConfigured: oracleNotConfigured,
		validID:       oracle.ValidID,
		idPattern:     oracle.IDPattern,
		set: func(ctx context.Context, id string, label *string) (vigilView, *string, error) {
			v, previous, err := h.deps.OracleSvc.SetVigilLabel(ctx, id, label)
			if err != nil {
				return vigilView{}, nil, err
			}
			return toVigilView(v), previous, nil
		},
		isNotFound: func(err error) bool { return errors.Is(err, oracle.ErrVigilNotFound) },
		notFoundf:  func(id string) string { return "vigil " + id + " not found" },
		failMsg:    "set vigil label failed",
		event:      audit.EventVigilLabelChanged,
	})
}

// oracleNotConfigured — public-detail of oracle-tools' nil-guard. OracleSvc is
// an optional HandlerDeps field (production wire-up passes the same
// *oracle.Service as REST): when nil, oracle-tools dispatch but return
// internal-error "not configured" (same pattern as AugurSvc/ServiceSvc).
const oracleNotConfigured = "oracle registry is not configured"

// vigilView — output projection of a Vigil for oracle-tools (schemaVigilView).
// 1:1 with REST vigilResponse / [oracle.Vigil].
type vigilView struct {
	ID string `json:"id"`
	// Label — display caption (ADR-0085); absent → a consumer shows `name`.
	Label        *string         `json:"label,omitempty"`
	Subject      subjectPayload  `json:"subject"`
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
		ID:           v.ID,
		Label:        v.Label,
		Subject:      toSubjectPayload(v.Subject()),
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
// subject carries exactly one of the four dimensions ([subjectPayload]);
// enabled is optional (omitted → true).
type vigilCreateArgs struct {
	ID string `json:"id"`
	// Label — optional display caption (ADR-0085), free text; changed afterwards
	// by keeper.oracle.vigil.label-set.
	Label    *string         `json:"label"`
	Subject  subjectPayload  `json:"subject"`
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
	if a.ID == "" {
		return h.toolError(req.ID, toolName, mcpCodeValidationFailed, "field 'id' is required")
	}

	enabled := true
	if a.Enabled != nil {
		enabled = *a.Enabled
	}

	callerAID := claims.Subject
	v, err := h.deps.OracleSvc.CreateVigil(ctx, oracle.CreateVigilInput{
		ID:        a.ID,
		Label:     a.Label,
		Subject:   a.Subject.selector(),
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
				slog.String("id", a.ID), slog.String("by_aid", callerAID), slog.Any("error", err))
		}
		return h.toolError(req.ID, toolName, code, detail)
	}

	// Audit — mirrors the REST handler: payload {name, check, interval,
	// subject, created_by_aid}. params is NOT included in the payload.
	h.writeAudit(audit.EventVigilCreated, callerAID, map[string]any{
		"id":             v.ID,
		"label":          v.Label,
		"check":          v.CheckAddr,
		"interval":       v.IntervalSpec,
		"subject":        v.Subject().String(),
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
	ID string `json:"id"`
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
	if a.ID == "" {
		return h.toolError(req.ID, toolName, mcpCodeValidationFailed, "field 'id' is required")
	}

	if err := h.deps.OracleSvc.DeleteVigil(ctx, a.ID); err != nil {
		code, detail := mapOracleErrorToMCP(err)
		if code == mcpCodeInternalError {
			h.deps.Logger.Error("mcp: oracle.vigil.delete failed",
				slog.String("id", a.ID), slog.String("by_aid", claims.Subject), slog.Any("error", err))
		}
		return h.toolError(req.ID, toolName, code, detail)
	}

	h.writeAudit(audit.EventVigilDeleted, claims.Subject, map[string]any{
		"id": a.ID,
	})

	// REST returns 204 No Content; the MCP equivalent is an empty output object.
	return h.toolResult(req.ID, struct{}{})
}
