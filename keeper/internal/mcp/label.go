package mcp

import (
	"context"
	"encoding/json"
	"log/slog"

	"github.com/souls-guild/soul-stack/keeper/internal/jwt"
	"github.com/souls-guild/soul-stack/keeper/internal/registrylabel"
	"github.com/souls-guild/soul-stack/shared/audit"
)

// keeper.<resource>.label-set — the MCP mirror of PUT
// /v1/<collection>/{name}/label, stated once for all ten registries ([ADR-0085],
// NIM-728).
//
// MCP is a PRIMARY operator surface (ADR-004), not a wrapper around REST, so
// each of the ten registries publishes its own tool with its own name grammar
// and its own row shape. What they share — argument shape, permission spelling,
// audit payload, the order of the checks — is written here rather than copied
// ten times, because ten copies is ten places for the audit key or the
// clear-semantics to drift apart from REST.
//
// [ADR-0085]: ../../../docs/adr/0085-entity-id-and-label.md

// labelSetArgs — arguments of every keeper.<resource>.label-set call.
//
// `label` is nullable and omitting it CLEARS the caption, exactly as the REST
// body does: there is no third state on an endpoint whose only field is the one
// being written.
type labelSetArgs struct {
	Name  string  `json:"name"`
	Label *string `json:"label"`
}

// labelSetSpec is what one registry has to say about its own label mutation:
// which tool, which permission resource, how its identifiers are spelled, how to
// perform the write, and which audit event records it.
//
// V is the registry's own output view — the row as it now reads, returned to the
// caller exactly as the read tools return it.
type labelSetSpec[V any] struct {
	// tool is the published tool name, `keeper.<resource>.label-set`.
	tool string
	// resource is the RBAC resource; the action is always "label-set".
	resource string
	// configured reports whether the registry's service is wired in. false →
	// the "not configured" internal error, as every other tool in this package.
	configured bool
	// notConfigured is the message for that case ("<registry> is not configured").
	notConfigured string
	// validName / namePattern check the IDENTIFIER in the arguments — not the
	// label. The label is free text and has no form to fail ([ADR-0085]); the
	// identifier still has to be well-formed because it addresses the row.
	validName   func(string) bool
	namePattern string
	// set performs the write and returns the row as it now reads, plus the
	// caption it held BEFORE — which the audit event records as `old_label`.
	set func(ctx context.Context, name string, label *string) (V, *string, error)
	// isNotFound recognises the registry's own not-found sentinel.
	isNotFound func(error) bool
	// notFoundf and failMsg build the two error messages.
	notFoundf func(name string) string
	failMsg   string
	// event is the `<resource>.label_changed` audit type.
	event audit.EventType
}

// callLabelSet runs one registry's label mutation.
//
// The order is the same as every other write tool here: configured → parse →
// identifier validity → permission → write → audit → result. The permission
// check sits before the write and after parsing, so a caller without
// `<resource>.label-set` learns that rather than whether the row exists.
func callLabelSet[V any](h *Handler, ctx context.Context, claims *jwt.Claims, req jsonRPCRequest, args json.RawMessage, spec labelSetSpec[V]) jsonRPCResponse {
	if !spec.configured {
		return h.toolError(req.ID, spec.tool, mcpCodeInternalError, spec.notConfigured)
	}
	var a labelSetArgs
	if len(args) > 0 {
		if err := strictUnmarshal(args, &a); err != nil {
			return h.toolError(req.ID, spec.tool, mcpCodeMalformedRequest, "invalid arguments: "+err.Error())
		}
	}
	if a.Name == "" {
		return h.toolError(req.ID, spec.tool, mcpCodeValidationFailed, "field 'name' is required")
	}
	if !spec.validName(a.Name) {
		return h.toolError(req.ID, spec.tool, mcpCodeValidationFailed, "field 'name' must match "+spec.namePattern)
	}
	// No check on a.Label on purpose: free text with capitals, spaces and
	// punctuation is what the field carries.
	if err := h.deps.RBAC.Check(claims.Subject, spec.resource, "label-set", nil); err != nil {
		return h.toolError(req.ID, spec.tool, mcpCodeForbidden,
			"operator lacks required permission "+spec.resource+".label-set")
	}
	view, previous, err := spec.set(ctx, a.Name, a.Label)
	if err != nil {
		if spec.isNotFound(err) {
			return h.toolError(req.ID, spec.tool, mcpCodeNotFound, spec.notFoundf(a.Name))
		}
		h.deps.Logger.Error("mcp: "+spec.tool+" failed", slog.String("name", a.Name), slog.Any("error", err))
		return h.toolError(req.ID, spec.tool, mcpCodeInternalError, spec.failMsg)
	}
	// Payload parity with REST (handlers.LabelWriteReply.AuditPayload): the
	// identifier addressed and the caption on both sides. The NEW value is
	// normalized rather than echoed, so the trail records what the row holds — a
	// caller sending "  " stored NULL and the audit must say so; the OLD value
	// comes off the UPDATE itself, so the pair always describes a transition that
	// really happened.
	h.writeAudit(spec.event, claims.Subject, map[string]any{
		"name":      a.Name,
		"old_label": previous,
		"new_label": registrylabel.Normalize(a.Label),
	})
	return h.toolResult(req.ID, view)
}
