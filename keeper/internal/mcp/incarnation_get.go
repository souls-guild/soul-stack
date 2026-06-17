package mcp

import (
	"context"
	"encoding/json"
	"log/slog"
	"time"

	"github.com/souls-guild/soul-stack/keeper/internal/incarnation"
	"github.com/souls-guild/soul-stack/keeper/internal/jwt"
	"github.com/souls-guild/soul-stack/shared/audit"
)

// incarnationGetArgs — arguments tool-а keeper.incarnation.get
// (schemaIncarnationGetInput: единственное required-поле `name`).
type incarnationGetArgs struct {
	Name string `json:"name"`
}

// incarnationGetOutput — output keeper.incarnation.get. Симметричен REST
// IncarnationGetReply (handlers.incarnationDTO): тот же snake_case-набор полей
// и та же семантика nullable (status_details / created_by_aid отдаются `null`
// при отсутствии, не пропускаются).
//
// СЕКРЕТЫ: spec / state наполняются ИСКЛЮЧИТЕЛЬНО через [audit.MaskSecrets]
// (см. callIncarnationGet) — defense-in-depth, паритет с REST toDTO. MCP-вывод
// оператору не светит ни sensitive-key-значения, ни vault-ref-ы.
type incarnationGetOutput struct {
	Name               string         `json:"name"`
	Service            string         `json:"service"`
	ServiceVersion     string         `json:"service_version"`
	StateSchemaVersion int            `json:"state_schema_version"`
	Spec               map[string]any `json:"spec"`
	State              map[string]any `json:"state"`
	Status             string         `json:"status"`
	StatusDetails      map[string]any `json:"status_details"`
	CreatedByAID       *string        `json:"created_by_aid"`
	CreatedAt          time.Time      `json:"created_at"`
	UpdatedAt          time.Time      `json:"updated_at"`

	// ADR-031 Slice C: см. handlers.incarnationDTO. LastDriftSummary — typed
	// (counts + scanned_at), wire-форма та же, что у REST (общий json-контракт
	// incarnation.DriftScanSummary).
	LastDriftCheckAt *time.Time                    `json:"last_drift_check_at,omitempty"`
	LastDriftSummary *incarnation.DriftScanSummary `json:"last_drift_summary,omitempty"`
}

// callIncarnationGet — read-tool keeper.incarnation.get. Эталон тиража
// incarnation read-tool-ов (list / history): порядок шагов —
//
//  1. strictUnmarshal arguments (DisallowUnknownFields).
//  2. валидация name по [incarnation.ValidName] (паритет с REST path-name).
//  3. RBAC.Check(subject, "incarnation", "get", incarnationRBACContext(name))
//     — name-bound селектор, тот же, что REST [handlers.IncarnationNameSelector].
//  4. SelectByName → ошибки маппятся [mapIncarnationErrorToMCP]
//     (NotFound → not-found и т.д.).
//  5. построение output с маскингом spec/state через [audit.MaskSecrets].
//
// reads НЕ аудируются (симметрия с REST Get — без audit-payload).
func (h *Handler) callIncarnationGet(ctx context.Context, claims *jwt.Claims, req jsonRPCRequest, args json.RawMessage) jsonRPCResponse {
	const toolName = "keeper.incarnation.get"

	var a incarnationGetArgs
	if len(args) > 0 {
		if err := strictUnmarshal(args, &a); err != nil {
			return h.toolError(req.ID, toolName, mcpCodeMalformedRequest,
				"invalid arguments: "+err.Error())
		}
	}
	if a.Name == "" {
		return h.toolError(req.ID, toolName, mcpCodeValidationFailed, "field 'name' is required")
	}
	if !incarnation.ValidName(a.Name) {
		return h.toolError(req.ID, toolName, mcpCodeValidationFailed,
			"field 'name' must match "+incarnation.NamePattern)
	}

	inc, err := incarnation.SelectByName(ctx, h.deps.IncarnationDB, a.Name)
	if err != nil {
		// Fail-closed RBAC при ненайденной/сбойной incarnation (паритет REST).
		if scopeErr := h.checkIncarnationScope(claims, "get", a.Name, "", nil); scopeErr != nil {
			return h.toolError(req.ID, toolName, mcpCodeForbidden,
				"operator lacks required permission incarnation.get")
		}
		code, detail := mapIncarnationErrorToMCP(err)
		if code == mcpCodeInternalError {
			h.deps.Logger.Error("mcp: incarnation.get select failed",
				slog.String("name", a.Name),
				slog.String("by_aid", claims.Subject),
				slog.Any("error", err),
			)
		}
		return h.toolError(req.ID, toolName, code, detail)
	}

	// RBAC OR-Check по coven/service-scope incarnation (covens ∪ {name}) —
	// зеркало REST middleware, scope из inc.Service / inc.Covens.
	if err := h.checkIncarnationScope(claims, "get", inc.Name, inc.Service, inc.Covens); err != nil {
		return h.toolError(req.ID, toolName, mcpCodeForbidden,
			"operator lacks required permission incarnation.get")
	}

	return h.toolResult(req.ID, incarnationGetOutput{
		Name:               inc.Name,
		Service:            inc.Service,
		ServiceVersion:     inc.ServiceVersion,
		StateSchemaVersion: inc.StateSchemaVersion,
		Spec:               audit.MaskSecrets(inc.Spec),
		State:              audit.MaskSecrets(inc.State),
		Status:             string(inc.Status),
		StatusDetails:      inc.StatusDetails,
		CreatedByAID:       inc.CreatedByAID,
		CreatedAt:          inc.CreatedAt,
		UpdatedAt:          inc.UpdatedAt,
		LastDriftCheckAt:   inc.LastDriftCheckAt,
		LastDriftSummary:   inc.LastDriftSummary,
	})
}
