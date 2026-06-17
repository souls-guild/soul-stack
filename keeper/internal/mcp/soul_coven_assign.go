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

// keeper.soul.coven-assign — паритет REST POST /v1/souls/coven
// (SoulHandler.AssignCoven). Массово добавляет (append) или снимает (remove)
// ОДНУ Coven-метку с хостов под selector ∩ coven-scope оператора.
//
// Логика scope-intersection переиспользует тот же service-слой, что REST
// (soul.BulkAssignCoven / soul.CountBulkMatched + soul.BulkScope из
// CovenScope), без дубля бизнес-логики и без хождения через HTTP-handler.
//
// БЕЗОПАСНОСТЬ — двойная проверка scope, идентичная REST:
//   - гейт (a): целевые хосты ⊆ coven-scope (предикат `coven && ARRAY[scope]`
//     в BulkAssignCoven/CountBulkMatched);
//   - гейт (b): назначаемая (append) метка ∈ scope. В REST его держат ДВА слоя —
//     middleware RBAC.Check с селектором `{coven: label}` (SoulCovenLabelSelector)
//     и service-проверка (ErrBulkLabelOutOfScope). У MCP middleware-селектора
//     нет, поэтому RBAC.Check с `{"coven": label}` зовётся здесь явно, плюс
//     остаётся service-гейт. Без обоих MCP стал бы обходом REST-защиты.

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

// soulCovenAssignOutput — паритет REST soulCovenAssignResponse. Для
// mode=replace отдаём `labels` (включая пустой `[]`), для append/remove — `label`.
// MarshalJSON решает XOR на сериализации (`omitempty` на []string не годится:
// пустой набор для replace надо отдать как `[]`, а не опустить поле). json-теги
// нужны для UnmarshalJSON (тесты декодят tool-output обратной операцией).
type soulCovenAssignOutput struct {
	Mode    string   `json:"mode"`
	Label   string   `json:"label,omitempty"`
	Labels  []string `json:"labels,omitempty"`
	HasSet  bool     `json:"-"` // только для MarshalJSON.
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

	// XOR label↔labels по mode (паритет REST handler).
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

	// Гейт (b), permission-слой: RBAC.Check с селектором `{coven: label}` —
	// эквивалент REST-middleware SoulCovenLabelSelector. Для append/remove —
	// одна метка. Для replace — проверяем КАЖДУЮ метку набора (множественные
	// последовательные Check; иначе coven-scoped оператор с scope `dev` мог
	// бы пройти как `labels=[dev, prod]` — на первой метке). Без обоих гейтов
	// MCP стал бы обходом REST-защиты.
	switch mode {
	case soul.CovenAppend, soul.CovenRemove:
		if err := h.deps.RBAC.Check(claims.Subject, "soul", "coven-assign",
			map[string]string{"coven": a.Label}); err != nil {
			return h.toolError(req.ID, toolName, mcpCodeForbidden,
				"operator lacks required permission soul.coven-assign")
		}
	case soul.CovenReplace:
		// Пустой набор: ни одной метки в проверке — bare-permission
		// достаточна, coven-scoped без scope-match отвалится на service-гейте
		// (a) (целевые хосты ⊆ scope).
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

	pv := h.deps.PurviewResolver.ResolvePurview(claims.Subject, "soul", "coven-assign")
	scope := soul.BulkScope{Covens: pv.Covens, Unrestricted: pv.Unrestricted}

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
		// partial: часть чанков закоммичена — отдаём результат (не error),
		// чтобы оператор видел сделанное и мог до-повторить идемпотентно
		// (паритет REST: 200 + status:partial).
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

// buildCovenAssignOutput собирает паритетный REST-у output: для append/remove —
// одна `label`, для replace — `labels[]` (nil→[] для устойчивого JSON).
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

// bulkAssignError маппит ошибки bulk-слоя в MCP-error. ErrBulkEmptySelector и
// ErrBulkLabelOutOfScope → validation-failed (паритет REST TypeValidationFailed
// в writeBulkError); прочее → internal-error с логом (oracle-attack-защита, как
// в соседних мапперах).
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

// auditCovenAssign пишет audit-event soul.coven-changed. Payload симметричен
// REST (respondCovenAssign), но `source` = "mcp" (string(audit.SourceMCP)) —
// MCP-канал отделён от api/keeper_internal для granular trail.
//
// `label`/`labels` — XOR по mode (REST-паритет): append/remove → `label`,
// replace → `labels` (всегда массив, в т.ч. пустой при «снять все»).
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

// normalizeMCPCovenSelector — нормализованная форма селектора для audit-payload
// (паритет handlers.normalizeCovenSelector, приватного для пакета handlers).
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
