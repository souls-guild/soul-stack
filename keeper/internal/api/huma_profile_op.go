package api

// FULL-TYPED shape of the PROFILE domain (Cloud Profile CRUD, ADR-017). Operations:
// create (WRITE+AUDIT profile.created), list (read-with-typed-query +
// provider filter), get (read-with-path), delete (WRITE+AUDIT profile.deleted).
// No update (parity Provider: VM-spec immutability).
//
// params — opaque VM-spec (additionalProperties:true inside, validated
// against CloudDriver.Schema at the scenario layer); cloud_init — optional userdata.

import (
	"net/http"

	"github.com/danielgtaylor/huma/v2"
)

// === POST /v1/profiles (create) — WRITE+AUDIT profile.created ===

type profileCreateInput struct {
	Body ProfileCreateRequest
}

// ProfileCreateRequest — the Go shape of the POST /v1/profiles body. name — kebab; provider
// — the name of an existing Provider (FK, 422 on missing); params — opaque VM-spec
// (optional, nil → {}); cloud_init — optional userdata.
type ProfileCreateRequest struct {
	Name      string         `json:"name" required:"true" pattern:"^[a-z0-9-]{1,63}$" doc:"Cloud Profile name (kebab)"`
	Provider  string         `json:"provider" required:"true" pattern:"^[a-z0-9-]{1,63}$" doc:"name of an existing Cloud Provider"`
	Params    map[string]any `json:"params,omitempty" doc:"opaque VM-spec (validated against CloudDriver.Schema at the scenario layer)"`
	CloudInit *string        `json:"cloud_init,omitempty" doc:"raw cloud-init userdata (optional)"`
}

type profileCreateOutput struct {
	Status int `json:"-"`
	Body   Profile
}

func profileCreateOperation() huma.Operation {
	return huma.Operation{
		OperationID:   "createProfile",
		Method:        http.MethodPost,
		Path:          "/",
		Summary:       "Create a Cloud Profile",
		Description:   "Creates a Cloud Profile (VM-spec on top of a Provider, profiles registry, ADR-017). Permission profile.create. 409 - name taken; 422 - provider doesn't exist.",
		Tags:          []string{"profile"},
		DefaultStatus: http.StatusCreated,
		Errors:        []int{http.StatusBadRequest, http.StatusForbidden, http.StatusConflict, http.StatusUnprocessableEntity, http.StatusInternalServerError},
	}
}

// === GET /v1/profiles (list) — READ with typed query (no audit) ===

type profileListInput struct {
	Provider string `query:"provider" doc:"filter by Provider name (optional)"`
	Offset   int32  `query:"offset" default:"0" doc:"offset from start of set, ≥0 (out-of-range → 400)"`
	Limit    int32  `query:"limit" default:"50" doc:"page size 1..1000 (out-of-range → 400)"`
}

type profileListOutput struct {
	Body ProfileListReply
}

func profileListOperation() huma.Operation {
	return huma.Operation{
		OperationID:   "listProfiles",
		Method:        http.MethodGet,
		Path:          "/",
		Summary:       "List Cloud Profiles (paged)",
		Description:   "Registry of Cloud Profiles with pagination and provider filter (ADR-017). Permission profile.read. Read-only, no audit.",
		Tags:          []string{"profile"},
		DefaultStatus: http.StatusOK,
		Errors:        []int{http.StatusBadRequest, http.StatusForbidden, http.StatusInternalServerError},
	}
}

// === GET /v1/profiles/{name} (get) — READ with path (no audit) ===

type profileGetInput struct {
	Name string `path:"name" pattern:"^[a-z0-9-]{1,63}$" doc:"Cloud Profile name"`
}

type profileGetOutput struct {
	Body Profile
}

func profileGetOperation() huma.Operation {
	return huma.Operation{
		OperationID:   "getProfile",
		Method:        http.MethodGet,
		Path:          "/{name}",
		Summary:       "Cloud Profile card",
		Description:   "Metadata of a single Cloud Profile by name (ADR-017). Permission profile.read. Read-only, no audit.",
		Tags:          []string{"profile"},
		DefaultStatus: http.StatusOK,
		Errors:        []int{http.StatusForbidden, http.StatusNotFound, http.StatusUnprocessableEntity, http.StatusInternalServerError},
	}
}

// === DELETE /v1/profiles/{name} (delete) — WRITE+AUDIT profile.deleted ===

type profileDeleteInput struct {
	Name string `path:"name" pattern:"^[a-z0-9-]{1,63}$" doc:"Cloud Profile name"`
}

// profileNoContentOutput — 204 No Content (no Body).
type profileNoContentOutput struct {
	Status int `json:"-"`
}

func profileDeleteOperation() huma.Operation {
	return huma.Operation{
		OperationID:   "deleteProfile",
		Method:        http.MethodDelete,
		Path:          "/{name}",
		Summary:       "Delete a Cloud Profile",
		Description:   "Deletes a Cloud Profile record (ADR-017). Permission profile.delete. 404 - record absent.",
		Tags:          []string{"profile"},
		DefaultStatus: http.StatusNoContent,
		Errors:        []int{http.StatusForbidden, http.StatusNotFound, http.StatusUnprocessableEntity, http.StatusInternalServerError},
	}
}
