package handlers

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"reflect"
	"testing"
	"time"

	"github.com/souls-guild/soul-stack/keeper/internal/maintenance"
	"github.com/souls-guild/soul-stack/keeper/internal/maintenance/maintenancetest"
)

// ★ The REST readers hold an incarnation without windows of its own to the
// cluster default exactly as to the same slots as its own windows (NIM-1138):
// a run asking for the window, the status of GET .../maintenance-windows, and a
// scenario Voyage's start gate — each wired the way the server wires it.
func TestRESTReaders_DefaultWindowsHoldAsOwnWindows(t *testing.T) {
	slot, _ := maintenancetest.Slot(time.Now())
	run, status, voyageStart := map[string]any{}, map[string]any{}, map[string]any{}
	for _, m := range maintenancetest.Modes(slot) {
		db := gateDB(m.Own, nil)
		h := NewIncarnationHandler(db, &fakeStarter{}, nil, &fakeResolver{ok: true}, nil, nil, nil, nil)
		h.SetMaintenanceDefaults(m.Defaults)

		reply, err := h.RunTyped(context.Background(), claims("archon-alice"), maintenancetest.Incarnation, "restart", nil,
			RunGate{InMaintenanceWindow: true})
		if err != nil {
			t.Fatalf("%s: RunTyped: %v", m.Name, err)
		}
		run[m.Name] = fmt.Sprint(reply.Body.ScheduleAt != nil, reply.Body.ApplyID != "")
		if reply.Body.ScheduleAt != nil {
			run[m.Name] = reply.Body.ScheduleAt.UTC().String()
		}

		view, err := h.ListMaintenanceWindowsTyped(context.Background(), maintenancetest.Incarnation)
		if err != nil {
			t.Fatalf("%s: ListMaintenanceWindowsTyped: %v", m.Name, err)
		}
		status[m.Name] = fmt.Sprint(view.Status.OpenNow, " ", view.Status.NextStart.UTC(), " ", view.Status.ActiveFreeze)

		vh := &VoyageHandler{store: &maintenancetest.DB{Windows: m.Own}, logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
		vh.SetMaintenanceDefaults(m.Defaults)
		start, err := vh.scenarioStartErr(context.Background(), &voyageCreateRequest{InMaintenanceWindow: true},
			[]string{maintenancetest.Incarnation})
		if err != nil {
			t.Fatalf("%s: scenarioStartErr: %v", m.Name, err)
		}
		voyageStart[m.Name] = start.DeferTo.UTC().String()
	}
	maintenancetest.Agree(t, "a run in the window", run)

	// A forced run is held to no window: under the default it starts now.
	starter := &fakeStarter{}
	h := NewIncarnationHandler(gateDB(nil, nil), starter, nil, &fakeResolver{ok: true}, nil, nil, nil, nil)
	h.SetMaintenanceDefaults(maintenancetest.Modes(slot)[0].Defaults)
	if _, err := h.RunTyped(context.Background(), claims("archon-alice"), maintenancetest.Incarnation, "restart", nil, RunGate{}); err != nil || starter.calls != 1 {
		t.Errorf("a forced run under the default: starts=%d err=%v, want it started now", starter.calls, err)
	}
	maintenancetest.Agree(t, "the windows status", status)
	maintenancetest.Agree(t, "a scenario Voyage's start gate", voyageStart)
}

// The list shows the default and says where the windows come from: `default`
// with the slots while the incarnation has no weekly window, `own` and no
// defaults once it has one, `none` and no defaults without a default.
func TestListMaintenanceWindows_DefaultsAndSource(t *testing.T) {
	slot, _ := maintenancetest.Slot(time.Now())
	modes := maintenancetest.Modes(slot)
	byDefault := modes[0].Defaults
	own := maintenance.Window{ID: "22222222-2222-4222-8222-000000000002", IncarnationID: maintenancetest.Incarnation,
		Kind: maintenance.KindWeekly, Days: []string{"sun"}, StartTime: "04:00", DurationMinutes: 30, Timezone: "UTC"}

	for _, tc := range []struct {
		name     string
		own      []maintenance.Window
		defaults maintenance.DefaultsSource
		source   string
		shown    int
	}{
		{"no own window", nil, byDefault, maintenance.SourceDefault, 1},
		{"an own weekly window", []maintenance.Window{own}, byDefault, maintenance.SourceOwn, 0},
		{"no default", nil, nil, maintenance.SourceNone, 0},
	} {
		h := NewIncarnationHandler(gateDB(tc.own, nil), &fakeStarter{}, nil, &fakeResolver{ok: true}, nil, nil, nil, nil)
		h.SetMaintenanceDefaults(tc.defaults)
		view, err := h.ListMaintenanceWindowsTyped(context.Background(), maintenancetest.Incarnation)
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if view.Status.Source != tc.source || len(view.Defaults) != tc.shown {
			t.Errorf("%s: source %q with %d defaults, want %q with %d", tc.name, view.Status.Source, len(view.Defaults), tc.source, tc.shown)
		}
		if tc.shown > 0 && !reflect.DeepEqual(view.Defaults[0].Days, slot.Days) {
			t.Errorf("%s: defaults %+v, want the cluster slot", tc.name, view.Defaults)
		}
	}
}
