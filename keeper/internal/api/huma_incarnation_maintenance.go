package api

// huma mounting of an incarnation's maintenance windows and freezes (NIM-1072).
// The typed shapes and operation metadata live in huma_incarnation_maintenance_op.go;
// the RBAC gate per route is wired in router.go.

import (
	"context"
	"net/http"

	"github.com/danielgtaylor/huma/v2"

	"github.com/souls-guild/soul-stack/keeper/internal/api/handlers"
	apimiddleware "github.com/souls-guild/soul-stack/keeper/internal/api/middleware"
	"github.com/souls-guild/soul-stack/keeper/internal/maintenance"
)

// registerHumaIncarnationMaintenanceRead mounts the two reads (GET
// .../maintenance-windows, GET .../freezes). incH nil → no-op.
func registerHumaIncarnationMaintenanceRead(humaAPI huma.API, incH *handlers.IncarnationHandler) {
	if incH == nil {
		return
	}
	huma.Register(humaAPI, maintenanceWindowListOperation(), func(ctx context.Context, in *maintenanceWindowListInput) (*maintenanceWindowListOutput, error) {
		view, err := incH.ListMaintenanceWindowsTyped(ctx, in.ID)
		if err != nil {
			return nil, incProblem(err)
		}
		return &maintenanceWindowListOutput{Body: newMaintenanceWindowListReply(view)}, nil
	})
	huma.Register(humaAPI, freezeListOperation(), func(ctx context.Context, in *freezeListInput) (*freezeListOutput, error) {
		freezes, err := incH.ListFreezesTyped(ctx, in.ID)
		if err != nil {
			return nil, incProblem(err)
		}
		items := make([]Freeze, 0, len(freezes))
		for _, f := range freezes {
			items = append(items, newFreezeReply(f))
		}
		return &freezeListOutput{Body: FreezeListReply{Items: items}}, nil
	})
}

// registerHumaIncarnationMaintenanceWrite mounts the six writes (SELF-AUDIT
// maintenance_window.* / freeze.* — written BY the handler). incH nil → no-op.
func registerHumaIncarnationMaintenanceWrite(humaAPI huma.API, incH *handlers.IncarnationHandler) {
	if incH == nil {
		return
	}
	huma.Register(humaAPI, maintenanceWindowCreateOperation(), func(ctx context.Context, in *maintenanceWindowCreateInput) (*maintenanceWindowWriteOutput, error) {
		claims, ok := apimiddleware.ClaimsFromContext(ctx)
		if !ok {
			return nil, incMissingClaims()
		}
		w, err := incH.CreateMaintenanceWindowTyped(ctx, claims, in.ID, windowSpec(in.Body))
		if err != nil {
			return nil, incProblem(err)
		}
		return &maintenanceWindowWriteOutput{Body: newMaintenanceWindowReply(w)}, nil
	})
	huma.Register(humaAPI, maintenanceWindowUpdateOperation(), func(ctx context.Context, in *maintenanceWindowUpdateInput) (*maintenanceWindowWriteOutput, error) {
		claims, ok := apimiddleware.ClaimsFromContext(ctx)
		if !ok {
			return nil, incMissingClaims()
		}
		w, err := incH.UpdateMaintenanceWindowTyped(ctx, claims, in.ID, in.WindowID, windowSpec(in.Body))
		if err != nil {
			return nil, incProblem(err)
		}
		return &maintenanceWindowWriteOutput{Body: newMaintenanceWindowReply(w)}, nil
	})
	huma.Register(humaAPI, maintenanceWindowDeleteOperation(), func(ctx context.Context, in *maintenanceWindowDeleteInput) (*maintenanceDeleteOutput, error) {
		claims, ok := apimiddleware.ClaimsFromContext(ctx)
		if !ok {
			return nil, incMissingClaims()
		}
		if err := incH.DeleteMaintenanceWindowTyped(ctx, claims, in.ID, in.WindowID); err != nil {
			return nil, incProblem(err)
		}
		return &maintenanceDeleteOutput{Status: http.StatusNoContent}, nil
	})
	huma.Register(humaAPI, freezeCreateOperation(), func(ctx context.Context, in *freezeCreateInput) (*freezeWriteOutput, error) {
		claims, ok := apimiddleware.ClaimsFromContext(ctx)
		if !ok {
			return nil, incMissingClaims()
		}
		f, err := incH.CreateFreezeTyped(ctx, claims, in.ID, freezeSpec(in.Body))
		if err != nil {
			return nil, incProblem(err)
		}
		return &freezeWriteOutput{Body: newFreezeReply(f)}, nil
	})
	huma.Register(humaAPI, freezeUpdateOperation(), func(ctx context.Context, in *freezeUpdateInput) (*freezeWriteOutput, error) {
		claims, ok := apimiddleware.ClaimsFromContext(ctx)
		if !ok {
			return nil, incMissingClaims()
		}
		f, err := incH.UpdateFreezeTyped(ctx, claims, in.ID, in.FreezeID, freezeSpec(in.Body))
		if err != nil {
			return nil, incProblem(err)
		}
		return &freezeWriteOutput{Body: newFreezeReply(f)}, nil
	})
	huma.Register(humaAPI, freezeDeleteOperation(), func(ctx context.Context, in *freezeDeleteInput) (*maintenanceDeleteOutput, error) {
		claims, ok := apimiddleware.ClaimsFromContext(ctx)
		if !ok {
			return nil, incMissingClaims()
		}
		if err := incH.DeleteFreezeTyped(ctx, claims, in.ID, in.FreezeID); err != nil {
			return nil, incProblem(err)
		}
		return &maintenanceDeleteOutput{Status: http.StatusNoContent}, nil
	})
}

// === projection between the wire DTOs and the domain ===

func windowSpec(r MaintenanceWindowRequest) maintenance.WindowSpec {
	return maintenance.WindowSpec{
		Kind:            r.Kind,
		Days:            r.Days,
		StartTime:       r.StartTime,
		DurationMinutes: r.DurationMinutes,
		Timezone:        r.Timezone,
		StartsAt:        r.StartsAt,
		EndsAt:          r.EndsAt,
		Comment:         r.Comment,
	}
}

func freezeSpec(r FreezeRequest) maintenance.FreezeSpec {
	return maintenance.FreezeSpec{StartsAt: r.StartsAt, EndsAt: r.EndsAt, Comment: r.Comment}
}

func newMaintenanceWindowReply(w maintenance.Window) MaintenanceWindow {
	out := MaintenanceWindow{
		ID:        w.ID,
		Kind:      w.Kind,
		Comment:   w.Comment,
		CreatedAt: w.CreatedAt,
		UpdatedAt: w.UpdatedAt,
	}
	if w.Kind == maintenance.KindOnce {
		starts, ends := w.StartsAt.UTC(), w.EndsAt.UTC()
		out.StartsAt, out.EndsAt = &starts, &ends
		return out
	}
	out.Days, out.StartTime, out.DurationMinutes, out.Timezone = w.Days, w.StartTime, w.DurationMinutes, w.Timezone
	return out
}

func newFreezeReply(f maintenance.Freeze) Freeze {
	return Freeze{
		ID:        f.ID,
		StartsAt:  f.StartsAt,
		EndsAt:    f.EndsAt,
		Comment:   f.Comment,
		CreatedAt: f.CreatedAt,
		UpdatedAt: f.UpdatedAt,
	}
}

func newMaintenanceWindowListReply(v handlers.MaintenanceWindowsView) MaintenanceWindowListReply {
	items := make([]MaintenanceWindow, 0, len(v.Windows))
	for _, w := range v.Windows {
		items = append(items, newMaintenanceWindowReply(w))
	}
	status := MaintenanceStatus{OpenNow: v.Status.OpenNow, NextStart: v.Status.NextStart.UTC()}
	if v.Status.ActiveFreeze != nil {
		f := newFreezeReply(*v.Status.ActiveFreeze)
		status.ActiveFreeze = &f
	}
	return MaintenanceWindowListReply{Items: items, Status: status}
}
