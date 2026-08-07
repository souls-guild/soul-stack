package api

// FULL-TYPED form of the SIGIL domain (plugins/sigils allow-list; code-first source of
// OpenAPI, ADR-054 §Pattern). ROLLOUT-BATCH-2a: allow (WRITE+AUDIT plugin.allowed),
// list (read-bare, no audit), revoke (WRITE+AUDIT plugin.revoked, a triple of path
// segments). Go types are the single source of truth (JSON Schema + validation +
// typed-output).

import (
	"net/http"

	"github.com/danielgtaylor/huma/v2"
)

// === POST /v1/plugins/sigils (allow) — WRITE+AUDIT plugin.allowed ===

// sigilAllowInput — huma input for POST /v1/plugins/sigils (FULL-TYPED). Body —
// a typed body (alias + source + ref).
type sigilAllowInput struct {
	Body PluginSigilAllowRequest
}

// PluginSigilAllowRequest — the Go form of the POST /v1/plugins/sigils body (code-first
// source of BOTH schema AND validation).
//
// The body carries the two identities NIM-377 separated. `alias` is the registration
// the operator is creating — address level 1 and the slot to read; it is NOT signed.
// `source` + `ref` are what the operator asserts about the artifact in that slot, and
// are the only identity the signature covers, because the artifact carries no self-name
// to sign instead.
//
// The reserved-name check and the alias shape are domain validation in AllowTyped (422)
// rather than schema constraints: they are one list, held in shared/plugin, and a second
// copy in a struct tag is how the two ends drift. required:"true" — missing→422;
// additionalProperties:false → unknown→400. The struct name = the contract schema name
// (huma DefaultSchemaNamer).
type PluginSigilAllowRequest struct {
	Alias  string `json:"alias" required:"true" doc:"registration alias — address level 1 (lowercase kebab-case); must not be a reserved name"`
	Source string `json:"source" required:"true" maxLength:"2048" doc:"artifact source: the git remote the module repository was fetched from (signed)"`
	Ref    string `json:"ref" required:"true" pattern:"^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$" doc:"git-tag-ref of the release (stable tag, no slashes; signed)"`
}

// sigilAllowOutput — huma output for POST /v1/plugins/sigils (FULL-TYPED). Status=201;
// Body — the native 201 body (PluginSigilAllowReply: namespace/name/ref + the sha256
// of the allowed binary, computed by the Keeper).
type sigilAllowOutput struct {
	Status int `json:"-"`
	Body   PluginSigilAllowReply
}

// sigilAllowOperation — metadata for POST /v1/plugins/sigils. Path = "/" relative to
// the chi group /v1/plugins/sigils. DefaultStatus=201. Permission plugin.allow + audit
// plugin.allowed. Errors: 400 unknown/malformed, 403 RBAC, 404 plugin-not-in-cache,
// 409 sigil-already-active, 422 triple validation, 500.
func sigilAllowOperation() huma.Operation {
	return huma.Operation{
		OperationID:   "allowPluginSigil",
		Method:        http.MethodPost,
		Path:          "/",
		Summary:       "Allow a plugin (Sigil)",
		Description:   "Approves the artifact registered under {alias} on the identity (source, ref), signing its SHA-256 and schema document (ADR-026 S4a, re-keyed by NIM-377). Permission plugin.allow. 404 — no artifact under that alias in the host cache. 409 — the alias is taken, or (source, ref) is already approved. 422 — malformed or reserved alias.",
		Tags:          []string{"plugin"},
		DefaultStatus: http.StatusCreated,
		Errors:        []int{http.StatusBadRequest, http.StatusForbidden, http.StatusNotFound, http.StatusConflict, http.StatusUnprocessableEntity, http.StatusInternalServerError},
	}
}

// === GET /v1/plugins/sigils (list) — READ-bare (no audit) ===

// sigilListInput — huma input for GET /v1/plugins/sigils. No parameters (an unfiltered
// feed) — an empty struct (parity with roleListInput).
type sigilListInput struct{}

// sigilListOutput — huma output for GET /v1/plugins/sigils (FULL-TYPED). Body — the native
// 200 body (PluginSigilListReply: items[] of active allowances without signature/manifest).
// The wire shape (items non-nil [], RevokedAt nil→omitted, allowed_at second-precision)
// is pinned by a golden-JSON snapshot test.
type sigilListOutput struct {
	Body PluginSigilListReply
}

// sigilListOperation — metadata for GET /v1/plugins/sigils. Path = "/" relative to
// the chi group /v1/plugins/sigils. DefaultStatus=200. A READ route: audit not wired.
func sigilListOperation() huma.Operation {
	return huma.Operation{
		OperationID:   "listPluginSigils",
		Method:        http.MethodGet,
		Path:          "/",
		Summary:       "List active Sigils",
		Description:   "Feed of active plugin releases (without signature/manifest, ADR-026 S4a). Permission plugin.list. Read-only, no audit.",
		Tags:          []string{"plugin"},
		DefaultStatus: http.StatusOK,
		Errors:        []int{http.StatusForbidden, http.StatusInternalServerError},
	}
}

// === DELETE /v1/plugins/sigils/{alias} (revoke) — WRITE+AUDIT plugin.revoked ===

// sigilRevokeInput — huma input for DELETE /v1/plugins/sigils/{alias}. ONE path
// segment: the alias identifies exactly one active grant
// (plugin_sigils_active_alias_idx), and it is the operator's gesture — "un-register
// this". The signed identity cannot serve here: a source is a git URL, not a path
// segment. Alias shape is domain validation in RevokeTyped. No Body.
type sigilRevokeInput struct {
	Alias string `path:"alias" pattern:"^[a-z][a-z0-9-]{0,62}$" doc:"registration alias of the grant to revoke"`
}

// sigilNoContentOutput — huma output of the 204 write route revoke. No Body (the legacy
// contract: 204 No Content). huma on an output without a Body → SetStatus(204) → empty body.
type sigilNoContentOutput struct {
	Status int `json:"-"`
}

// sigilRevokeOperation — metadata for DELETE /v1/plugins/sigils/{alias}.
// DefaultStatus=204. Permission plugin.revoke + audit plugin.revoked. Errors: 403
// RBAC, 404 sigil-not-found, 422 invalid path segment, 500.
func sigilRevokeOperation() huma.Operation {
	return huma.Operation{
		OperationID:   "revokePluginSigil",
		Method:        http.MethodDelete,
		Path:          "/{alias}",
		Summary:       "Revoke Sigil",
		Description:   "Removes the active grant registered under {alias} from the allow-list (ADR-026 S4a). Permission plugin.revoke. 404 — no active grant for that alias.",
		Tags:          []string{"plugin"},
		DefaultStatus: http.StatusNoContent,
		Errors:        []int{http.StatusForbidden, http.StatusNotFound, http.StatusUnprocessableEntity, http.StatusInternalServerError},
	}
}
