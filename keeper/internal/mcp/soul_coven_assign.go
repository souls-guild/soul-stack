package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"

	"github.com/souls-guild/soul-stack/keeper/internal/incarnation"
	"github.com/souls-guild/soul-stack/keeper/internal/jwt"
	"github.com/souls-guild/soul-stack/keeper/internal/soul"
	"github.com/souls-guild/soul-stack/shared/audit"
)

// keeper.soul.coven-assign — parity with REST POST /v1/souls/coven
// (SoulHandler.AssignCoven). Bulk-adds (append) or removes (remove) ONE
// Coven label on hosts under selector ∩ operator coven-scope.
//
// Scope-intersection logic reuses the same service layer as REST
// (soul.BulkAssignCoven / soul.CountBulkMatched + soul.BulkScope from
// CovenScope) — no duplicated business logic and no HTTP-handler round-trip.
//
// SECURITY — dual scope check, identical to REST:
//   - gate (a): target hosts ⊆ coven-scope (predicate `coven && ARRAY[scope]`
//     in BulkAssignCoven/CountBulkMatched);
//   - gate (b): the (append) label being assigned ∈ scope. REST enforces
//     this with TWO layers — middleware RBAC.Check with selector
//     `{coven: label}` (SoulCovenLabelSelector) plus a service check
//     (ErrBulkLabelOutOfScope). MCP has no middleware selector, so
//     RBAC.Check with `{"coven": label}` is called explicitly here, on top
//     of the service gate. Without both, MCP would bypass REST protection.

type soulCovenAssignArgs struct {
	Mode     string                  `json:"mode"`
	Label    string                  `json:"label,omitempty"`
	Labels   []string                `json:"labels,omitempty"`
	Selector soulCovenAssignSelector `json:"selector"`
	DryRun   bool                    `json:"dry_run,omitempty"`
}

type soulCovenAssignSelector struct {
	All         bool     `json:"all,omitempty"`
	SIDs        []string `json:"sids,omitempty"`
	Coven       string   `json:"coven,omitempty"`
	Incarnation string   `json:"incarnation,omitempty"`
	Status      string   `json:"status,omitempty"`
}

// soulCovenAssignOutput — parity with REST soulCovenAssignResponse. For
// mode=replace returns `labels` (including empty `[]`), for append/remove —
// `label`. MarshalJSON resolves the XOR at serialization time (`omitempty`
// on []string won't work: an empty replace set must render as `[]`, not be
// omitted). json tags are needed for UnmarshalJSON (tests decode tool
// output back).
type soulCovenAssignOutput struct {
	Mode    string   `json:"mode"`
	Label   string   `json:"label,omitempty"`
	Labels  []string `json:"labels,omitempty"`
	HasSet  bool     `json:"-"` // MarshalJSON only.
	Matched int      `json:"matched"`
	Changed int      `json:"changed"`
	Status  string   `json:"status"`
	DryRun  bool     `json:"dry_run"`
}

func (o soulCovenAssignOutput) MarshalJSON() ([]byte, error) {
	out := map[string]any{
		"mode":    o.Mode,
		"matched": o.Matched,
		"changed": o.Changed,
		"status":  o.Status,
		"dry_run": o.DryRun,
	}
	if o.HasSet {
		labels := o.Labels
		if labels == nil {
			labels = []string{}
		}
		out["labels"] = labels
	} else {
		out["label"] = o.Label
	}
	return json.Marshal(out)
}

func (h *Handler) callSoulCovenAssign(ctx context.Context, claims *jwt.Claims, req jsonRPCRequest, args json.RawMessage) jsonRPCResponse {
	const toolName = "keeper.soul.coven-assign"

	if h.deps.SoulDB == nil {
		return h.toolError(req.ID, toolName, mcpCodeInternalError, "soul DB is not configured")
	}
	if h.deps.PurviewResolver == nil {
		h.deps.Logger.Error("mcp: soul.coven-assign scoper not configured")
		return h.toolError(req.ID, toolName, mcpCodeInternalError, "coven-assign unavailable")
	}

	var a soulCovenAssignArgs
	if len(args) > 0 {
		if err := strictUnmarshal(args, &a); err != nil {
			return h.toolError(req.ID, toolName, mcpCodeMalformedRequest,
				"invalid arguments: "+err.Error())
		}
	}

	mode := soul.CovenMode(a.Mode)
	if !soul.ValidCovenMode(mode) {
		return h.toolError(req.ID, toolName, mcpCodeValidationFailed,
			"field 'mode' must be one of: append, remove, replace")
	}

	// XOR label↔labels by mode (parity with REST handler).
	switch mode {
	case soul.CovenAppend, soul.CovenRemove:
		if len(a.Labels) > 0 {
			return h.toolError(req.ID, toolName, mcpCodeValidationFailed,
				"field 'labels' is allowed only for mode=replace; use 'label' for append/remove")
		}
		if !soul.ValidCoven(a.Label) {
			return h.toolError(req.ID, toolName, mcpCodeValidationFailed,
				"field 'label' must match "+soul.CovenPattern)
		}
		if err := (soul.NoopCovenLabelValidator{}).Validate(a.Label); err != nil {
			return h.toolError(req.ID, toolName, mcpCodeValidationFailed, err.Error())
		}
	case soul.CovenReplace:
		if a.Label != "" {
			return h.toolError(req.ID, toolName, mcpCodeValidationFailed,
				"field 'label' is allowed only for mode=append/remove; use 'labels' for replace")
		}
		for _, l := range a.Labels {
			if !soul.ValidCoven(l) {
				return h.toolError(req.ID, toolName, mcpCodeValidationFailed,
					"labels entry "+l+" must match "+soul.CovenPattern)
			}
			if err := (soul.NoopCovenLabelValidator{}).Validate(l); err != nil {
				return h.toolError(req.ID, toolName, mcpCodeValidationFailed, err.Error())
			}
		}
	}

	sel := soul.BulkSelector{
		All:         a.Selector.All,
		SIDs:        a.Selector.SIDs,
		Coven:       a.Selector.Coven,
		Incarnation: a.Selector.Incarnation,
	}
	if a.Selector.Status != "" {
		st := soul.Status(a.Selector.Status)
		if !soul.ValidStatus(st) {
			return h.toolError(req.ID, toolName, mcpCodeValidationFailed,
				"selector 'status' must be one of pending/connected/disconnected/revoked/expired/destroyed")
		}
		sel.Status = st
	}
	for _, s := range a.Selector.SIDs {
		if !soul.ValidSID(s) {
			return h.toolError(req.ID, toolName, mcpCodeValidationFailed,
				"selector 'sids' entry "+s+" must match "+soul.SIDPattern)
		}
	}
	if a.Selector.Coven != "" && !soul.ValidCoven(a.Selector.Coven) {
		return h.toolError(req.ID, toolName, mcpCodeValidationFailed,
			"selector 'coven' must match "+soul.CovenPattern)
	}
	if a.Selector.Incarnation != "" && !incarnation.ValidName(a.Selector.Incarnation) {
		return h.toolError(req.ID, toolName, mcpCodeValidationFailed,
			"selector 'incarnation' must match "+incarnation.NamePattern)
	}

	// Gate (b), permission layer: RBAC.Check with selector `{coven: label}`
	// — equivalent to REST middleware SoulCovenLabelSelector. append/remove
	// check one label. replace checks EVERY label in the set (multiple
	// sequential Check calls; otherwise a coven-scoped operator with scope
	// `dev` could pass `labels=[dev, prod]` on the first label alone).
	// Without both gates, MCP would bypass REST protection.
	switch mode {
	case soul.CovenAppend, soul.CovenRemove:
		if err := h.deps.RBAC.Check(claims.Subject, "soul", "coven-assign",
			map[string]string{"coven": a.Label}); err != nil {
			return h.toolError(req.ID, toolName, mcpCodeForbidden,
				"operator lacks required permission soul.coven-assign")
		}
	case soul.CovenReplace:
		// Empty set: no label to check — bare permission is enough;
		// coven-scoped without a scope match fails at service gate (a)
		// (target hosts ⊆ scope).
		if len(a.Labels) == 0 {
			if err := h.deps.RBAC.Check(claims.Subject, "soul", "coven-assign", nil); err != nil {
				return h.toolError(req.ID, toolName, mcpCodeForbidden,
					"operator lacks required permission soul.coven-assign")
			}
		}
		for _, l := range a.Labels {
			if err := h.deps.RBAC.Check(claims.Subject, "soul", "coven-assign",
				map[string]string{"coven": l}); err != nil {
				return h.toolError(req.ID, toolName, mcpCodeForbidden,
					"operator lacks required permission soul.coven-assign")
			}
		}
	}

	// Bulk mutation targets by coven → project the boolean scope onto its coven
	// dimension ([rbac.Enforcer.CovenScope], NIM-128), reached via the same narrow
	// assertion as traits-assign (the shared PurviewResolver only exposes
	// ResolvePurview; nil/absent → fail-closed internal error).
	scoper, ok := h.deps.PurviewResolver.(covenScoper)
	if !ok {
		h.deps.Logger.Error("mcp: soul.coven-assign resolver lacks CovenScope")
		return h.toolError(req.ID, toolName, mcpCodeInternalError, "coven-assign unavailable")
	}
	covens, unrestricted := scoper.CovenScope(claims.Subject, "soul", "coven-assign")
	scope := soul.BulkScope{Covens: covens, Unrestricted: unrestricted}

	if a.DryRun {
		matched, err := soul.CountBulkMatched(ctx, h.deps.SoulDB, sel, scope)
		if err != nil {
			return h.bulkAssignError(req.ID, toolName, err)
		}
		h.auditCovenAssign(claims.Subject, a, mode, scope, soul.Report{
			Matched: matched,
			Status:  soul.BulkCompleted,
		}, true)
		return h.toolResult(req.ID, buildCovenAssignOutput(a, mode, soul.Report{
			Matched: matched,
			Status:  soul.BulkCompleted,
		}, true))
	}

	var (
		rep soul.Report
		err error
	)
	if mode == soul.CovenReplace {
		rep, err = soul.BulkReplaceCoven(ctx, h.deps.SoulDB, sel, scope, a.Labels)
	} else {
		rep, err = soul.BulkAssignCoven(ctx, h.deps.SoulDB, sel, scope, a.Label, mode)
	}
	if err != nil {
		// partial: some chunks committed — return a result (not an error)
		// so the operator can see what happened and idempotently retry
		// (parity with REST: 200 + status:partial).
		if rep.Status == soul.BulkPartial {
			h.deps.Logger.Warn("mcp: soul.coven-assign partial",
				slog.String("label", a.Label),
				slog.Any("labels", a.Labels),
				slog.String("mode", a.Mode),
				slog.Int("changed", rep.Changed),
				slog.Int("chunks", rep.ChunksCommitted),
				slog.Any("error", err),
			)
			h.auditCovenAssign(claims.Subject, a, mode, scope, rep, false)
			return h.toolResult(req.ID, buildCovenAssignOutput(a, mode, rep, false))
		}
		return h.bulkAssignError(req.ID, toolName, err)
	}

	h.auditCovenAssign(claims.Subject, a, mode, scope, rep, false)
	return h.toolResult(req.ID, buildCovenAssignOutput(a, mode, rep, false))
}

// buildCovenAssignOutput assembles REST-parity output: one `label` for
// append/remove, `labels[]` for replace (nil→[] for stable JSON).
func buildCovenAssignOutput(a soulCovenAssignArgs, mode soul.CovenMode, rep soul.Report, dryRun bool) soulCovenAssignOutput {
	out := soulCovenAssignOutput{
		Mode:    string(mode),
		Matched: rep.Matched,
		Status:  string(rep.Status),
		DryRun:  dryRun,
	}
	if !dryRun {
		out.Changed = rep.Changed
	}
	if mode == soul.CovenReplace {
		labels := a.Labels
		if labels == nil {
			labels = []string{}
		}
		out.Labels = labels
		out.HasSet = true
	} else {
		out.Label = a.Label
	}
	return out
}

// bulkAssignError maps bulk-layer errors to an MCP error. ErrBulkEmptySelector
// and ErrBulkLabelOutOfScope → validation-failed (parity with REST
// TypeValidationFailed in writeBulkError); everything else → internal-error
// with a log entry (oracle-attack protection, same as neighboring mappers).
func (h *Handler) bulkAssignError(id json.RawMessage, toolName string, err error) jsonRPCResponse {
	switch {
	case errors.Is(err, soul.ErrBulkEmptySelector):
		return h.toolError(id, toolName, mcpCodeValidationFailed,
			"selector matches no hosts: set one of all/sids/coven/status")
	case errors.Is(err, soul.ErrBulkLabelOutOfScope):
		return h.toolError(id, toolName, mcpCodeValidationFailed,
			"label is outside operator coven-scope")
	default:
		h.deps.Logger.Error("mcp: soul.coven-assign failed", slog.Any("error", err))
		return h.toolError(id, toolName, mcpCodeInternalError, "coven-assign failed")
	}
}

// auditCovenAssign writes the soul.coven-changed audit event. Payload
// mirrors REST (respondCovenAssign), but `source` = "mcp"
// (string(audit.SourceMCP)) — the MCP channel is separated from
// api/keeper_internal for a granular trail.
//
// `label`/`labels` is XOR by mode (REST parity): append/remove → `label`,
// replace → `labels` (always an array, including empty for "remove all").
func (h *Handler) auditCovenAssign(aid string, a soulCovenAssignArgs, mode soul.CovenMode, scope soul.BulkScope, rep soul.Report, dryRun bool) {
	payload := map[string]any{
		"mode":          string(mode),
		"selector":      normalizeMCPCovenSelector(a.Selector),
		"matched":       rep.Matched,
		"changed":       rep.Changed,
		"status":        string(rep.Status),
		"scope_applied": !scope.Unrestricted,
		"dry_run":       dryRun,
		"source":        string(audit.SourceMCP),
	}
	if mode == soul.CovenReplace {
		labels := a.Labels
		if labels == nil {
			labels = []string{}
		}
		payload["labels"] = labels
	} else {
		payload["label"] = a.Label
	}
	h.writeAudit(audit.EventSoulCovenChanged, aid, payload)
}

// normalizeMCPCovenSelector — normalized selector form for the audit
// payload (mirrors handlers.normalizeCovenSelector, private to package handlers).
func normalizeMCPCovenSelector(s soulCovenAssignSelector) map[string]any {
	out := map[string]any{"all": s.All}
	if len(s.SIDs) > 0 {
		out["sids"] = s.SIDs
	}
	if s.Coven != "" {
		out["coven"] = s.Coven
	}
	if s.Incarnation != "" {
		out["incarnation"] = s.Incarnation
	}
	if s.Status != "" {
		out["status"] = s.Status
	}
	return out
}
