package handlers

// An incarnation's maintenance windows and freezes (epic NIM-1069, NIM-1072):
// list, create, replace and delete each, under /v1/incarnations/{id}.
//
// Permission incarnation.maintenance-set on the writes and incarnation.get on
// the reads, both applied by route middleware with the same incarnation scope as
// every other incarnation route. No status gate: a window decides when a FUTURE
// run may start, so editing one while the incarnation is `applying` or
// `error_locked` changes nothing in flight.
//
// AUDIT CLASS: SELF-AUDIT — the writes record `maintenance_window.*` /
// `freeze.*` themselves, with the payload keeper/internal/maintenance builds for
// both REST and MCP.

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	apimiddleware "github.com/souls-guild/soul-stack/keeper/internal/api/middleware"
	"github.com/souls-guild/soul-stack/keeper/internal/api/problem"
	"github.com/souls-guild/soul-stack/keeper/internal/incarnation"
	"github.com/souls-guild/soul-stack/keeper/internal/jwt"
	"github.com/souls-guild/soul-stack/keeper/internal/maintenance"
	"github.com/souls-guild/soul-stack/shared/audit"
)

// MaintenanceWindowsView is the domain result of GET .../maintenance-windows:
// the windows, the cluster default when it applies (NIM-1138), and what the
// calculator says about the windows in effect now.
type MaintenanceWindowsView struct {
	Windows  []maintenance.Window
	Defaults []maintenance.Window
	Status   maintenance.Status
}

// ListMaintenanceWindowsTyped — GET /v1/incarnations/{id}/maintenance-windows
// (READ). The status block is computed over the windows in effect AND the
// freezes, so a freeze in force shows as `open_now: false` with its end as
// `next_start`.
func (h *IncarnationHandler) ListMaintenanceWindowsTyped(ctx context.Context, name string) (MaintenanceWindowsView, error) {
	var zero MaintenanceWindowsView
	if err := h.loadIncarnationForMaintenance(ctx, name, "list-maintenance-windows"); err != nil {
		return zero, err
	}
	rules, err := maintenance.LoadRulesOf(ctx, h.db, name, h.maintenanceDefaults)
	if err != nil {
		return zero, h.maintenanceInternal("list-maintenance-windows", name, err)
	}
	status, err := rules.Status(time.Now())
	if err != nil {
		return zero, h.maintenanceInternal("list-maintenance-windows", name, err)
	}
	return MaintenanceWindowsView{Windows: rules.Windows, Defaults: rules.DefaultsInEffect(), Status: status}, nil
}

// CreateMaintenanceWindowTyped — POST /v1/incarnations/{id}/maintenance-windows
// (SELF-AUDIT maintenance_window.created).
func (h *IncarnationHandler) CreateMaintenanceWindowTyped(ctx context.Context, claims *jwt.Claims, name string, spec maintenance.WindowSpec) (maintenance.Window, error) {
	if err := h.loadIncarnationForMaintenance(ctx, name, "create-maintenance-window"); err != nil {
		return maintenance.Window{}, err
	}
	w, err := maintenance.InsertWindow(ctx, h.db, name, spec)
	if err != nil {
		return maintenance.Window{}, h.maintenanceError("create-maintenance-window", name, kindWindow, "", err)
	}
	h.writeMaintenanceAudit(ctx, claims, audit.EventMaintenanceWindowCreated, maintenance.WindowAuditPayload(w, nil))
	return w, nil
}

// UpdateMaintenanceWindowTyped — PUT .../maintenance-windows/{window_id}
// (SELF-AUDIT maintenance_window.updated). Replaces every field.
func (h *IncarnationHandler) UpdateMaintenanceWindowTyped(ctx context.Context, claims *jwt.Claims, name, id string, spec maintenance.WindowSpec) (maintenance.Window, error) {
	if err := h.checkMaintenanceAddress(ctx, name, "window_id", id, "update-maintenance-window"); err != nil {
		return maintenance.Window{}, err
	}
	before, after, err := maintenance.UpdateWindow(ctx, h.db, name, id, spec)
	if err != nil {
		return maintenance.Window{}, h.maintenanceError("update-maintenance-window", name, kindWindow, id, err)
	}
	h.writeMaintenanceAudit(ctx, claims, audit.EventMaintenanceWindowUpdated, maintenance.WindowAuditPayload(after, &before))
	return after, nil
}

// DeleteMaintenanceWindowTyped — DELETE .../maintenance-windows/{window_id}
// (SELF-AUDIT maintenance_window.deleted). Not idempotent: a second delete is
// 404, so the trail never holds a delete of a window that was not there.
func (h *IncarnationHandler) DeleteMaintenanceWindowTyped(ctx context.Context, claims *jwt.Claims, name, id string) error {
	if err := h.checkMaintenanceAddress(ctx, name, "window_id", id, "delete-maintenance-window"); err != nil {
		return err
	}
	w, err := maintenance.DeleteWindow(ctx, h.db, name, id)
	if err != nil {
		return h.maintenanceError("delete-maintenance-window", name, kindWindow, id, err)
	}
	h.writeMaintenanceAudit(ctx, claims, audit.EventMaintenanceWindowDeleted, maintenance.WindowAuditPayload(w, nil))
	return nil
}

// ListFreezesTyped — GET /v1/incarnations/{id}/freezes (READ), past freezes
// included.
func (h *IncarnationHandler) ListFreezesTyped(ctx context.Context, name string) ([]maintenance.Freeze, error) {
	if err := h.loadIncarnationForMaintenance(ctx, name, "list-freezes"); err != nil {
		return nil, err
	}
	freezes, err := maintenance.ListFreezes(ctx, h.db, name)
	if err != nil {
		return nil, h.maintenanceInternal("list-freezes", name, err)
	}
	return freezes, nil
}

// CreateFreezeTyped — POST /v1/incarnations/{id}/freezes (SELF-AUDIT
// freeze.created).
func (h *IncarnationHandler) CreateFreezeTyped(ctx context.Context, claims *jwt.Claims, name string, spec maintenance.FreezeSpec) (maintenance.Freeze, error) {
	if err := h.loadIncarnationForMaintenance(ctx, name, "create-freeze"); err != nil {
		return maintenance.Freeze{}, err
	}
	f, err := maintenance.InsertFreeze(ctx, h.db, name, spec)
	if err != nil {
		return maintenance.Freeze{}, h.maintenanceError("create-freeze", name, kindFreeze, "", err)
	}
	h.writeMaintenanceAudit(ctx, claims, audit.EventFreezeCreated, maintenance.FreezeAuditPayload(f, nil))
	return f, nil
}

// UpdateFreezeTyped — PUT .../freezes/{freeze_id} (SELF-AUDIT freeze.updated).
func (h *IncarnationHandler) UpdateFreezeTyped(ctx context.Context, claims *jwt.Claims, name, id string, spec maintenance.FreezeSpec) (maintenance.Freeze, error) {
	if err := h.checkMaintenanceAddress(ctx, name, "freeze_id", id, "update-freeze"); err != nil {
		return maintenance.Freeze{}, err
	}
	before, after, err := maintenance.UpdateFreeze(ctx, h.db, name, id, spec)
	if err != nil {
		return maintenance.Freeze{}, h.maintenanceError("update-freeze", name, kindFreeze, id, err)
	}
	h.writeMaintenanceAudit(ctx, claims, audit.EventFreezeUpdated, maintenance.FreezeAuditPayload(after, &before))
	return after, nil
}

// DeleteFreezeTyped — DELETE .../freezes/{freeze_id} (SELF-AUDIT
// freeze.deleted).
func (h *IncarnationHandler) DeleteFreezeTyped(ctx context.Context, claims *jwt.Claims, name, id string) error {
	if err := h.checkMaintenanceAddress(ctx, name, "freeze_id", id, "delete-freeze"); err != nil {
		return err
	}
	f, err := maintenance.DeleteFreeze(ctx, h.db, name, id)
	if err != nil {
		return h.maintenanceError("delete-freeze", name, kindFreeze, id, err)
	}
	h.writeMaintenanceAudit(ctx, claims, audit.EventFreezeDeleted, maintenance.FreezeAuditPayload(f, nil))
	return nil
}

// checkMaintenanceAddress validates a window or freeze id from the path and
// resolves the incarnation, in that order: a malformed id is 422 before any
// read.
func (h *IncarnationHandler) checkMaintenanceAddress(ctx context.Context, name, field, id, op string) error {
	if !incarnation.ValidID(name) {
		return incProblem(problem.TypeValidationFailed, "path 'id' must match "+incarnation.IDPattern)
	}
	if !maintenance.ValidID(id) {
		return incProblem(problem.TypeValidationFailed, "path '"+field+"' must be a UUID")
	}
	return h.loadIncarnationForMaintenance(ctx, name, op)
}

func (h *IncarnationHandler) loadIncarnationForMaintenance(ctx context.Context, name, op string) error {
	if !incarnation.ValidID(name) {
		return incProblem(problem.TypeValidationFailed, "path 'id' must match "+incarnation.IDPattern)
	}
	if _, err := incarnation.SelectByID(ctx, h.db, name); err != nil {
		if errors.Is(err, incarnation.ErrIncarnationNotFound) {
			return incProblem(problem.TypeNotFound, "incarnation "+name+" not found")
		}
		return h.maintenanceInternal(op, name, err)
	}
	return nil
}

type maintenanceKind struct {
	noun   string
	plural string
	max    int
}

var (
	kindWindow = maintenanceKind{"maintenance window", "maintenance windows", maintenance.MaxWindowsPerIncarnation}
	kindFreeze = maintenanceKind{"freeze", "freezes", maintenance.MaxFreezesPerIncarnation}
)

// maintenanceError maps the store's error vocabulary onto problem+json; id is
// the addressed row, empty on a create.
func (h *IncarnationHandler) maintenanceError(op, name string, kind maintenanceKind, id string, err error) error {
	var ve *maintenance.ValidationError
	switch {
	case errors.As(err, &ve):
		return incProblem(problem.TypeValidationFailed, ve.Error())
	case errors.Is(err, maintenance.ErrNotFound):
		return incProblem(problem.TypeNotFound, kind.noun+" "+id+" not found on incarnation "+name)
	case errors.Is(err, maintenance.ErrIncarnationNotFound):
		return incProblem(problem.TypeNotFound, "incarnation "+name+" not found")
	case errors.Is(err, maintenance.ErrLimitReached):
		return incProblem(problem.TypeValidationFailed, fmt.Sprintf(
			"incarnation %s already has %d %s, the maximum; delete one first", name, kind.max, kind.plural))
	default:
		return h.maintenanceInternal(op, name, err)
	}
}

func (h *IncarnationHandler) maintenanceInternal(op, name string, err error) error {
	h.logger.Error("incarnation."+op+": failed", slog.String("name", name), slog.Any("error", err))
	return incProblem(problem.TypeInternalError, op+" failed")
}

func (h *IncarnationHandler) writeMaintenanceAudit(ctx context.Context, claims *jwt.Claims, ev audit.EventType, payload map[string]any) {
	if h.auditW == nil {
		return
	}
	_ = h.auditW.Write(ctx, &audit.Event{
		EventType: ev,
		Source:    apimiddleware.ScenarioInvocationSource(ctx),
		ArchonAID: claims.Subject,
		Payload:   payload,
	})
}
