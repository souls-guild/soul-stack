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
	ID string `json:"id" required:"true" pattern:"^[a-z0-9-]{1,63}$" doc:"Cloud Provider id (kebab, immutable)"`
	// label is the optional display caption (ADR-0085): free text, changed later
	// by PUT /v1/providers/{id}/label. No pattern — capitals and spaces are the
	// point. Omitted → NULL, and consumers show `name`.
	Label  *string `json:"label,omitempty" doc:"Display caption: free text, may carry capitals and spaces (ADR-0085). Omitted means consumers show the name instead. Never used to derive a Vault path, an RBAC scope, a snapshot directory or a CEL root"`
	Type   string  `json:"type" required:"true" pattern:"^[a-z0-9-]{1,63}$" doc:"CloudDriver plugin name (= plugins.cloud_drivers[].name)"`
	Region string  `json:"region" required:"true" doc:"provider region"`
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

// === GET /v1/providers/{id} (get) — READ with path (no audit) ===

type providerGetInput struct {
	ID string `path:"id" pattern:"^[a-z0-9-]{1,63}$" doc:"Cloud Provider id"`
}

type providerGetOutput struct {
	Body Provider
}

func providerGetOperation() huma.Operation {
	return huma.Operation{
		OperationID:   "getProvider",
		Method:        http.MethodGet,
		Path:          "/{id}",
		Summary:       "Cloud-Provider detail",
		Description:   "Metadata of one Cloud-Provider by name (ADR-017). Permission provider.read. Read-only, no audit. credentials_ref is a path, the secret is not resolved.",
		Tags:          []string{"provider"},
		DefaultStatus: http.StatusOK,
		Errors:        []int{http.StatusForbidden, http.StatusNotFound, http.StatusUnprocessableEntity, http.StatusInternalServerError},
	}
}

// === PUT /v1/providers/{id}/label (label-set) — WRITE+AUDIT provider.label_changed ===

type providerSetLabelInput struct {
	ID   string `path:"id" pattern:"^[a-z0-9-]{1,63}$" doc:"Cloud Provider id"`
	Body LabelSetRequest
}

type providerSetLabelOutput struct {
	Body Provider
}

func providerSetLabelOperation() huma.Operation {
	return huma.Operation{
		OperationID:   "setProviderLabel",
		Method:        http.MethodPut,
		Path:          "/{id}/label",
		Summary:       "Set the Cloud-Provider display caption",
		Description:   "Replaces the display caption of one Cloud-Provider (ADR-0085). Permission provider.label-set, audit provider.label_changed. The caption is free text - capitals and spaces are allowed and nothing validates its form; null clears it and consumers fall back to showing `name`. The identifier in the path is NOT touched and has no rename operation anywhere: the caption participates in nothing derived (no Vault path, no RBAC scope, no snapshot directory, no CEL root), which is what makes changing it move nothing.",
		Tags:          []string{"provider"},
		DefaultStatus: http.StatusOK,
		Errors:        []int{http.StatusBadRequest, http.StatusForbidden, http.StatusNotFound, http.StatusUnprocessableEntity, http.StatusInternalServerError},
	}
}

// === DELETE /v1/providers/{id} (delete) — WRITE+AUDIT provider.deleted ===

type providerDeleteInput struct {
	ID string `path:"id" pattern:"^[a-z0-9-]{1,63}$" doc:"Cloud Provider id"`
}

// providerNoContentOutput — 204 No Content (no Body).
type providerNoContentOutput struct {
	Status int `json:"-"`
}

func providerDeleteOperation() huma.Operation {
	return huma.Operation{
		OperationID:   "deleteProvider",
		Method:        http.MethodDelete,
		Path:          "/{id}",
		Summary:       "Delete Cloud-Provider",
		Description:   "Deletes a Cloud-Provider record (ADR-017). Permission provider.delete. 404 - record not found; 409 - dependent Profiles exist (FK RESTRICT).",
		Tags:          []string{"provider"},
		DefaultStatus: http.StatusNoContent,
		Errors:        []int{http.StatusForbidden, http.StatusNotFound, http.StatusConflict, http.StatusUnprocessableEntity, http.StatusInternalServerError},
	}
}
