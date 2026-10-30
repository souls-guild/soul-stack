package api

// FULL-TYPED form of an incarnation's maintenance windows and freezes (code-first
// OpenAPI source, ADR-054 §Pattern; epic NIM-1069, NIM-1072). Sub-resources of an
// incarnation, the members layout: each huma op carries the FULL path relative to
// the /v1/incarnations group.
//
// Audit class: WRITE-SELF-AUDIT for the six writes (maintenance_window.* / freeze.*
// written by the handler ITSELF) — audit middleware is NOT wired on these routes.
// The two reads write no audit.

import (
	"net/http"

	"github.com/danielgtaylor/huma/v2"
)

const maintenanceSetNote = " Permission incarnation.maintenance-set, the same incarnation scope as every other incarnation mutation; allowed in any incarnation status, since a window only decides when a FUTURE run may start."

var maintenanceWriteErrors = []int{http.StatusForbidden, http.StatusNotFound, http.StatusUnprocessableEntity, http.StatusInternalServerError}

// === GET /v1/incarnations/{id}/maintenance-windows — READ (no audit) ===

type maintenanceWindowListInput struct {
	ID string `path:"id" doc:"incarnation id"`
}

type maintenanceWindowListOutput struct {
	Body MaintenanceWindowListReply
}

func maintenanceWindowListOperation() huma.Operation {
	return huma.Operation{
		OperationID: "listIncarnationMaintenanceWindows",
		Method:      http.MethodGet,
		Path:        "/{id}/maintenance-windows",
		Summary:     "List the incarnation's maintenance windows",
		Description: "Every maintenance window of the incarnation in creation order, the cluster default `defaults` when it applies (the runtime setting maintenance.default_windows, for an incarnation with no weekly window of its own; its once windows add to it), plus `status`: whether a run may start now (`open_now`), the first instant it may (`next_start`, now itself when open), the freeze in force (`active_freeze`) and where the windows in effect come from (`source`: own, default or none). The status is computed over the windows in effect AND the freezes: no windows in effect means any time, and a freeze closes everything while it lasts. A `once` window counts only until its `ends_at`; one that has ended is still listed until the Reaper deletes it after `maintenance.window_retention`, and an incarnation whose only windows have ended is open at any time unless the default holds it. A window gates the start of a run; a started run is never cut off when the slot ends. Permission incarnation.get. Read-only, no audit.",
		Tags:        []string{"incarnation"},
		Errors:      []int{http.StatusForbidden, http.StatusNotFound, http.StatusUnprocessableEntity, http.StatusInternalServerError},
	}
}

// === POST /v1/incarnations/{id}/maintenance-windows — WRITE-SELF-AUDIT maintenance_window.created (201) ===

type maintenanceWindowCreateInput struct {
	ID   string `path:"id" doc:"incarnation id"`
	Body MaintenanceWindowRequest
}

type maintenanceWindowWriteOutput struct {
	Body MaintenanceWindow
}

func maintenanceWindowCreateOperation() huma.Operation {
	return huma.Operation{
		OperationID:   "createIncarnationMaintenanceWindow",
		Method:        http.MethodPost,
		Path:          "/{id}/maintenance-windows",
		Summary:       "Add a maintenance window to an incarnation",
		Description:   "Adds a window. `kind: weekly` (the default when `kind` is omitted) is a slot set: on each listed day a slot starts at start_time in timezone and lasts duration_minutes (may cross midnight). `kind: once` is one interval [starts_at, ends_at). Each kind takes only its own fields: a field of the other kind is 422 naming it. 422 also on any other refused field or when the incarnation already has 50 windows of both kinds together. Audit maintenance_window.created." + maintenanceSetNote,
		Tags:          []string{"incarnation"},
		DefaultStatus: http.StatusCreated,
		Errors:        maintenanceWriteErrors,
	}
}

// === PUT /v1/incarnations/{id}/maintenance-windows/{window_id} — WRITE-SELF-AUDIT maintenance_window.updated (200) ===

type maintenanceWindowUpdateInput struct {
	ID       string `path:"id" doc:"incarnation id"`
	WindowID string `path:"window_id" doc:"maintenance window id (UUID)"`
	Body     MaintenanceWindowRequest
}

func maintenanceWindowUpdateOperation() huma.Operation {
	return huma.Operation{
		OperationID: "updateIncarnationMaintenanceWindow",
		Method:      http.MethodPut,
		Path:        "/{id}/maintenance-windows/{window_id}",
		Summary:     "Replace a maintenance window",
		Description: "Replaces every field of the window, its kind included; an omitted `kind` is `weekly`. 404 when no window with that id belongs to this incarnation. Audit maintenance_window.updated, with the fields before the change under `previous`." + maintenanceSetNote,
		Tags:        []string{"incarnation"},
		Errors:      maintenanceWriteErrors,
	}
}

// === DELETE /v1/incarnations/{id}/maintenance-windows/{window_id} — WRITE-SELF-AUDIT maintenance_window.deleted (204) ===

type maintenanceWindowDeleteInput struct {
	ID       string `path:"id" doc:"incarnation id"`
	WindowID string `path:"window_id" doc:"maintenance window id (UUID)"`
}

type maintenanceDeleteOutput struct {
	Status int `json:"-"`
}

func maintenanceWindowDeleteOperation() huma.Operation {
	return huma.Operation{
		OperationID:   "deleteIncarnationMaintenanceWindow",
		Method:        http.MethodDelete,
		Path:          "/{id}/maintenance-windows/{window_id}",
		Summary:       "Delete a maintenance window",
		Description:   "Deletes the window. Not idempotent: deleting it again is 404. Deleting the last window that counts makes the incarnation open at any time outside a freeze, or held to the cluster default when it is set. Audit maintenance_window.deleted." + maintenanceSetNote,
		Tags:          []string{"incarnation"},
		DefaultStatus: http.StatusNoContent,
		Errors:        maintenanceWriteErrors,
	}
}

// === GET /v1/incarnations/{id}/freezes — READ (no audit) ===

type freezeListInput struct {
	ID string `path:"id" doc:"incarnation id"`
}

type freezeListOutput struct {
	Body FreezeListReply
}

func freezeListOperation() huma.Operation {
	return huma.Operation{
		OperationID: "listIncarnationFreezes",
		Method:      http.MethodGet,
		Path:        "/{id}/freezes",
		Summary:     "List the incarnation's freezes",
		Description: "Every freeze of the incarnation, past ones included until the Reaper deletes them after `maintenance.freeze_retention`, ordered by start. During a freeze no automatic run starts, with or without windows; a forced run needs explicit consent. Permission incarnation.get. Read-only, no audit.",
		Tags:        []string{"incarnation"},
		Errors:      []int{http.StatusForbidden, http.StatusNotFound, http.StatusUnprocessableEntity, http.StatusInternalServerError},
	}
}

// === POST /v1/incarnations/{id}/freezes — WRITE-SELF-AUDIT freeze.created (201) ===

type freezeCreateInput struct {
	ID   string `path:"id" doc:"incarnation id"`
	Body FreezeRequest
}

type freezeWriteOutput struct {
	Body Freeze
}

func freezeCreateOperation() huma.Operation {
	return huma.Operation{
		OperationID:   "createIncarnationFreeze",
		Method:        http.MethodPost,
		Path:          "/{id}/freezes",
		Summary:       "Add a freeze to an incarnation",
		Description:   "Adds the freeze [starts_at, ends_at). 422 when ends_at is not after starts_at or the incarnation already has 200 freezes. Audit freeze.created." + maintenanceSetNote,
		Tags:          []string{"incarnation"},
		DefaultStatus: http.StatusCreated,
		Errors:        maintenanceWriteErrors,
	}
}

// === PUT /v1/incarnations/{id}/freezes/{freeze_id} — WRITE-SELF-AUDIT freeze.updated (200) ===

type freezeUpdateInput struct {
	ID       string `path:"id" doc:"incarnation id"`
	FreezeID string `path:"freeze_id" doc:"freeze id (UUID)"`
	Body     FreezeRequest
}

func freezeUpdateOperation() huma.Operation {
	return huma.Operation{
		OperationID: "updateIncarnationFreeze",
		Method:      http.MethodPut,
		Path:        "/{id}/freezes/{freeze_id}",
		Summary:     "Replace a freeze",
		Description: "Replaces every field of the freeze. 404 when no freeze with that id belongs to this incarnation. Audit freeze.updated, with the fields before the change under `previous`." + maintenanceSetNote,
		Tags:        []string{"incarnation"},
		Errors:      maintenanceWriteErrors,
	}
}

// === DELETE /v1/incarnations/{id}/freezes/{freeze_id} — WRITE-SELF-AUDIT freeze.deleted (204) ===

type freezeDeleteInput struct {
	ID       string `path:"id" doc:"incarnation id"`
	FreezeID string `path:"freeze_id" doc:"freeze id (UUID)"`
}

func freezeDeleteOperation() huma.Operation {
	return huma.Operation{
		OperationID:   "deleteIncarnationFreeze",
		Method:        http.MethodDelete,
		Path:          "/{id}/freezes/{freeze_id}",
		Summary:       "Delete a freeze",
		Description:   "Deletes the freeze. Not idempotent: deleting it again is 404. Audit freeze.deleted." + maintenanceSetNote,
		Tags:          []string{"incarnation"},
		DefaultStatus: http.StatusNoContent,
		Errors:        maintenanceWriteErrors,
	}
}
