package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"sort"

	"github.com/souls-guild/soul-stack/keeper/internal/incarnation"
	"github.com/souls-guild/soul-stack/keeper/internal/jwt"
	"github.com/souls-guild/soul-stack/keeper/internal/rbac"
	"github.com/souls-guild/soul-stack/keeper/internal/soul"
	"github.com/souls-guild/soul-stack/shared/audit"
)

// keeper.soul.traits-assign — паритет REST POST /v1/souls/traits
// (SoulHandler.AssignTraitsTyped, ADR-060). Массово merge/replace/remove
// operator-set trait-меток (jsonb-колонка `souls.traits`) на хостах под
// selector ∩ coven-scope оператора.
//
// Логика scope-intersection переиспользует тот же service-слой, что REST
// (soul.BulkAssignTraits / soul.BulkReplaceTraits / soul.CountBulkMatched +
// soul.BulkScope из PurviewResolver), без дубля бизнес-логики и без хождения
// через HTTP-handler.
//
// БЕЗОПАСНОСТЬ. Least-privilege держится ОДНИМ гейтом (a) — целевые хосты ⊆
// coven-scope оператора (предикат `coven && ARRAY[scope]` в BulkAssignTraits/
// CountBulkMatched). trait-КЛЮЧ НЕ является RBAC-измерением scope (в отличие от
// Coven-метки), поэтому гейта (b) на ключи нет; permission-проверка — bare
// `RBAC.Check(soul, traits-assign, nil)` (эквивалент REST-middleware NoSelector).
// Без неё MCP стал бы обходом REST-защиты (у MCP нет chi-middleware).

type soulTraitsAssignArgs struct {
	Mode     string                   `json:"mode,omitempty"`
	Traits   map[string]any           `json:"traits,omitempty"`
	Keys     []string                 `json:"keys,omitempty"`
	Selector soulTraitsAssignSelector `json:"selector"`
	DryRun   bool                     `json:"dry_run,omitempty"`
}

type soulTraitsAssignSelector struct {
	All         bool     `json:"all,omitempty"`
	SIDs        []string `json:"sids,omitempty"`
	Coven       string   `json:"coven,omitempty"`
	Incarnation string   `json:"incarnation,omitempty"`
	Status      string   `json:"status,omitempty"`
}

// soulTraitsAssignOutput — паритет REST soulTraitsAssignResponse. keys[] — набор
// затронутых trait-ключей; trait-значения НЕ эхуются (секрет-гигиена). json-теги
// нужны для UnmarshalJSON (тесты декодят tool-output обратной операцией).
type soulTraitsAssignOutput struct {
	Mode    string   `json:"mode"`
	Keys    []string `json:"keys"`
	Matched int      `json:"matched"`
	Changed int      `json:"changed"`
	Status  string   `json:"status"`
	DryRun  bool     `json:"dry_run"`
}

func (h *Handler) callSoulTraitsAssign(ctx context.Context, claims *jwt.Claims, req jsonRPCRequest, args json.RawMessage) jsonRPCResponse {
	const toolName = "keeper.soul.traits-assign"

	// DEPRECATED (ADR-060 amend R1): per-soul trait-write перенесён на
	// per-incarnation (keeper.incarnation.traits-set). Per-soul write перетирается
	// проекцией incarnation.traits. Tool сохранён forward-compat; вызов сигналим.
	h.deps.Logger.Warn("mcp: soul.traits-assign DEPRECATED per-soul trait-write (ADR-060) — используйте keeper.incarnation.traits-set",
		slog.String("by_aid", claims.Subject))

	if h.deps.SoulDB == nil {
		return h.toolError(req.ID, toolName, mcpCodeInternalError, "soul DB is not configured")
	}
	if h.deps.PurviewResolver == nil {
		h.deps.Logger.Error("mcp: soul.traits-assign scoper not configured")
		return h.toolError(req.ID, toolName, mcpCodeInternalError, "traits-assign unavailable")
	}

	var a soulTraitsAssignArgs
	if len(args) > 0 {
		if err := strictUnmarshal(args, &a); err != nil {
			return h.toolError(req.ID, toolName, mcpCodeMalformedRequest,
				"invalid arguments: "+err.Error())
		}
	}

	mode := soul.TraitMode(a.Mode)
	if mode == "" {
		mode = soul.TraitMerge // дефолт (паритет REST).
	}
	if !soul.ValidTraitMode(mode) {
		return h.toolError(req.ID, toolName, mcpCodeValidationFailed,
			"field 'mode' must be one of: merge, replace, remove")
	}

	// XOR traits↔keys по mode + format/значение-валидация (паритет REST handler).
	switch mode {
	case soul.TraitMerge, soul.TraitReplace:
		if len(a.Keys) > 0 {
			return h.toolError(req.ID, toolName, mcpCodeValidationFailed,
				"field 'keys' is allowed only for mode=remove; use 'traits' for merge/replace")
		}
		if err := soul.ValidateTraitDelta(a.Traits); err != nil {
			return h.toolError(req.ID, toolName, mcpCodeValidationFailed, err.Error())
		}
	case soul.TraitRemove:
		if len(a.Traits) > 0 {
			return h.toolError(req.ID, toolName, mcpCodeValidationFailed,
				"field 'traits' is allowed only for mode=merge/replace; use 'keys' for remove")
		}
		if len(a.Keys) == 0 {
			return h.toolError(req.ID, toolName, mcpCodeValidationFailed,
				"field 'keys' is required and must be non-empty for mode=remove")
		}
		if err := soul.ValidateTraitKeys(a.Keys); err != nil {
			return h.toolError(req.ID, toolName, mcpCodeValidationFailed, err.Error())
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

	// Permission-слой = existence-gate (паритет REST RequireAction(soul,
	// traits-assign)): «держит ли оператор право в ЛЮБОМ scope-измерении».
	// Селекторный RBAC.Check(nil) здесь НЕ годится — он отрезал бы coven-scoped
	// оператора (его `coven=dev`-permission не сматчила бы запрос без coven-
	// контекста), хотя тот ВПРАВЕ менять traits на dev-хостах. trait-ключ не
	// scope-измерение → гейта (b) нет; least-privilege держит ОДИН гейт (a)
	// ниже (BulkScope сужает целевые хосты). pv тут же — источник этого scope.
	pv := h.deps.PurviewResolver.ResolvePurview(claims.Subject, "soul", "traits-assign")
	if !holdsTraitsAssign(pv) {
		return h.toolError(req.ID, toolName, mcpCodeForbidden,
			"operator lacks required permission soul.traits-assign")
	}
	scope := soul.BulkScope{Covens: pv.Covens, Unrestricted: pv.Unrestricted}

	if a.DryRun {
		matched, err := soul.CountBulkMatched(ctx, h.deps.SoulDB, sel, scope)
		if err != nil {
			return h.bulkTraitsError(req.ID, toolName, err)
		}
		h.auditTraitsAssign(claims.Subject, a, mode, scope, soul.Report{
			Matched: matched,
			Status:  soul.BulkCompleted,
		}, true)
		return h.toolResult(req.ID, buildTraitsAssignOutput(a, mode, soul.Report{
			Matched: matched,
			Status:  soul.BulkCompleted,
		}, true))
	}

	var (
		rep soul.Report
		err error
	)
	if mode == soul.TraitReplace {
		rep, err = soul.BulkReplaceTraits(ctx, h.deps.SoulDB, sel, scope, a.Traits)
	} else {
		rep, err = soul.BulkAssignTraits(ctx, h.deps.SoulDB, sel, scope, mode, a.Traits, a.Keys)
	}
	if err != nil {
		// partial: часть чанков закоммичена — отдаём результат (не error),
		// чтобы оператор видел сделанное (паритет REST: 200 + status:partial).
		if rep.Status == soul.BulkPartial {
			h.deps.Logger.Warn("mcp: soul.traits-assign partial",
				slog.String("mode", a.Mode),
				slog.Int("changed", rep.Changed),
				slog.Int("chunks", rep.ChunksCommitted),
				slog.Any("error", err),
			)
			h.auditTraitsAssign(claims.Subject, a, mode, scope, rep, false)
			return h.toolResult(req.ID, buildTraitsAssignOutput(a, mode, rep, false))
		}
		return h.bulkTraitsError(req.ID, toolName, err)
	}

	h.auditTraitsAssign(claims.Subject, a, mode, scope, rep, false)
	return h.toolResult(req.ID, buildTraitsAssignOutput(a, mode, rep, false))
}

// holdsTraitsAssign — existence-gate по [rbac.Purview]: оператор держит
// soul.traits-assign, если право есть в любом scope-измерении (unrestricted либо
// непустой coven/regex/soulprint/state) и не Deny (ревокнут / explicit-deny).
// Эквивалент REST `RequireAction.HoldsAction` без расширения MCP-интерфейса
// PermissionChecker: тот же Purview, что даёт scope гейта (a).
func holdsTraitsAssign(pv rbac.Purview) bool {
	if pv.Deny {
		return false
	}
	if pv.Unrestricted {
		return true
	}
	return len(pv.Covens) > 0 || len(pv.Regexes) > 0 ||
		len(pv.SoulprintExprs) > 0 || len(pv.StateExprs) > 0
}

// buildTraitsAssignOutput собирает паритетный REST-у output.
func buildTraitsAssignOutput(a soulTraitsAssignArgs, mode soul.TraitMode, rep soul.Report, dryRun bool) soulTraitsAssignOutput {
	out := soulTraitsAssignOutput{
		Mode:    string(mode),
		Keys:    mcpAffectedTraitKeys(mode, a.Traits, a.Keys),
		Matched: rep.Matched,
		Status:  string(rep.Status),
		DryRun:  dryRun,
	}
	if !dryRun {
		out.Changed = rep.Changed
	}
	return out
}

// bulkTraitsError маппит ошибки bulk-слоя в MCP-error (паритет REST writeBulkError /
// bulkAssignError). ErrBulkEmptySelector / валидационные ошибки → validation-failed;
// прочее → internal-error с логом (oracle-attack-защита).
func (h *Handler) bulkTraitsError(id json.RawMessage, toolName string, err error) jsonRPCResponse {
	switch {
	case errors.Is(err, soul.ErrBulkEmptySelector):
		return h.toolError(id, toolName, mcpCodeValidationFailed,
			"selector matches no hosts: set one of all/sids/coven/status")
	default:
		h.deps.Logger.Error("mcp: soul.traits-assign failed", slog.Any("error", err))
		return h.toolError(id, toolName, mcpCodeInternalError, "traits-assign failed")
	}
}

// auditTraitsAssign пишет audit-event soul.traits-changed. Payload симметричен
// REST (buildTraitsAssignReply), но `source` = "mcp" — MCP-канал отделён для
// granular trail. keys — затронутые trait-ключи; trait-значения НЕ кладутся.
func (h *Handler) auditTraitsAssign(aid string, a soulTraitsAssignArgs, mode soul.TraitMode, scope soul.BulkScope, rep soul.Report, dryRun bool) {
	payload := map[string]any{
		"mode":          string(mode),
		"selector":      normalizeMCPTraitsSelector(a.Selector),
		"keys":          mcpAffectedTraitKeys(mode, a.Traits, a.Keys),
		"matched":       rep.Matched,
		"changed":       rep.Changed,
		"status":        string(rep.Status),
		"scope_applied": !scope.Unrestricted,
		"dry_run":       dryRun,
		"source":        string(audit.SourceMCP),
	}
	h.writeAudit(audit.EventSoulTraitsChanged, aid, payload)
}

// mcpAffectedTraitKeys — отсортированный набор затронутых trait-ключей (merge/replace —
// ключи map-а; remove — список keys). nil → []string{} для устойчивого JSON.
func mcpAffectedTraitKeys(mode soul.TraitMode, traits map[string]any, keys []string) []string {
	var out []string
	if mode == soul.TraitRemove {
		out = append([]string(nil), keys...)
	} else {
		out = make([]string, 0, len(traits))
		for k := range traits {
			out = append(out, k)
		}
	}
	sort.Strings(out)
	if out == nil {
		out = []string{}
	}
	return out
}

// normalizeMCPTraitsSelector — нормализованная форма селектора для audit-payload
// (паритет normalizeMCPCovenSelector).
func normalizeMCPTraitsSelector(s soulTraitsAssignSelector) map[string]any {
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
