package mcp

import (
	"context"
	"encoding/json"
	"log/slog"

	"github.com/souls-guild/soul-stack/keeper/internal/jwt"
	"github.com/souls-guild/soul-stack/keeper/internal/sigil"
	"github.com/souls-guild/soul-stack/shared/audit"
	sharedhost "github.com/souls-guild/soul-stack/shared/pluginhost"
)

// sigilNotConfigured — public detail for the plugin-tools nil-guard. SigilSvc
// is an optional HandlerDeps field (production wire-up passes *sigil.Service
// ONLY when sigil.signing_key_ref is set): with nil, plugin-tools still
// dispatch but return internal-error "not configured" (mirrors the
// role-tools RBACRoles guard).
const sigilNotConfigured = "sigil is not configured"

// pluginAllowArgs — arguments for keeper.plugin.allow (schemaPluginAllowInput):
// alias + source + ref are all required.
//
// alias is the registration being created (address level 1, the slot to read) and is
// NOT signed; source + ref are what the operator asserts about the artifact in that
// slot, and are the only identity the signature covers.
type pluginAllowArgs struct {
	Alias  string `json:"alias"`
	Source string `json:"source"`
	Ref    string `json:"ref"`
}

// pluginArtifactOutput — one approved file of a release (NIM-793). os/arch/path are
// empty on a kind=git grant, whose single binary declares no platform.
type pluginArtifactOutput struct {
	OS     string `json:"os"`
	Arch   string `json:"arch"`
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
}

// pluginAllowOutput — output of keeper.plugin.allow: echoes the request + the kind and
// the artifacts the Keeper approved (parity with the REST POST /v1/plugins/sigils 201
// response). A release, not a hash: the tool confirms a release, and the caller is told
// which files that turned out to be.
type pluginAllowOutput struct {
	Alias     string                 `json:"alias"`
	Source    string                 `json:"source"`
	Ref       string                 `json:"ref"`
	Kind      string                 `json:"kind"`
	Artifacts []pluginArtifactOutput `json:"artifacts"`
}

// artifactOutputsOf projects the service's artifact rows into the tool's shape. Always
// non-nil: an approved release has files, and `null` there would read as "unknown".
func artifactOutputsOf(artifacts []sharedhost.SigilArtifact) []pluginArtifactOutput {
	out := make([]pluginArtifactOutput, 0, len(artifacts))
	for _, a := range artifacts {
		out = append(out, pluginArtifactOutput{OS: a.OS, Arch: a.Arch, Path: a.Path, SHA256: a.SHA256})
	}
	return out
}

// artifactDigests lists a release's digests for the audit row, in canonical order.
// Every one of them: recording a single digest would make the audit row a true
// statement about part of the approval and a silent omission about the rest.
func artifactDigests(artifacts []sharedhost.SigilArtifact) []string {
	out := make([]string, 0, len(artifacts))
	for _, a := range artifacts {
		out = append(out, a.SHA256)
	}
	return out
}

// callPluginAllow — mutating tool keeper.plugin.allow. A transport over
// [sigil.Service.Allow]: reading the cache slot, signing, inserting the
// record all live in Service; the tool decodes input, validates the triple,
// checks the permission, maps sentinels to MCP codes, and writes the
// plugin.allowed audit event.
//
// RBAC — plugin.allow with no selector (rbac.md: NoSelector, like
// operator.*/role.*).
func (h *Handler) callPluginAllow(ctx context.Context, claims *jwt.Claims, req jsonRPCRequest, args json.RawMessage) jsonRPCResponse {
	const toolName = "keeper.plugin.allow"

	if h.deps.SigilSvc == nil {
		return h.toolError(req.ID, toolName, mcpCodeInternalError, sigilNotConfigured)
	}

	// RBAC BEFORE unmarshal/validation (least-disclosure): an unauthorized
	// operator gets no validation feedback about the body. Context nil — the
	// permission doesn't depend on the request body.
	if err := h.deps.RBAC.Check(claims.Subject, "plugin", "allow", nil); err != nil {
		return h.toolError(req.ID, toolName, mcpCodeForbidden,
			"operator lacks required permission plugin.allow")
	}

	var a pluginAllowArgs
	if len(args) > 0 {
		if err := strictUnmarshal(args, &a); err != nil {
			return h.toolError(req.ID, toolName, mcpCodeMalformedRequest,
				"invalid arguments: "+err.Error())
		}
	}
	if msg, valid := validateSigilAllowArgs(a); !valid {
		return h.toolError(req.ID, toolName, mcpCodeValidationFailed, msg)
	}

	approved, err := h.deps.SigilSvc.Allow(ctx, sigil.AllowInput{
		Alias:     a.Alias,
		Source:    a.Source,
		Ref:       a.Ref,
		CallerAID: claims.Subject,
	})
	if err != nil {
		code, detail := mapSigilErrorToMCP(err)
		if code == mcpCodeInternalError {
			h.deps.Logger.Error("mcp: plugin.allow failed",
				slog.String("alias", a.Alias),
				slog.String("source", a.Source),
				slog.String("ref", a.Ref),
				slog.String("by_aid", claims.Subject),
				slog.Any("error", err),
			)
		}
		return h.toolError(req.ID, toolName, code, detail)
	}

	// Audit — mirrors the REST handler (supply-chain control, ADR-022): payload
	// {alias, source, ref, kind, artifact_sha256, allowed_by_aid}. The scalar `sha256`
	// key is gone rather than reused for the list — see the REST AuditPayload for why.
	// The signature and the schema (crypto material / a large document) are NOT
	// written — same as REST.
	h.writeAudit(audit.EventPluginAllowed, claims.Subject, map[string]any{
		"alias":           a.Alias,
		"source":          a.Source,
		"ref":             a.Ref,
		"kind":            approved.Kind,
		"artifact_sha256": artifactDigests(approved.Artifacts),
		"allowed_by_aid":  claims.Subject,
	})

	return h.toolResult(req.ID, pluginAllowOutput{
		Alias:     a.Alias,
		Source:    a.Source,
		Ref:       a.Ref,
		Kind:      approved.Kind,
		Artifacts: artifactOutputsOf(approved.Artifacts),
	})
}
