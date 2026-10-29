package mcp

import (
	"encoding/json"
	"errors"

	"github.com/souls-guild/soul-stack/keeper/internal/api/handlers"
)

// runGateToolError maps a refusal of the start gate (handlers.PlanRunStart,
// handlers.CheckFreezeConsent; ADR-0093 §5) to its tool error, as REST maps it
// to its problem. ok=false for any other error: the caller reports it as
// internal.
func (h *Handler) runGateToolError(id json.RawMessage, toolName string, err error) (jsonRPCResponse, bool) {
	var fe *handlers.FreezeActiveError
	if errors.As(err, &fe) {
		return h.toolError(id, toolName, mcpCodeFreezeActive, fe.Error()), true
	}
	var ne *handlers.NoCommonWindowError
	if errors.As(err, &ne) {
		return h.toolError(id, toolName, mcpCodeValidationFailed, ne.Error()), true
	}
	return jsonRPCResponse{}, false
}
