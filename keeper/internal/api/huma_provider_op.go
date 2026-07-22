package api

// FULL-TYPED shape of the PROVIDER domain (Cloud Provider CRUD, ADR-017, code is the
// OpenAPI source). Operations: create (WRITE+AUDIT provider.created), list
// (read-with-typed-query), get (read-with-path), delete (WRITE+AUDIT
// provider.deleted). No update: Provider is immutable (changing parameters =
// delete+create, protection against a partial mutation of a live cloud-spec).
//
// credentials_ref — a PATH (`vault:<path>`), input pattern `^vault:` (parity with
// MCP schemaProviderCreateInput); the secret itself is NOT resolved.

import (
	"net/http"

	"github.com/danielgtaylor/huma/v2"
)

// === POST /v1/providers (create) — WRITE+AUDIT provider.created ===

type providerCreateInput struct {
	Body ProviderCreateRequest
}

// ProviderCreateRequest — the Go shape of the POST /v1/providers body (code-first
// schema + validation). name/type — kebab (the CloudDriver plugin format); region —
// an arbitrary string; credentials_ref — a vault-ref. additionalProperties=false
// (huma default) → an unknown field → 400. Domain format validation is in
// CreateTyped (422).
type ProviderCreateRequest struct {
	Name   string `json:"name" required:"true" pattern:"^[a-z0-9-]{1,63}$" doc:"Cloud Provider name (kebab)"`
	Type   string `json:"type" required:"true" pattern:"^[a-z0-9-]{1,63}$" doc:"CloudDriver plugin name (= plugins.cloud_drivers[].name)"`
	Region string `json:"region" required:"true" doc:"provider region"`
	// credentials_ref XOR credentials (dual-mode, ADR-064): exactly one. ref — a
	// vault path (the value is NOT resolved); credentials — plaintext (keeper writes
	// it to Vault itself). The service validates format/XOR (422); pattern dropped
	// (conditional validation).
	CredentialsRef string         `json:"credentials_ref,omitempty" doc:"vault-ref to credentials (vault:<path>); XOR with credentials. Value is NOT resolved"`
	Credentials    map[string]any `json:"credentials,omitempty" doc:"opt. plaintext cloud-credentials (dual-mode, ADR-064): e.g. {access_key, secret_key}; keeper writes them to Vault itself; XOR with credentials_ref. Requires TLS front (secret_ingest.accept_plaintext)"`
	FQDNSuffix     *string        `json:"fqdn_suffix,omitempty" doc:"VM FQDN suffix (self-onboard: keeper predicts FQDN=<name>-<index>.<fqdn_suffix>). If omitted, self-onboard is unavailable"`
}

type providerCreateOutput struct {
	Status int `json:"-"`
	Body   Provider
}

func providerCreateOperation() huma.Operation {
	return huma.Operation{
		OperationID:   "createProvider",
		Method:        http.MethodPost,
		Path:          "/",
		Summary:       "Create Cloud-Provider",
		Description:   "Registers a Cloud-Provider (providers registry, ADR-017). Permission provider.create. 409 - name taken. credentials_ref is stored as a vault path, the secret is not resolved.",
		Tags:          []string{"provider"},
		DefaultStatus: http.StatusCreated,
		Errors:        []int{http.StatusBadRequest, http.StatusForbidden, http.StatusConflict, http.StatusUnprocessableEntity, http.StatusInternalServerError},
	}
}

// === GET /v1/providers (list) — READ with typed query (no audit) ===

type providerListInput struct {
	Offset int32 `query:"offset" default:"0" doc:"offset from start of set, ≥0 (out-of-range → 400)"`
	Limit  int32 `query:"limit" default:"50" doc:"page size 1..1000 (out-of-range → 400)"`
}

type providerListOutput struct {
	Body ProviderListReply
}

func providerListOperation() huma.Operation {
	return huma.Operation{
		OperationID:   "listProviders",
		Method:        http.MethodGet,
		Path:          "/",
		Summary:       "List Cloud-Providers (paged)",
		Description:   "Cloud-Provider registry with pagination (ADR-017). Permission provider.read. Read-only, no audit.",
		Tags:          []string{"provider"},
		DefaultStatus: http.StatusOK,
		Errors:        []int{http.StatusBadRequest, http.StatusForbidden, http.StatusInternalServerError},
	}
}

// === GET /v1/providers/{name} (get) — READ with path (no audit) ===

type providerGetInput struct {
	Name string `path:"name" pattern:"^[a-z0-9-]{1,63}$" doc:"Cloud Provider name"`
}

type providerGetOutput struct {
	Body Provider
}

func providerGetOperation() huma.Operation {
	return huma.Operation{
		OperationID:   "getProvider",
		Method:        http.MethodGet,
		Path:          "/{name}",
		Summary:       "Cloud-Provider detail",
		Description:   "Metadata of one Cloud-Provider by name (ADR-017). Permission provider.read. Read-only, no audit. credentials_ref is a path, the secret is not resolved.",
		Tags:          []string{"provider"},
		DefaultStatus: http.StatusOK,
		Errors:        []int{http.StatusForbidden, http.StatusNotFound, http.StatusUnprocessableEntity, http.StatusInternalServerError},
	}
}

// === DELETE /v1/providers/{name} (delete) — WRITE+AUDIT provider.deleted ===

type providerDeleteInput struct {
	Name string `path:"name" pattern:"^[a-z0-9-]{1,63}$" doc:"Cloud Provider name"`
}

// providerNoContentOutput — 204 No Content (no Body).
type providerNoContentOutput struct {
	Status int `json:"-"`
}

func providerDeleteOperation() huma.Operation {
	return huma.Operation{
		OperationID:   "deleteProvider",
		Method:        http.MethodDelete,
		Path:          "/{name}",
		Summary:       "Delete Cloud-Provider",
		Description:   "Deletes a Cloud-Provider record (ADR-017). Permission provider.delete. 404 - record not found; 409 - dependent Profiles exist (FK RESTRICT).",
		Tags:          []string{"provider"},
		DefaultStatus: http.StatusNoContent,
		Errors:        []int{http.StatusForbidden, http.StatusNotFound, http.StatusConflict, http.StatusUnprocessableEntity, http.StatusInternalServerError},
	}
}
