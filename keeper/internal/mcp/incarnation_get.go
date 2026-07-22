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

// incarnationGetArgs — arguments for keeper.incarnation.get
// (schemaIncarnationGetInput: the only required field is `name`).
type incarnationGetArgs struct {
	Name string `json:"name"`
}

// incarnationGetOutput — output of keeper.incarnation.get. Mirrors REST
// IncarnationGetReply (handlers.incarnationDTO): same snake_case field set
// and the same nullable semantics (status_details / created_by_aid are
// emitted as `null` when absent, not omitted).
//
// SECRETS: spec / state are populated EXCLUSIVELY via [audit.MaskSecrets]
// (see callIncarnationGet) — defense-in-depth, parity with REST toDTO. The
// MCP output never exposes sensitive-key values or vault-refs to the operator.
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

	// ADR-031 Slice C: see handlers.incarnationDTO. LastDriftSummary is typed
	// (counts + scanned_at), same wire form as REST (shared json contract
	// incarnation.DriftScanSummary).
	LastDriftCheckAt *time.Time                    `json:"last_drift_check_at,omitempty"`
	LastDriftSummary *incarnation.DriftScanSummary `json:"last_drift_summary,omitempty"`
}

// callIncarnationGet — read-tool keeper.incarnation.get. Reference
// implementation for the incarnation read-tools rollout (list / history):
// step order —
//
//  1. strictUnmarshal arguments (DisallowUnknownFields).
//  2. validate name via [incarnation.ValidName] (parity with REST path-name).
//  3. RBAC.Check(subject, "incarnation", "get", incarnationRBACContext(name))
//     — name-bound selector, same as REST [handlers.IncarnationNameSelector].
//  4. SelectByName → errors mapped via [mapIncarnationErrorToMCP]
//     (NotFound → not-found etc.).
//  5. build output, masking spec/state via [audit.MaskSecrets].
//
// reads are NOT audited (symmetry with REST Get — no audit payload).
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
		// Fail-closed RBAC when the incarnation is missing/errored (REST parity).
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

	// RBAC OR-Check over the incarnation's coven/service scope (covens ∪
	// {name}) — mirrors REST middleware, scope from inc.Service / inc.Covens.
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
