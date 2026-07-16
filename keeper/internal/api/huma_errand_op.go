package api

// FULL-TYPED form of the ERRAND domain (list + get + cancel; code-first source of OpenAPI,
// ADR-054 §Pattern). ROLLOUT BATCH 2c (errand read+cancel on huma over the augur/
// audit-endpoint references): list — read with typed query (started_after date-time→400,
// offset/limit→400, status enum, sid/module string/array); get — read with path
// (200 ErrandResult / 202 running); cancel — WRITE+AUDIT (errand.cancelled).
// Go types are the single source of truth (JSON Schema + validation + typed output).

import (
	"encoding/json"
	"net/http"
	"time"

	"github.com/danielgtaylor/huma/v2"
)

// === GET /v1/errands (list) — READ with typed query (no audit) ===

// errandListInput — huma-input GET /v1/errands (FULL-TYPED typed-query). offset/limit —
// int32 with a default (parity ParsePage; out-of-range → 400 via CheckPageBounds, NOT
// huma minimum/maximum). started_after — time.Time (date-time): bad value → 400 on
// huma-bind BEFORE delegation (ADR-051 Amendment 2026-06-10 — the single source, the former
// domain 422 is unreachable through the router). status — a closed-set enum (a value outside
// the set → 422). sid — a string filter (the FQDN format is validated by the domain → 422). module —
// multi-value exact-match OR (?module=X&module=Y; explode form).
type errandListInput struct {
	SID          string    `query:"sid" doc:"фильтр по целевому Soul (FQDN); битый formт → 422"`
	Status       string    `query:"status" enum:"running,success,failed,timed_out,cancelled,module_not_allowed" doc:"фильтр по статусу Errand-а; зonчение вне enum → 422"`
	StartedAfter time.Time `query:"started_after" doc:"фильтр по onчалу (started_at > value, RFC3339); bad value → 400"`
	Modules      []string  `query:"module" explode:"true" doc:"multi-value exact-match OR по имени модуля (?module=X&module=Y)"`
	Offset       int32     `query:"offset" default:"0" doc:"offset from start of set, ≥0 (matches shared/api.ParsePage; out-of-range → 400)"`
	Limit        int32     `query:"limit" default:"50" doc:"page size 1..1000 (matches shared/api.ParsePage; out-of-range → 400)"`
}

// errandListOutput — huma-output GET /v1/errands (FULL-TYPED). Body — a typed
// 200 envelope (native api.ErrandListReply: items/offset/limit/total; element
// api.ErrandResult — a projection of the flat handlers.ErrandResultView in the register function).
// The wire shape of items (status a bare enum string, started_at/finished_at second-precision
// RFC3339 UTC) is pinned by a golden-JSON byte-exact test.
type errandListOutput struct {
	Body ErrandListReply
}

// errandListOperation — metadata for GET /v1/errands. Path = "/errands" relative to the
// chi group /v1 (the full under-/v1 path — a distinct path for spec-dump). DefaultStatus=200.
// READ route: audit not wired. Errors: 400 (out-of-range pagination / bad started_after),
// 403 RBAC, 422 (bad sid format / bad status enum), 500.
func errandListOperation() huma.Operation {
	return huma.Operation{
		OperationID:   "listErrands",
		Method:        http.MethodGet,
		Path:          "/errands",
		Summary:       "Спиwithк Errand-ов (paged)",
		Description:   "Реестр Errand-ов с фильтрами и пагиonцией (ADR-033). Permission errand.list. Read-only, no audit.",
		Tags:          []string{"errand"},
		DefaultStatus: http.StatusOK,
		Errors:        []int{http.StatusBadRequest, http.StatusForbidden, http.StatusUnprocessableEntity, http.StatusInternalServerError},
	}
}

// === GET /v1/errands/{errand_id} (get) — READ with path (no audit) ===

// errandGetInput — huma-input GET /v1/errands/{errand_id}. ErrandID — a path parameter
// (ULID; empty → 422 in GetTyped).
type errandGetInput struct {
	ErrandID string `path:"errand_id" doc:"ULID of Errand"`
}

// errandGetOutput — huma-output GET /v1/errands/{errand_id} with TWO success codes
// under a single OperationID (200 terminal ErrandResult / 202 running ErrandAccepted —
// different bodies). Status — the huma field convention (response-code override: the handler sets
// 200 or 202). Body — json.RawMessage: the handler pre-marshals the chosen body (its
// schema in the huma fragment = `{}`, NOT octet-stream — rawMessageType → an empty Schema;
// the committed openapi.yaml carries the typed 200/ErrandResult + 202/ErrandAccepted,
// that is the authority, the fragment only duplicates the op-id/path for the drift test). The wire body
// the register function marshals from the native projection of the flat domain view
// (newErrandResult / newErrandAccepted) — the bytes are identical to legacy.
type errandGetOutput struct {
	Status int             `json:"-"`
	Body   json.RawMessage `json:"body"`
}

// errandGetOperation — metadata for GET /v1/errands/{errand_id}. DefaultStatus=200 (terminal).
// 202 (running) — an additional success code (declared in the Errors set to document the
// contract; the huma handler itself sets SetStatus(202) on running). READ route: audit not
// wired. Permission errand.list. Errors: 202 running, 403 RBAC, 404 not-found, 422 bad id, 500.
func errandGetOperation() huma.Operation {
	return huma.Operation{
		OperationID:   "getErrand",
		Method:        http.MethodGet,
		Path:          "/errands/{errand_id}",
		Summary:       "Состояние Errand-а",
		Description:   "Термиonл-string (200) либо running-poll (202) по ULID (ADR-033). Permission errand.list. Read-only, no audit.",
		Tags:          []string{"errand"},
		DefaultStatus: http.StatusOK,
		Errors:        []int{http.StatusAccepted, http.StatusForbidden, http.StatusNotFound, http.StatusUnprocessableEntity, http.StatusInternalServerError},
	}
}

// === DELETE /v1/errands/{errand_id} (cancel) — WRITE+AUDIT errand.cancelled ===

// errandCancelInput — huma-input DELETE /v1/errands/{errand_id}. ErrandID — path. No Body.
type errandCancelInput struct {
	ErrandID string `path:"errand_id" doc:"ULID of Errand"`
}

// errandNoContentOutput — huma-output of the errand 204 cancel route. No Body (legacy
// contract: 204 No Content). huma on an output without a Body does SetStatus(204) → empty body.
type errandNoContentOutput struct {
	Status int `json:"-"`
}

// errandCancelOperation — metadata for DELETE /v1/errands/{errand_id}. DefaultStatus=204.
// Permission errand.cancel + audit errand.cancelled. Errors: 403 RBAC, 404 not-found
// (errand / soul-not-connected), 409 terminal (nothing to cancel), 422 empty id, 500.
func errandCancelOperation() huma.Operation {
	return huma.Operation{
		OperationID:   "cancelErrand",
		Method:        http.MethodDelete,
		Path:          "/errands/{errand_id}",
		Summary:       "Отменить Errand",
		Description:   "Отправляет cancel-сигonл Soul-у (ADR-033, slice E5). Permission errand.cancel. 409 — already термиonл.",
		Tags:          []string{"errand"},
		DefaultStatus: http.StatusNoContent,
		Errors:        []int{http.StatusForbidden, http.StatusNotFound, http.StatusConflict, http.StatusUnprocessableEntity, http.StatusInternalServerError},
	}
}
