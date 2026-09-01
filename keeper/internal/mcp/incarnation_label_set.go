package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"

	"github.com/souls-guild/soul-stack/keeper/internal/incarnation"
	"github.com/souls-guild/soul-stack/keeper/internal/jwt"
	"github.com/souls-guild/soul-stack/keeper/internal/registrylabel"
	"github.com/souls-guild/soul-stack/shared/audit"
)

// keeper.incarnation.label-set — parity with REST PUT
// /v1/incarnations/{name}/label (IncarnationHandler.SetLabelTyped, [ADR-0085]).
// Replaces the incarnation's display caption; `name` addresses the row and is
// never written.
//
// Written out rather than routed through [callLabelSet] like the other nine
// registries, for one reason: incarnation permissions are SCOPED. The shared
// helper asks `RBAC.Check(subject, resource, "label-set", nil)`, which is right
// for the NoSelector families and wrong here — this tool has to run the same
// coven/service/incarnation OR-check the REST middleware runs, or MCP would be
// the wider of the two surfaces on a mutating call.
//
// SECURITY — gate (a) only, and that asymmetry with the traits-set tool beside
// it is deliberate. Gate (b) exists there because `trait.<key>` is a live scope
// dimension: stamping a pair grants every role scoped by it sight of the
// incarnation, so holding the object does not imply holding the label. A caption
// is in no dimension of anything — not an RBAC scope, not a Vault path segment,
// not the CEL root — so there is no second question to ask. That is the whole
// reason the field was split off ([ADR-0085] THE INVARIANT).
//
// [ADR-0085]: ../../../docs/adr/0085-entity-id-and-label.md

type incarnationLabelSetArgs struct {
	Name string `json:"name"`
	// Label — the new caption. null or omitted CLEARS it, after which consumers
	// show `name` again. Free text: no pattern, no length bound, no case folding.
	Label *string `json:"label"`
}

func (h *Handler) callIncarnationLabelSet(ctx context.Context, claims *jwt.Claims, req jsonRPCRequest, args json.RawMessage) jsonRPCResponse {
	const toolName = "keeper.incarnation.label-set"

	var a incarnationLabelSetArgs
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
	// a.Label is NOT validated: free text with capitals, spaces and punctuation
	// is what the field carries.

	// RBAC OR-check over the incarnation's coven/service scope (covens ∪ {name})
	// — mirrors the REST middleware. UpdateLabel takes no lock and reads nothing
	// first, so scope is resolved via a separate probe-SelectByName (the
	// unlock/destroy/traits-set pattern).
	//
	// On a FAILED probe the fallback offers the name alone, so a role scoped
	// `incarnation=<name>` is still admitted here while the REST selector
	// ([handlers.IncarnationScopeSelector] returns nil on a read error) denies
	// it. That asymmetry is inherited from every other incarnation tool in this
	// package (unlock / destroy / traits-set / members) rather than introduced
	// here, and it grants nothing extra: the only context offered is the row the
	// caller already named, so a grant that matches it names exactly the row
	// being written. Roles scoped on coven or service ARE denied, because those
	// dimensions are unknown without the row.
	inc, probeErr := incarnation.SelectByName(ctx, h.deps.IncarnationDB, a.Name)
	if probeErr != nil {
		if scopeErr := h.checkIncarnationScope(claims, "label-set", a.Name, "", nil); scopeErr != nil {
			return h.toolError(req.ID, toolName, mcpCodeForbidden,
				"operator lacks required permission incarnation.label-set")
		}
	} else if scopeErr := h.checkIncarnationScope(claims, "label-set", inc.Name, inc.Service, inc.Covens); scopeErr != nil {
		return h.toolError(req.ID, toolName, mcpCodeForbidden,
			"operator lacks required permission incarnation.label-set")
	}

	if err := incarnation.UpdateLabel(ctx, h.deps.IncarnationDB, a.Name, a.Label); err != nil {
		if errors.Is(err, incarnation.ErrIncarnationNotFound) {
			return h.toolError(req.ID, toolName, mcpCodeNotFound,
				"incarnation "+a.Name+" not found")
		}
		h.deps.Logger.Error("mcp: incarnation.label-set failed",
			slog.String("name", a.Name),
			slog.String("by_aid", claims.Subject),
			slog.Any("error", err),
		)
		return h.toolError(req.ID, toolName, mcpCodeInternalError, "update incarnation label failed")
	}

	// Payload parity with REST (handlers.LabelWriteReply.AuditPayload): the
	// identifier addressed and the caption as it now reads, explicitly null when
	// cleared. Normalized rather than echoed, so the trail records what the row
	// holds — a caller sending "  " stored NULL and the audit must say so.
	stored := registrylabel.Normalize(a.Label)
	h.writeAudit(audit.EventIncarnationLabelChanged, claims.Subject, map[string]any{
		"name":  a.Name,
		"label": stored,
	})

	return h.toolResult(req.ID, incarnationLabelSetOutput{
		Incarnation: a.Name,
		Label:       stored,
	})
}

// incarnationLabelSetOutput — output of keeper.incarnation.label-set: which row
// was addressed and what its caption now reads. The full record stays behind
// keeper.incarnation.get, which is where state masking lives.
type incarnationLabelSetOutput struct {
	Incarnation string  `json:"incarnation"`
	Label       *string `json:"label"`
}
