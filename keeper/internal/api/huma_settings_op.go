package api

// FULL-TYPED form of the SETTINGS domain (Keeper runtime settings overlay,
// ADR-0073; code-first OpenAPI source). GET — read (no audit, permission
// setting.read); PUT — WRITE+AUDIT setting.updated (permission setting.update);
// DELETE — WRITE+AUDIT setting.deleted (permission setting.delete).

import (
	"net/http"

	"github.com/danielgtaylor/huma/v2"
)

// === GET /v1/settings (catalog) — READ (no audit) ===

// settingsListInput — huma input GET /v1/settings. No parameters.
type settingsListInput struct{}

// settingsListOutput — huma output GET. Body — the native catalog.
type settingsListOutput struct {
	Body SettingsCatalogReply
}

func settingsListOperation() huma.Operation {
	return huma.Operation{
		OperationID:   "listSettings",
		Method:        http.MethodGet,
		Path:          "/",
		Summary:       "Keeper runtime settings catalog",
		Description:   "Every admitted runtime setting with its type, range bounds, built-in default, the EFFECTIVE value on this instance and its source (default < pg < file, ADR-0073 — the instance's own keeper.yml wins). Where the file shadows a cluster override the entry also carries cluster_value + overridden_locally. Renders the settings form without hardcoding the field list in the UI. Permission setting.read. Read-only, no audit.",
		Tags:          []string{"settings"},
		DefaultStatus: http.StatusOK,
		Errors:        []int{http.StatusForbidden, http.StatusInternalServerError},
	}
}

// === PUT /v1/settings/{key} — WRITE+AUDIT setting.updated ===

// settingPutInput — huma input PUT /v1/settings/{key}.
type settingPutInput struct {
	Key  string `path:"key" doc:"setting key from GET /v1/settings (cfg_* namespace of keeper_settings)"`
	Body SettingUpdateRequest
}

// SettingUpdateRequest — Go form of the PUT body. The value travels in its TEXT
// form, exactly as `keeper_settings` stores it, and goes through the SAME
// field-registry parse + range check the loader uses — so PUT cannot admit a
// value a reload would later reject. Struct name = contract schema name.
type SettingUpdateRequest struct {
	Value string `json:"value" required:"true" doc:"new value in text form (e.g. \"0.5\"); parsed and range-checked by the field registry"`
}

// settingPutOutput — huma output PUT. Status=200 WITH BODY (the updated entry).
type settingPutOutput struct {
	Status int `json:"-"`
	Body   SettingReply
}

func settingPutOperation() huma.Operation {
	return huma.Operation{
		OperationID:   "updateSetting",
		Method:        http.MethodPut,
		Path:          "/{key}",
		Summary:       "Override a Keeper runtime setting",
		Description:   "Writes a cluster-wide override into keeper_settings and invalidates the other Keeper nodes, which re-merge it onto their keeper.yml without a restart (ADR-0073). An instance whose own keeper.yml sets the key keeps its local value; the catalog then reports cluster_value + overridden_locally. Permission setting.update. 404 - the key is not in the registry; 422 - unparsable value, out of range, or a cross-field invariant broken either here or on a node whose file is silent about the key; nothing is written.",
		Tags:          []string{"settings"},
		DefaultStatus: http.StatusOK,
		Errors:        []int{http.StatusBadRequest, http.StatusForbidden, http.StatusNotFound, http.StatusUnprocessableEntity, http.StatusInternalServerError},
	}
}

// === DELETE /v1/settings/{key} — WRITE+AUDIT setting.deleted ===

// settingDeleteInput — huma input DELETE /v1/settings/{key}.
type settingDeleteInput struct {
	Key string `path:"key" doc:"setting key whose override is dropped"`
}

// settingDeleteOutput — huma output DELETE. Status=200 WITH BODY (the entry as
// it now reads, back on the file or default value).
type settingDeleteOutput struct {
	Status int `json:"-"`
	Body   SettingReply
}

func settingDeleteOperation() huma.Operation {
	return huma.Operation{
		OperationID:   "deleteSetting",
		Method:        http.MethodDelete,
		Path:          "/{key}",
		Summary:       "Drop a Keeper runtime setting override",
		Description:   "Removes the keeper_settings row, so the keeper.yml value (or the built-in default) is back in effect cluster-wide — the clean revert of ADR-0073(f). Permission setting.delete. 404 - the key is not in the registry, or there is no override to drop.",
		Tags:          []string{"settings"},
		DefaultStatus: http.StatusOK,
		Errors:        []int{http.StatusForbidden, http.StatusNotFound, http.StatusInternalServerError},
	}
}
