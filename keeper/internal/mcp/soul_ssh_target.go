package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"

	"github.com/souls-guild/soul-stack/keeper/internal/jwt"
	"github.com/souls-guild/soul-stack/keeper/internal/pushprovider"
	"github.com/souls-guild/soul-stack/keeper/internal/soul"
	"github.com/souls-guild/soul-stack/shared/audit"
)

// keeper.soul.ssh-target.update — parity with REST PUT
// /v1/souls/{sid}/ssh-target (SoulHandler.UpdateSshTarget, ADR-032
// amendment 2026-05-26, S7-1). MCP tool name is 3-segment
// (`keeper.soul.ssh-target.update`), permission is 2-segment
// (`soul.ssh-target-update`); same pattern as `keeper.sigil.key.<verb>` ↔
// `sigil.key-<verb>` (see catalog.go).
//
// Same logic as REST: field validation → soul.UpdateSshTarget → audit.

type soulSshTargetArgs struct {
	SID         string `json:"sid"`
	SSHPort     int    `json:"ssh_port"`
	SSHUser     string `json:"ssh_user"`
	SoulPath    string `json:"soul_path"`
	SSHProvider string `json:"ssh_provider,omitempty"`
}

type soulSshTargetOutput struct {
	SID       string                  `json:"sid"`
	SSHTarget soulSshTargetOutputBody `json:"ssh_target"`
}

type soulSshTargetOutputBody struct {
	SSHPort     int    `json:"ssh_port"`
	SSHUser     string `json:"ssh_user"`
	SoulPath    string `json:"soul_path"`
	SSHProvider string `json:"ssh_provider,omitempty"`
}

func (h *Handler) callSoulSshTargetUpdate(ctx context.Context, claims *jwt.Claims, req jsonRPCRequest, args json.RawMessage) jsonRPCResponse {
	const toolName = "keeper.soul.ssh-target.update"

	if h.deps.SoulDB == nil {
		return h.toolError(req.ID, toolName, mcpCodeInternalError, "soul DB is not configured")
	}

	var a soulSshTargetArgs
	if len(args) > 0 {
		if err := strictUnmarshal(args, &a); err != nil {
			return h.toolError(req.ID, toolName, mcpCodeMalformedRequest,
				"invalid arguments: "+err.Error())
		}
	}
	if a.SID == "" {
		return h.toolError(req.ID, toolName, mcpCodeValidationFailed, "field 'sid' is required")
	}
	if !soul.ValidSID(a.SID) {
		return h.toolError(req.ID, toolName, mcpCodeValidationFailed,
			"field 'sid' must match "+soul.SIDPattern)
	}
	if a.SSHPort < 1 || a.SSHPort > 65535 {
		return h.toolError(req.ID, toolName, mcpCodeValidationFailed,
			"field 'ssh_port' must be in [1..65535]")
	}
	if a.SSHUser == "" {
		return h.toolError(req.ID, toolName, mcpCodeValidationFailed,
			"field 'ssh_user' is required")
	}
	if a.SoulPath == "" || a.SoulPath[0] != '/' {
		return h.toolError(req.ID, toolName, mcpCodeValidationFailed,
			"field 'soul_path' must be an absolute Unix path (start with '/')")
	}
	// P2 W-1: optional `ssh_provider` — kebab-case plugin name (see the
	// push_providers.name regex). An empty string means "not set".
	if a.SSHProvider != "" && !pushprovider.ValidName(a.SSHProvider) {
		return h.toolError(req.ID, toolName, mcpCodeValidationFailed,
			"field 'ssh_provider' must match "+pushprovider.NamePattern)
	}

	// RBAC check — `soul.ssh-target-update` with selector `host=<sid>` (REST:
	// SoulSIDSelector). Mirrors keeper.soul.issue-token.
	if err := h.deps.RBAC.Check(claims.Subject, "soul", "ssh-target-update", map[string]string{"host": a.SID}); err != nil {
		return h.toolError(req.ID, toolName, mcpCodeForbidden,
			"operator lacks required permission soul.ssh-target-update")
	}

	target := &soul.SSHTarget{
		SSHPort:  a.SSHPort,
		SSHUser:  a.SSHUser,
		SoulPath: a.SoulPath,
	}
	if a.SSHProvider != "" {
		sp := a.SSHProvider
		target.SSHProvider = &sp
	}
	if err := soul.UpdateSshTarget(ctx, h.deps.SoulDB, a.SID, target); err != nil {
		if errors.Is(err, soul.ErrSoulNotFound) {
			return h.toolError(req.ID, toolName, mcpCodeNotFound, "soul "+a.SID+" not found")
		}
		h.deps.Logger.Error("mcp: soul.ssh-target.update failed",
			slog.String("sid", a.SID), slog.Any("error", err))
		return h.toolError(req.ID, toolName, mcpCodeInternalError, "update ssh_target failed")
	}

	auditPayload := map[string]any{
		"sid":       a.SID,
		"ssh_port":  a.SSHPort,
		"ssh_user":  a.SSHUser,
		"soul_path": a.SoulPath,
	}
	if a.SSHProvider != "" {
		auditPayload["ssh_provider"] = a.SSHProvider
	}
	h.writeAudit(audit.EventSoulSshTargetUpdated, claims.Subject, auditPayload)

	return h.toolResult(req.ID, soulSshTargetOutput{
		SID: a.SID,
		SSHTarget: soulSshTargetOutputBody{
			SSHPort:     a.SSHPort,
			SSHUser:     a.SSHUser,
			SoulPath:    a.SoulPath,
			SSHProvider: a.SSHProvider,
		},
	})
}
