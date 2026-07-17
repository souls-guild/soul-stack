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
// a typed body (the triple namespace/name/ref).
type sigilAllowInput struct {
	Body PluginSigilAllowRequest
}

// PluginSigilAllowRequest — the Go form of the POST /v1/plugins/sigils body (code-first source
// of BOTH schema AND validation). Mirrors the domain PluginSigilAllowRequest: the triple of a
// supply-chain allowance (namespace/name/ref). Segment format (reSigilSegment) is domain
// validation in AllowTyped (422), not the huma schema. required:"true" —
// missing→422; additionalProperties:false → unknown→400. The struct name = the contract
// schema name (huma DefaultSchemaNamer; hand-written spec PluginSigilAllowRequest, N4).
type PluginSigilAllowRequest struct {
	Namespace string `json:"namespace" required:"true" pattern:"^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$" doc:"plugin namespace (type — cloud/ssh/mod)"`
	Name      string `json:"name" required:"true" pattern:"^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$" doc:"plugin name (as in manifest.name)"`
	Ref       string `json:"ref" required:"true" pattern:"^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$" doc:"git-tag-ref of the release (stable tag, no slashes)"`
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
		Description:   "Registers (namespace,name,ref) in the plugin integrity allow-list with SHA-256 signature (ADR-026 S4a). Permission plugin.allow. 404 — plugin not in the host cache. 409 — release already active.",
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

// === DELETE /v1/plugins/sigils/{namespace}/{name}/{ref} (revoke) — WRITE+AUDIT plugin.revoked ===

// sigilRevokeInput — huma input for DELETE /v1/plugins/sigils/{namespace}/{name}/{ref}.
// Three path segments (huma extracts them by `path:"…"`). Segment format
// (reSigilSegment, a slash in ref → 422) is domain validation in RevokeTyped. No Body.
type sigilRevokeInput struct {
	Namespace string `path:"namespace" pattern:"^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$" doc:"plugin namespace"`
	Name      string `path:"name" pattern:"^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$" doc:"plugin name"`
	Ref       string `path:"ref" pattern:"^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$" doc:"git-tag-ref of the release"`
}

// sigilNoContentOutput — huma output of the 204 write route revoke. No Body (the legacy
// contract: 204 No Content). huma on an output without a Body → SetStatus(204) → empty body.
type sigilNoContentOutput struct {
	Status int `json:"-"`
}

// sigilRevokeOperation — metadata for DELETE /v1/plugins/sigils/{namespace}/{name}/{ref}.
// DefaultStatus=204. Permission plugin.revoke + audit plugin.revoked. Errors: 403
// RBAC, 404 sigil-not-found, 422 invalid path segment, 500.
func sigilRevokeOperation() huma.Operation {
	return huma.Operation{
		OperationID:   "revokePluginSigil",
		Method:        http.MethodDelete,
		Path:          "/{namespace}/{name}/{ref}",
		Summary:       "Revoke Sigil",
		Description:   "Removes the active release (namespace,name,ref) from allow-list (ADR-026 S4a). Permission plugin.revoke. 404 — active record absent.",
		Tags:          []string{"plugin"},
		DefaultStatus: http.StatusNoContent,
		Errors:        []int{http.StatusForbidden, http.StatusNotFound, http.StatusUnprocessableEntity, http.StatusInternalServerError},
	}
}
