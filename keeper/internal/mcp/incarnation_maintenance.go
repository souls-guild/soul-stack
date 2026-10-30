package mcp

// keeper.incarnation.maintenance-window.list / .create / .update / .delete and
// keeper.incarnation.freeze.list / .create / .update / .delete — parity with REST
// /v1/incarnations/{id}/maintenance-windows[/{window_id}] and /freezes[/{freeze_id}]
// (epic NIM-1069, NIM-1072).
//
// The writes ask incarnation.maintenance-set and the reads incarnation.get, each
// through the body-scoped OR-Check over the incarnation's scope
// ([Handler.checkIncarnationScope]) that the REST middleware applies there.
// Validation, storage and the audit payload are keeper/internal/maintenance's,
// shared with REST, so the two surfaces cannot accept or record different things.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"slices"
	"time"

	"github.com/souls-guild/soul-stack/keeper/internal/incarnation"
	"github.com/souls-guild/soul-stack/keeper/internal/jwt"
	"github.com/souls-guild/soul-stack/keeper/internal/maintenance"
	"github.com/souls-guild/soul-stack/shared/audit"
)

// Schema fragments the manifest assembles the eight tools' schemas from, so the
// create and update inputs cannot describe a window two ways.
const (
	maintenanceIDPattern = `^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`

	maintenanceWindowFieldsSchema = `"kind":{"type":"string","enum":["weekly","once"],"description":"weekly (the default when omitted) or once. Each kind takes only its own fields; a field of the other kind is refused with code=validation-failed naming it."},
"days":{"type":"array","minItems":1,"maxItems":7,"uniqueItems":true,"items":{"type":"string","enum":["mon","tue","wed","thu","fri","sat","sun"]},"description":"weekly, required: weekdays a slot starts on."},
"start_time":{"type":"string","pattern":"^([01][0-9]|2[0-3]):[0-5][0-9]$","description":"weekly, required: wall-clock start of each slot, HH:MM, in timezone."},
"duration_minutes":{"type":"integer","minimum":1,"maximum":1440,"description":"weekly, required: slot length in minutes of elapsed time, at most a day."},
"timezone":{"type":"string","minLength":1,"maxLength":64,"description":"weekly, required: IANA zone, e.g. Europe/Moscow or UTC; names that depend on the Keeper host (Local, localtime, posix/...) are refused."},
"starts_at":{"type":"string","description":"once, required: start of the window, RFC 3339, inclusive."},
"ends_at":{"type":"string","description":"once, required: end of the window, RFC 3339, exclusive; after starts_at."},
"comment":{"type":"string","maxLength":500,"description":"Free text, at most 500 characters."}`

	maintenanceWindowEntrySchema = `"type":"object","additionalProperties":false,"required":["id","kind","comment","created_at","updated_at"],"properties":{
"id":{"type":"string","description":"Window id (UUID)."},
"kind":{"type":"string","enum":["weekly","once"]},
"days":{"type":"array","items":{"type":"string"},"description":"weekly: weekdays a slot starts on, in calendar order."},
"start_time":{"type":"string","description":"weekly."},
"duration_minutes":{"type":"integer","description":"weekly."},
"timezone":{"type":"string","description":"weekly."},
"starts_at":{"type":"string","description":"once: RFC3339, inclusive."},
"ends_at":{"type":"string","description":"once: RFC3339, exclusive. Listed after it has passed until the Reaper deletes the window; it no longer counts."},
"comment":{"type":"string"},
"created_at":{"type":"string","description":"RFC3339."},
"updated_at":{"type":"string","description":"RFC3339."}}`

	freezeFieldsSchema = `"starts_at":{"type":"string","description":"Start of the freeze, RFC 3339, inclusive."},
"ends_at":{"type":"string","description":"End of the freeze, RFC 3339, exclusive; after starts_at."},
"comment":{"type":"string","maxLength":500,"description":"Free text, at most 500 characters."}`

	freezeEntrySchema = `"type":"object","additionalProperties":false,"required":["id","starts_at","ends_at","comment","created_at","updated_at"],"properties":{
"id":{"type":"string","description":"Freeze id (UUID)."},
"starts_at":{"type":"string","description":"RFC3339."},
"ends_at":{"type":"string","description":"RFC3339."},
"comment":{"type":"string"},
"created_at":{"type":"string","description":"RFC3339."},
"updated_at":{"type":"string","description":"RFC3339."}}`
)

type maintenanceWindowEntry struct {
	ID              string   `json:"id"`
	Kind            string   `json:"kind"`
	Days            []string `json:"days,omitempty"`
	StartTime       string   `json:"start_time,omitempty"`
	DurationMinutes int      `json:"duration_minutes,omitempty"`
	Timezone        string   `json:"timezone,omitempty"`
	StartsAt        string   `json:"starts_at,omitempty"`
	EndsAt          string   `json:"ends_at,omitempty"`
	Comment         string   `json:"comment"`
	CreatedAt       string   `json:"created_at"`
	UpdatedAt       string   `json:"updated_at"`
}

type freezeEntry struct {
	ID        string `json:"id"`
	StartsAt  string `json:"starts_at"`
	EndsAt    string `json:"ends_at"`
	Comment   string `json:"comment"`
	CreatedAt string `json:"created_at"`
	UpdatedAt string `json:"updated_at"`
}

type maintenanceStatusEntry struct {
	OpenNow      bool         `json:"open_now"`
	NextStart    string       `json:"next_start"`
	ActiveFreeze *freezeEntry `json:"active_freeze,omitempty"`
}

type maintenanceWindowListOutput struct {
	Incarnation string                   `json:"incarnation"`
	Items       []maintenanceWindowEntry `json:"items"`
	Status      maintenanceStatusEntry   `json:"status"`
}

type freezeListOutput struct {
	Incarnation string        `json:"incarnation"`
	Items       []freezeEntry `json:"items"`
}

type maintenanceDeleteOutput struct {
	Incarnation string `json:"incarnation"`
	ID          string `json:"id"`
}

type maintenanceListArgs struct {
	ID string `json:"id"`
}

type maintenanceWindowWriteArgs struct {
	ID              string   `json:"id"`
	WindowID        string   `json:"window_id"`
	Kind            string   `json:"kind"`
	Days            []string `json:"days"`
	StartTime       string   `json:"start_time"`
	DurationMinutes int      `json:"duration_minutes"`
	Timezone        string   `json:"timezone"`
	StartsAt        string   `json:"starts_at"`
	EndsAt          string   `json:"ends_at"`
	Comment         string   `json:"comment"`
}

type maintenanceWindowDeleteArgs struct {
	ID       string `json:"id"`
	WindowID string `json:"window_id"`
}

type freezeWriteArgs struct {
	ID       string `json:"id"`
	FreezeID string `json:"freeze_id"`
	StartsAt string `json:"starts_at"`
	EndsAt   string `json:"ends_at"`
	Comment  string `json:"comment"`
}

type freezeDeleteArgs struct {
	ID       string `json:"id"`
	FreezeID string `json:"freeze_id"`
}

func (h *Handler) callMaintenanceWindowList(ctx context.Context, claims *jwt.Claims, req jsonRPCRequest, args json.RawMessage) jsonRPCResponse {
	const toolName = "keeper.incarnation.maintenance-window.list"
	var a maintenanceListArgs
	if resp, ok := h.decodeMaintenanceArgs(req, toolName, args, &a); !ok {
		return resp
	}
	if resp, ok := h.authorizeMaintenance(ctx, claims, req, toolName, "get", a.ID); !ok {
		return resp
	}
	windows, err := maintenance.ListWindows(ctx, h.deps.IncarnationDB, a.ID)
	if err != nil {
		return h.maintenanceToolError(req, toolName, a.ID, "", "", err)
	}
	freezes, err := maintenance.ListFreezes(ctx, h.deps.IncarnationDB, a.ID)
	if err != nil {
		return h.maintenanceToolError(req, toolName, a.ID, "", "", err)
	}
	calc, err := maintenance.NewCalculator(windows, freezes)
	if err != nil {
		return h.maintenanceToolError(req, toolName, a.ID, "", "", err)
	}
	st := calc.Status(time.Now())
	out := maintenanceWindowListOutput{
		Incarnation: a.ID,
		Items:       make([]maintenanceWindowEntry, 0, len(windows)),
		Status:      maintenanceStatusEntry{OpenNow: st.OpenNow, NextStart: rfc3339(st.NextStart)},
	}
	for _, w := range windows {
		out.Items = append(out.Items, newMaintenanceWindowEntry(w))
	}
	if st.ActiveFreeze != nil {
		f := newFreezeEntry(*st.ActiveFreeze)
		out.Status.ActiveFreeze = &f
	}
	return h.toolResult(req.ID, out)
}

func (h *Handler) callMaintenanceWindowCreate(ctx context.Context, claims *jwt.Claims, req jsonRPCRequest, args json.RawMessage) jsonRPCResponse {
	const toolName = "keeper.incarnation.maintenance-window.create"
	var a maintenanceWindowWriteArgs
	if resp, ok := h.decodeMaintenanceArgs(req, toolName, args, &a); !ok {
		return resp
	}
	if resp, ok := h.authorizeMaintenance(ctx, claims, req, toolName, "maintenance-set", a.ID); !ok {
		return resp
	}
	if a.WindowID != "" {
		return h.toolError(req.ID, toolName, mcpCodeValidationFailed, "field 'window_id' is assigned by the Keeper; omit it on create")
	}
	spec, err := a.spec(args)
	if err != nil {
		return h.toolError(req.ID, toolName, mcpCodeValidationFailed, err.Error())
	}
	w, err := maintenance.InsertWindow(ctx, h.deps.IncarnationDB, a.ID, spec)
	if err != nil {
		return h.maintenanceToolError(req, toolName, a.ID, "maintenance window", "", err)
	}
	h.writeAudit(ctx, audit.EventMaintenanceWindowCreated, claims.Subject, maintenance.WindowAuditPayload(w, nil))
	return h.toolResult(req.ID, newMaintenanceWindowEntry(w))
}

func (h *Handler) callMaintenanceWindowUpdate(ctx context.Context, claims *jwt.Claims, req jsonRPCRequest, args json.RawMessage) jsonRPCResponse {
	const toolName = "keeper.incarnation.maintenance-window.update"
	var a maintenanceWindowWriteArgs
	if resp, ok := h.decodeMaintenanceArgs(req, toolName, args, &a); !ok {
		return resp
	}
	if resp, ok := h.authorizeMaintenance(ctx, claims, req, toolName, "maintenance-set", a.ID); !ok {
		return resp
	}
	if !maintenance.ValidID(a.WindowID) {
		return h.toolError(req.ID, toolName, mcpCodeValidationFailed, "field 'window_id' must be a UUID")
	}
	spec, err := a.spec(args)
	if err != nil {
		return h.toolError(req.ID, toolName, mcpCodeValidationFailed, err.Error())
	}
	before, after, err := maintenance.UpdateWindow(ctx, h.deps.IncarnationDB, a.ID, a.WindowID, spec)
	if err != nil {
		return h.maintenanceToolError(req, toolName, a.ID, "maintenance window", a.WindowID, err)
	}
	h.writeAudit(ctx, audit.EventMaintenanceWindowUpdated, claims.Subject, maintenance.WindowAuditPayload(after, &before))
	return h.toolResult(req.ID, newMaintenanceWindowEntry(after))
}

func (h *Handler) callMaintenanceWindowDelete(ctx context.Context, claims *jwt.Claims, req jsonRPCRequest, args json.RawMessage) jsonRPCResponse {
	const toolName = "keeper.incarnation.maintenance-window.delete"
	var a maintenanceWindowDeleteArgs
	if resp, ok := h.decodeMaintenanceArgs(req, toolName, args, &a); !ok {
		return resp
	}
	if resp, ok := h.authorizeMaintenance(ctx, claims, req, toolName, "maintenance-set", a.ID); !ok {
		return resp
	}
	if !maintenance.ValidID(a.WindowID) {
		return h.toolError(req.ID, toolName, mcpCodeValidationFailed, "field 'window_id' must be a UUID")
	}
	w, err := maintenance.DeleteWindow(ctx, h.deps.IncarnationDB, a.ID, a.WindowID)
	if err != nil {
		return h.maintenanceToolError(req, toolName, a.ID, "maintenance window", a.WindowID, err)
	}
	h.writeAudit(ctx, audit.EventMaintenanceWindowDeleted, claims.Subject, maintenance.WindowAuditPayload(w, nil))
	return h.toolResult(req.ID, maintenanceDeleteOutput{Incarnation: a.ID, ID: w.ID})
}

func (h *Handler) callFreezeList(ctx context.Context, claims *jwt.Claims, req jsonRPCRequest, args json.RawMessage) jsonRPCResponse {
	const toolName = "keeper.incarnation.freeze.list"
	var a maintenanceListArgs
	if resp, ok := h.decodeMaintenanceArgs(req, toolName, args, &a); !ok {
		return resp
	}
	if resp, ok := h.authorizeMaintenance(ctx, claims, req, toolName, "get", a.ID); !ok {
		return resp
	}
	freezes, err := maintenance.ListFreezes(ctx, h.deps.IncarnationDB, a.ID)
	if err != nil {
		return h.maintenanceToolError(req, toolName, a.ID, "", "", err)
	}
	out := freezeListOutput{Incarnation: a.ID, Items: make([]freezeEntry, 0, len(freezes))}
	for _, f := range freezes {
		out.Items = append(out.Items, newFreezeEntry(f))
	}
	return h.toolResult(req.ID, out)
}

func (h *Handler) callFreezeCreate(ctx context.Context, claims *jwt.Claims, req jsonRPCRequest, args json.RawMessage) jsonRPCResponse {
	const toolName = "keeper.incarnation.freeze.create"
	var a freezeWriteArgs
	if resp, ok := h.decodeMaintenanceArgs(req, toolName, args, &a); !ok {
		return resp
	}
	if resp, ok := h.authorizeMaintenance(ctx, claims, req, toolName, "maintenance-set", a.ID); !ok {
		return resp
	}
	if a.FreezeID != "" {
		return h.toolError(req.ID, toolName, mcpCodeValidationFailed, "field 'freeze_id' is assigned by the Keeper; omit it on create")
	}
	spec, err := a.spec()
	if err != nil {
		return h.toolError(req.ID, toolName, mcpCodeValidationFailed, err.Error())
	}
	f, err := maintenance.InsertFreeze(ctx, h.deps.IncarnationDB, a.ID, spec)
	if err != nil {
		return h.maintenanceToolError(req, toolName, a.ID, "freeze", "", err)
	}
	h.writeAudit(ctx, audit.EventFreezeCreated, claims.Subject, maintenance.FreezeAuditPayload(f, nil))
	return h.toolResult(req.ID, newFreezeEntry(f))
}

func (h *Handler) callFreezeUpdate(ctx context.Context, claims *jwt.Claims, req jsonRPCRequest, args json.RawMessage) jsonRPCResponse {
	const toolName = "keeper.incarnation.freeze.update"
	var a freezeWriteArgs
	if resp, ok := h.decodeMaintenanceArgs(req, toolName, args, &a); !ok {
		return resp
	}
	if resp, ok := h.authorizeMaintenance(ctx, claims, req, toolName, "maintenance-set", a.ID); !ok {
		return resp
	}
	if !maintenance.ValidID(a.FreezeID) {
		return h.toolError(req.ID, toolName, mcpCodeValidationFailed, "field 'freeze_id' must be a UUID")
	}
	spec, err := a.spec()
	if err != nil {
		return h.toolError(req.ID, toolName, mcpCodeValidationFailed, err.Error())
	}
	before, after, err := maintenance.UpdateFreeze(ctx, h.deps.IncarnationDB, a.ID, a.FreezeID, spec)
	if err != nil {
		return h.maintenanceToolError(req, toolName, a.ID, "freeze", a.FreezeID, err)
	}
	h.writeAudit(ctx, audit.EventFreezeUpdated, claims.Subject, maintenance.FreezeAuditPayload(after, &before))
	return h.toolResult(req.ID, newFreezeEntry(after))
}

func (h *Handler) callFreezeDelete(ctx context.Context, claims *jwt.Claims, req jsonRPCRequest, args json.RawMessage) jsonRPCResponse {
	const toolName = "keeper.incarnation.freeze.delete"
	var a freezeDeleteArgs
	if resp, ok := h.decodeMaintenanceArgs(req, toolName, args, &a); !ok {
		return resp
	}
	if resp, ok := h.authorizeMaintenance(ctx, claims, req, toolName, "maintenance-set", a.ID); !ok {
		return resp
	}
	if !maintenance.ValidID(a.FreezeID) {
		return h.toolError(req.ID, toolName, mcpCodeValidationFailed, "field 'freeze_id' must be a UUID")
	}
	f, err := maintenance.DeleteFreeze(ctx, h.deps.IncarnationDB, a.ID, a.FreezeID)
	if err != nil {
		return h.maintenanceToolError(req, toolName, a.ID, "freeze", a.FreezeID, err)
	}
	h.writeAudit(ctx, audit.EventFreezeDeleted, claims.Subject, maintenance.FreezeAuditPayload(f, nil))
	return h.toolResult(req.ID, maintenanceDeleteOutput{Incarnation: a.ID, ID: f.ID})
}

// decodeMaintenanceArgs strictly decodes args into dst and checks the
// incarnation id every one of these tools carries in `id`.
func (h *Handler) decodeMaintenanceArgs(req jsonRPCRequest, toolName string, args json.RawMessage, dst interface{ incarnationID() string }) (jsonRPCResponse, bool) {
	if len(args) > 0 {
		if err := strictUnmarshal(args, dst); err != nil {
			return h.toolError(req.ID, toolName, mcpCodeMalformedRequest, "invalid arguments: "+err.Error()), false
		}
	}
	id := dst.incarnationID()
	if id == "" {
		return h.toolError(req.ID, toolName, mcpCodeValidationFailed, "field 'id' is required"), false
	}
	if !incarnation.ValidID(id) {
		return h.toolError(req.ID, toolName, mcpCodeValidationFailed, "field 'id' must match "+incarnation.IDPattern), false
	}
	return jsonRPCResponse{}, true
}

// authorizeMaintenance is gate + existence in the order every incarnation tool
// uses: a failed probe still asks the scoped check first (fail-closed), so an
// operator outside the scope learns "forbidden", not whether the incarnation
// exists.
// The tools call it before checking any argument but the incarnation id, so a
// caller without the right gets `forbidden` on a malformed body, as REST's
// middleware answers.
func (h *Handler) authorizeMaintenance(ctx context.Context, claims *jwt.Claims, req jsonRPCRequest, toolName, action, id string) (jsonRPCResponse, bool) {
	denied := "operator lacks required permission incarnation." + action
	inc, probeErr := incarnation.SelectByID(ctx, h.deps.IncarnationDB, id)
	if probeErr != nil {
		if scopeErr := h.checkIncarnationScope(claims, action, id, "", nil); scopeErr != nil {
			return h.toolError(req.ID, toolName, mcpCodeForbidden, denied), false
		}
		if errors.Is(probeErr, incarnation.ErrIncarnationNotFound) {
			return h.toolError(req.ID, toolName, mcpCodeNotFound, "incarnation "+id+" not found"), false
		}
		h.deps.Logger.Error("mcp: "+toolName+" probe failed", slog.String("name", id), slog.Any("error", probeErr))
		return h.toolError(req.ID, toolName, mcpCodeInternalError, "select incarnation failed"), false
	}
	if scopeErr := h.checkIncarnationScope(claims, action, inc.ID, inc.Service, inc.Covens); scopeErr != nil {
		return h.toolError(req.ID, toolName, mcpCodeForbidden, denied), false
	}
	return jsonRPCResponse{}, true
}

// maintenanceToolError maps the store's error vocabulary onto MCP codes, the
// same split REST makes; kind/rowID name the addressed row for a not-found.
func (h *Handler) maintenanceToolError(req jsonRPCRequest, toolName, incID, kind, rowID string, err error) jsonRPCResponse {
	var ve *maintenance.ValidationError
	switch {
	case errors.As(err, &ve):
		return h.toolError(req.ID, toolName, mcpCodeValidationFailed, ve.Error())
	case errors.Is(err, maintenance.ErrNotFound):
		return h.toolError(req.ID, toolName, mcpCodeNotFound, kind+" "+rowID+" not found on incarnation "+incID)
	case errors.Is(err, maintenance.ErrIncarnationNotFound):
		return h.toolError(req.ID, toolName, mcpCodeNotFound, "incarnation "+incID+" not found")
	case errors.Is(err, maintenance.ErrLimitReached):
		limit := maintenance.MaxWindowsPerIncarnation
		if kind == "freeze" {
			limit = maintenance.MaxFreezesPerIncarnation
		}
		return h.toolError(req.ID, toolName, mcpCodeValidationFailed,
			fmt.Sprintf("incarnation %s already has %d %ss, the maximum; delete one first", incID, limit, kind))
	default:
		h.deps.Logger.Error("mcp: "+toolName+" failed", slog.String("name", incID), slog.Any("error", err))
		return h.toolError(req.ID, toolName, mcpCodeInternalError, toolName+" failed")
	}
}

func (a *maintenanceListArgs) incarnationID() string         { return a.ID }
func (a *maintenanceWindowWriteArgs) incarnationID() string  { return a.ID }
func (a *maintenanceWindowDeleteArgs) incarnationID() string { return a.ID }
func (a *freezeWriteArgs) incarnationID() string             { return a.ID }
func (a *freezeDeleteArgs) incarnationID() string            { return a.ID }

// spec parses the two RFC 3339 strings ([parseBounds]); the kinds and the
// refusal of a mixed body are keeper/internal/maintenance's, shared with REST.
// args are the raw arguments, so a field of the other kind sent empty is
// refused here too, as REST's schema refuses it: zero reads as absent below.
func (a *maintenanceWindowWriteArgs) spec(args json.RawMessage) (maintenance.WindowSpec, error) {
	var sent map[string]json.RawMessage
	_ = json.Unmarshal(args, &sent)
	if err := maintenance.CheckKindFields(a.Kind, slices.Collect(maps.Keys(sent))); err != nil {
		return maintenance.WindowSpec{}, err
	}
	s := maintenance.WindowSpec{
		Kind:            a.Kind,
		Days:            a.Days,
		StartTime:       a.StartTime,
		DurationMinutes: a.DurationMinutes,
		Timezone:        a.Timezone,
		Comment:         a.Comment,
	}
	err := parseBounds(a.StartsAt, a.EndsAt, &s.StartsAt, &s.EndsAt)
	return s, err
}

func (a *freezeWriteArgs) spec() (maintenance.FreezeSpec, error) {
	s := maintenance.FreezeSpec{Comment: a.Comment}
	err := parseBounds(a.StartsAt, a.EndsAt, &s.StartsAt, &s.EndsAt)
	return s, err
}

// parseBounds parses starts_at and ends_at; an empty one is left zero so the
// validator reports it as missing (or as absent, on a weekly window), the same
// message REST gives.
func parseBounds(startsAt, endsAt string, starts, ends *time.Time) error {
	for _, f := range []struct {
		name string
		raw  string
		dst  *time.Time
	}{{"starts_at", startsAt, starts}, {"ends_at", endsAt, ends}} {
		if f.raw == "" {
			continue
		}
		t, err := time.Parse(time.RFC3339, f.raw)
		if err != nil {
			return fmt.Errorf("field '%s' must be an RFC 3339 timestamp, got %q", f.name, f.raw)
		}
		*f.dst = t
	}
	return nil
}

func newMaintenanceWindowEntry(w maintenance.Window) maintenanceWindowEntry {
	out := maintenanceWindowEntry{
		ID:        w.ID,
		Kind:      w.Kind,
		Comment:   w.Comment,
		CreatedAt: rfc3339(w.CreatedAt),
		UpdatedAt: rfc3339(w.UpdatedAt),
	}
	if w.Kind == maintenance.KindOnce {
		out.StartsAt, out.EndsAt = rfc3339(w.StartsAt), rfc3339(w.EndsAt)
		return out
	}
	out.Days, out.StartTime, out.DurationMinutes, out.Timezone = w.Days, w.StartTime, w.DurationMinutes, w.Timezone
	return out
}

func newFreezeEntry(f maintenance.Freeze) freezeEntry {
	return freezeEntry{
		ID:        f.ID,
		StartsAt:  rfc3339(f.StartsAt),
		EndsAt:    rfc3339(f.EndsAt),
		Comment:   f.Comment,
		CreatedAt: rfc3339(f.CreatedAt),
		UpdatedAt: rfc3339(f.UpdatedAt),
	}
}

func rfc3339(t time.Time) string { return t.UTC().Format(time.RFC3339) }
