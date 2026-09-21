package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"

	"github.com/souls-guild/soul-stack/keeper/internal/jwt"
	"github.com/souls-guild/soul-stack/keeper/internal/soul"
	"github.com/souls-guild/soul-stack/keeper/internal/soulforget"
	"github.com/souls-guild/soul-stack/shared/audit"
)

// keeper.soul.forget — parity with REST DELETE /v1/souls/{sid}
// (SoulHandler.ForgetTyped). Erases a host from the registry and releases what
// it held.
//
// Both surfaces call the same [soulforget.Erase], so the ordering that makes
// the operation safe — broadcast, then delete, then close and purge — is not
// re-implemented here and cannot drift between REST and MCP.
//
// There is no `force` argument, on purpose: a single verb must not be able to
// degrade from "release the host" to "drop its row". If the cluster cannot be
// told, the tool fails with NOTHING deleted; if the release only partly
// succeeded, `warnings` says which resource is still held.

type soulForgetArgs struct {
	SID string `json:"sid"`
}

// soulForgetOutput — parity with the REST SoulForgetReply body. warnings is
// always present (empty list = fully released), so an agent reading this cannot
// mistake "no warnings" for "field absent, probably fine".
type soulForgetOutput struct {
	SID                string   `json:"sid"`
	StatusBefore       string   `json:"status_before"`
	SeedsRevoked       int64    `json:"seeds_revoked"`
	BootstrapsBurned   int64    `json:"bootstraps_burned"`
	MembershipsSevered int64    `json:"memberships_severed"`
	ChoirVoicesRemoved int64    `json:"choir_voices_removed"`
	LocalStreamClosed  bool     `json:"local_stream_closed"`
	Broadcast          bool     `json:"broadcast"`
	CacheKeysPurged    int64    `json:"cache_keys_purged"`
	Warnings           []string `json:"warnings"`
}

func (h *Handler) callSoulForget(ctx context.Context, claims *jwt.Claims, req jsonRPCRequest, args json.RawMessage) jsonRPCResponse {
	const toolName = "keeper.soul.forget"

	if h.deps.SoulDB == nil {
		return h.toolError(req.ID, toolName, mcpCodeInternalError, "soul DB is not configured")
	}

	var a soulForgetArgs
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

	// RBAC — `soul.forget` over the host's scope, `host=<sid>` plus its Coven
	// labels (REST: SoulSIDScopeSelector, NIM-588).
	// Its own permission, not `soul.create`: onboarding a host and erasing one
	// are different grants, and this is the only soul tool that destroys rows
	// the operator never named (memberships and Choir Voices cascade with it).
	if err := h.checkSoulHostScope(ctx, claims, "forget", a.SID); err != nil {
		return h.toolError(req.ID, toolName, mcpCodeForbidden,
			"operator lacks required permission soul.forget")
	}

	reason := "forgotten by " + claims.Subject
	res, err := soulforget.Erase(ctx, h.deps.SoulDB, h.deps.SoulTeardown, a.SID, reason)
	if err != nil {
		switch {
		case errors.Is(err, soul.ErrSoulNotFound):
			return h.toolError(req.ID, toolName, mcpCodeNotFound, "soul "+a.SID+" not found")
		case errors.Is(err, soulforget.ErrTeardownUnavailable):
			h.deps.Logger.Error("mcp: soul.forget cluster teardown notice failed, nothing deleted",
				slog.String("sid", a.SID), slog.Any("error", err))
			return h.toolError(req.ID, toolName, mcpCodeTeardownUnavailable,
				"soul "+a.SID+" was NOT forgotten: the cluster-wide teardown notice could not be sent, "+
					"so a stream on another Keeper instance could not be closed; nothing was deleted, retry once Redis is reachable")
		default:
			h.deps.Logger.Error("mcp: soul.forget erase failed",
				slog.String("sid", a.SID), slog.Any("error", err))
			return h.toolError(req.ID, toolName, mcpCodeInternalError, "forget soul failed")
		}
	}

	warnings := res.Warnings
	if warnings == nil {
		warnings = []string{}
	}

	// Audit — parity with REST. It carries the full count set because after
	// this call there is nothing else left to read: every row it describes is
	// gone, so `soul.forgotten` is the only durable record that the host, its
	// seeds, its memberships and its Voices ever existed.
	h.writeAudit(audit.EventSoulForgotten, claims.Subject, map[string]any{
		"sid":                  a.SID,
		"status_before":        res.StatusBefore,
		"seeds_revoked":        res.SeedsRevoked,
		"bootstraps_burned":    res.BootstrapsBurned,
		"memberships_severed":  res.MembershipsSevered,
		"choir_voices_removed": res.ChoirVoicesRemoved,
		"local_stream_closed":  res.LocalStreamClosed,
		"broadcast":            res.Broadcast,
		"cache_keys_purged":    res.CacheKeysPurged,
		"warnings":             warnings,
	})

	for _, w := range warnings {
		h.deps.Logger.Warn("mcp: soul.forget resource not released",
			slog.String("sid", a.SID), slog.String("warning", w))
	}

	return h.toolResult(req.ID, soulForgetOutput{
		SID:                a.SID,
		StatusBefore:       res.StatusBefore,
		SeedsRevoked:       res.SeedsRevoked,
		BootstrapsBurned:   res.BootstrapsBurned,
		MembershipsSevered: res.MembershipsSevered,
		ChoirVoicesRemoved: res.ChoirVoicesRemoved,
		LocalStreamClosed:  res.LocalStreamClosed,
		Broadcast:          res.Broadcast,
		CacheKeysPurged:    res.CacheKeysPurged,
		Warnings:           warnings,
	})
}
