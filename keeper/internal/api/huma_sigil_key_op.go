package api

// FULL-TYPED form of the SIGIL-KEY domain (rotation of Sigil signing trust-anchor keys,
// ADR-026(h) R3-S7; code-first source of OpenAPI, ADR-054 §Pattern). ROLLOUT-BATCH-2a:
// introduce (WRITE+AUDIT sigil.key-introduced, 201+body), list (read-bare, WITHOUT audit),
// set-primary + retire (WRITE+AUDIT sigil.key-primary-set / sigil.key-retired, 204,
// path key_id). The Go types are the single source of truth.

import (
	"net/http"

	"github.com/danielgtaylor/huma/v2"
)

// === POST /v1/sigil/keys (introduce) — WRITE+AUDIT sigil.key-introduced ===

// sigilKeyIntroduceInput — huma-input POST /v1/sigil/keys (FULL-TYPED). Body —
// a typed body (opt. make_primary). The body is entirely optional (empty →
// make_primary=false): make_primary — `*bool omitempty` (parity with the legacy contract,
// presence-PATCH is NOT needed here — there is no omitted/null distinction, only a default false).
type sigilKeyIntroduceInput struct {
	Body SigilKeyIntroduceRequest
}

// SigilKeyIntroduceRequest — the Go form of the POST /v1/sigil/keys body. make_primary —
// an opt. flag "make the new key primary" (parity with SigilKeyIntroduceRequest).
// additionalProperties:false → unknown→400. The struct name = the contract schema name
// (huma DefaultSchemaNamer; hand-written SigilKeyIntroduceRequest, N4).
type SigilKeyIntroduceRequest struct {
	MakePrimary *bool `json:"make_primary,omitempty" doc:"make the new key primary (new Sigils are signed with it); default false"`
}

// sigilKeyIntroduceOutput — huma-output POST /v1/sigil/keys (FULL-TYPED). Status=201;
// Body — the native 201 body (SigilKeyIntroduceReply: key_id/pubkey_pem/is_primary/
// status/introduced_at). WITHOUT the private key (SENSITIVE never leaves KeyService).
type sigilKeyIntroduceOutput struct {
	Status int `json:"-"`
	Body   SigilKeyIntroduceReply
}

// sigilKeyIntroduceOperation — metadata for POST /v1/sigil/keys. Path = "/"
// relative to the chi group /v1/sigil/keys. DefaultStatus=201. Permission
// sigil.key-introduce + audit sigil.key-introduced. Errors: 400 unknown/malformed,
// 403 RBAC, 409 concurrent-primary-change, 500.
func sigilKeyIntroduceOperation() huma.Operation {
	return huma.Operation{
		OperationID:   "introduceSigilKey",
		Method:        http.MethodPost,
		Path:          "/",
		Summary:       "Introduce a Sigil signing key",
		Description:   "Generates an ed25519 pair, writes the private key into Vault, introduces the public part into the trust-anchor registry (ADR-026(h)). Permission sigil.key-introduce. Returns pubkey, NOT the private key.",
		Tags:          []string{"sigil-key"},
		DefaultStatus: http.StatusCreated,
		Errors:        []int{http.StatusBadRequest, http.StatusForbidden, http.StatusConflict, http.StatusInternalServerError},
	}
}

// === GET /v1/sigil/keys (list) — READ-bare (WITHOUT audit) ===

// sigilKeyListInput — huma-input GET /v1/sigil/keys. No parameters (active keys
// without filters) — an empty struct (parity with roleListInput).
type sigilKeyListInput struct{}

// sigilKeyListOutput — huma-output GET /v1/sigil/keys (FULL-TYPED). Body — the native
// 200 body (SigilKeyListReply: active keys, primary first, WITHOUT vault_ref).
// The wire shape (items non-nil [], introduced_at at second precision, typed status-enum)
// is pinned by a golden-JSON snapshot test.
type sigilKeyListOutput struct {
	Body SigilKeyListReply
}

// sigilKeyListOperation — metadata for GET /v1/sigil/keys. Path = "/" relative to
// the chi group /v1/sigil/keys. DefaultStatus=200. READ route: no audit attached.
func sigilKeyListOperation() huma.Operation {
	return huma.Operation{
		OperationID:   "listSigilKeys",
		Method:        http.MethodGet,
		Path:          "/",
		Summary:       "List of active Sigil signing keys",
		Description:   "Active trust-anchor signing keys (primary first, without vault_ref, ADR-026(h)). Permission sigil.key-list. Read-only, no audit.",
		Tags:          []string{"sigil-key"},
		DefaultStatus: http.StatusOK,
		Errors:        []int{http.StatusForbidden, http.StatusInternalServerError},
	}
}

// === POST /v1/sigil/keys/{key_id}/primary (set-primary) — WRITE+AUDIT sigil.key-primary-set ===

// sigilKeySetPrimaryInput — huma-input POST /v1/sigil/keys/{key_id}/primary. key_id —
// a path parameter (huma extracts it via `path:"key_id"`). The format (reSigilKeyID, 64 hex) —
// domain validation in SetPrimaryTyped (422). No Body.
type sigilKeySetPrimaryInput struct {
	KeyID string `path:"key_id" doc:"key_id of the signing key (SHA-256(SPKI), 64 hex)"`
}

// sigilKeyNoContentOutput — huma-output for the 204 write routes set-primary/retire. WITHOUT Body
// (legacy contract: 204 No Content). huma on an output without Body → SetStatus(204) → empty.
type sigilKeyNoContentOutput struct {
	Status int `json:"-"`
}

// sigilKeySetPrimaryOperation — metadata for POST /v1/sigil/keys/{key_id}/primary.
// DefaultStatus=204. Permission sigil.key-set-primary + audit sigil.key-primary-set.
// Errors: 403 RBAC, 404 key-not-found, 409 retired/concurrent-change, 422 bad key_id, 500.
func sigilKeySetPrimaryOperation() huma.Operation {
	return huma.Operation{
		OperationID:   "setPrimarySigilKey",
		Method:        http.MethodPost,
		Path:          "/{key_id}/primary",
		Summary:       "Make a signing key primary",
		Description:   "Sets an active key as primary (new Sigils are signed with it, ADR-026(h)). Permission sigil.key-set-primary. 404 - key not found; 409 - retired/race.",
		Tags:          []string{"sigil-key"},
		DefaultStatus: http.StatusNoContent,
		Errors:        []int{http.StatusForbidden, http.StatusNotFound, http.StatusConflict, http.StatusUnprocessableEntity, http.StatusInternalServerError},
	}
}

// === DELETE /v1/sigil/keys/{key_id} (retire) — WRITE+AUDIT sigil.key-retired ===

// sigilKeyRetireInput — huma-input DELETE /v1/sigil/keys/{key_id}. key_id — a path
// parameter. The format (reSigilKeyID) — domain validation in RetireTyped. No Body.
type sigilKeyRetireInput struct {
	KeyID string `path:"key_id" doc:"key_id of the signing key to retire (SHA-256(SPKI), 64 hex)"`
}

// sigilKeyRetireOperation — metadata for DELETE /v1/sigil/keys/{key_id}.
// DefaultStatus=204. Permission sigil.key-retire + audit sigil.key-retired. Errors:
// 403 RBAC, 404 key-not-found, 409 last-active/primary, 422 bad key_id, 500.
func sigilKeyRetireOperation() huma.Operation {
	return huma.Operation{
		OperationID:   "retireSigilKey",
		Method:        http.MethodDelete,
		Path:          "/{key_id}",
		Summary:       "Retire a signing key from active",
		Description:   "Marks the key retired (ADR-026(h)). Permission sigil.key-retire. 404 - no active record; 409 - last active or primary (SetPrimary to another key first).",
		Tags:          []string{"sigil-key"},
		DefaultStatus: http.StatusNoContent,
		Errors:        []int{http.StatusForbidden, http.StatusNotFound, http.StatusConflict, http.StatusUnprocessableEntity, http.StatusInternalServerError},
	}
}
