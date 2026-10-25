package api

// FULL-TYPED form of the LDAP-SYNC domain (background LDAP group sync, ADR-058
// amendment NIM-1053; code-first OpenAPI source). GET — read (no audit,
// permission setting.read); PUT — WRITE+AUDIT ldap_sync.settings_changed
// (permission setting.update).

import (
	"net/http"

	"github.com/danielgtaylor/huma/v2"
)

// === GET /v1/ldap-sync — READ (no audit) ===

type ldapSyncGetInput struct{}

type ldapSyncGetOutput struct {
	Body LDAPSyncReply
}

func ldapSyncGetOperation() huma.Operation {
	return huma.Operation{
		OperationID:   "getLDAPSync",
		Method:        http.MethodGet,
		Path:          "/",
		Summary:       "LDAP group sync settings and status",
		Description:   "Settings of the background LDAP group sync (ldap_sync in keeper_settings, ADR-058 amendment NIM-1053) and where it stands: when it last ran, when it last accepted an answer from the directory, and the active LDAP operators by the outcome of their last attempt. settings_set=false -> the key is absent and the defaults apply. status.enabled=false -> this Keeper takes no part in the sync (no auth.ldap or no Redis). Permission setting.read. Read-only, no audit.",
		Tags:          []string{"ldap-sync"},
		DefaultStatus: http.StatusOK,
		Errors:        []int{http.StatusForbidden, http.StatusInternalServerError},
	}
}

// === PUT /v1/ldap-sync — WRITE+AUDIT ldap_sync.settings_changed ===

type ldapSyncPutInput struct {
	Body LDAPSyncUpdateRequest
}

// LDAPSyncUpdateRequest — Go form of the PUT /v1/ldap-sync body: the whole
// settings object (replace semantics). Struct name = contract schema name.
type LDAPSyncUpdateRequest struct {
	Interval string `json:"interval" required:"true" doc:"how often each LDAP operator's groups are re-read, keeper.yml duration grammar (\"15m\", \"1h\"), within [1m, 24h]; LDAP being unreachable keeps the roles for one more interval"`
}

type ldapSyncPutOutput struct {
	Status int `json:"-"`
	Body   LDAPSyncReply
}

func ldapSyncPutOperation() huma.Operation {
	return huma.Operation{
		OperationID:   "updateLDAPSync",
		Method:        http.MethodPut,
		Path:          "/",
		Summary:       "Change the LDAP group sync settings",
		Description:   "Replaces the settings of the background LDAP group sync (ADR-058 amendment NIM-1053). The leader re-reads them on its next tick, within 30 seconds, and the next pass is due once the last one is an interval old; no restart. Permission setting.update. 422 - interval unparsable or outside [1m, 24h]; nothing is written.",
		Tags:          []string{"ldap-sync"},
		DefaultStatus: http.StatusOK,
		Errors:        []int{http.StatusBadRequest, http.StatusForbidden, http.StatusNotFound, http.StatusUnprocessableEntity, http.StatusInternalServerError},
	}
}
