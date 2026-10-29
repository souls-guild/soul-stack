//go:build integration

package cadence_test

// Migration 135 (NIM-1075): cadences.in_maintenance_window round-trips, the
// CHECK holds it to kind=scenario, and the two overlap checks see a child
// deferred to a window as the spawn path relies on: live for skip/queue,
// deferred for parallel, while a running child is live and not deferred.

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/souls-guild/soul-stack/keeper/internal/cadence"
	"github.com/souls-guild/soul-stack/keeper/internal/voyage"
)

func TestInMaintenanceWindow_RoundTrip(t *testing.T) {
	clearCadences(t)
	ctx := context.Background()

	c := newPercentCadence(nil)
	c.InMaintenanceWindow = true
	if err := cadence.Insert(ctx, integrationPool, c); err != nil {
		t.Fatalf("Insert: %v", err)
	}
	got, err := cadence.Get(ctx, integrationPool, c.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if !got.InMaintenanceWindow {
		t.Fatal("in_maintenance_window round-trip = false, want true")
	}

	got.InMaintenanceWindow = false
	if err := cadence.Update(ctx, integrationPool, got); err != nil {
		t.Fatalf("Update: %v", err)
	}
	if again, _ := cadence.Get(ctx, integrationPool, c.ID); again.InMaintenanceWindow {
		t.Fatal("in_maintenance_window after Update = true, want false")
	}
}

func TestInMaintenanceWindow_CheckRefusesACommandRecipe(t *testing.T) {
	clearCadences(t)
	_, err := integrationPool.Exec(context.Background(), `
		INSERT INTO cadences (
		    id, name, enabled, schedule_kind, interval_seconds, overlap_policy,
		    kind, module, target, in_maintenance_window, created_by_aid
		) VALUES ($1, 'bad', true, 'interval', 300, 'skip',
		    'command', 'core.pkg.present', '{"coven":["prod"]}'::jsonb, true, $2)`,
		nextID(), testAID)
	if err == nil || !strings.Contains(err.Error(), "cadences_in_maintenance_window_scenario_only") {
		t.Fatalf("INSERT command + in_maintenance_window: err = %v, want the scenario-only CHECK", err)
	}
}

func TestOverlapChecks_SeeADeferredChild(t *testing.T) {
	clearCadences(t)
	ctx := context.Background()
	t.Cleanup(func() { _, _ = integrationPool.Exec(context.Background(), `DELETE FROM voyages`) })

	c := newPercentCadence(nil)
	if err := cadence.Insert(ctx, integrationPool, c); err != nil {
		t.Fatalf("Insert: %v", err)
	}
	check := func(wantLive, wantDeferred bool) {
		t.Helper()
		live, err := cadence.HasLiveChild(ctx, integrationPool, c.ID)
		if err != nil {
			t.Fatalf("HasLiveChild: %v", err)
		}
		deferred, err := cadence.HasDeferredChild(ctx, integrationPool, c.ID)
		if err != nil {
			t.Fatalf("HasDeferredChild: %v", err)
		}
		if live != wantLive || deferred != wantDeferred {
			t.Fatalf("live = %v deferred = %v; want %v %v", live, deferred, wantLive, wantDeferred)
		}
	}
	check(false, false)

	at := time.Now().Add(12 * time.Hour).UTC()
	row, _ := cadence.BuildVoyage(c, cadence.NewVoyageID(), []string{"redis-prod"})
	row.ScheduleAt, row.Status = &at, voyage.StatusScheduled
	if err := voyage.Insert(ctx, integrationPool, row); err != nil {
		t.Fatalf("insert deferred child: %v", err)
	}
	check(true, true)

	if _, err := integrationPool.Exec(ctx, `UPDATE voyages SET status = 'running', started_at = now(),
		claimed_by_kid = 'kid-it', claim_expires_at = now() + interval '1 minute' WHERE voyage_id = $1`, row.VoyageID); err != nil {
		t.Fatalf("start child: %v", err)
	}
	check(true, false)
}
