//go:build integration

package conductor

import (
	"context"
	"testing"
	"time"

	"github.com/souls-guild/soul-stack/keeper/internal/cadence"
	"github.com/souls-guild/soul-stack/keeper/internal/incarnation"
	"github.com/souls-guild/soul-stack/keeper/internal/maintenance"
	"github.com/souls-guild/soul-stack/keeper/internal/voyage"
)

// TestIntegration_Spawn_DeferredToTheWindowThenHeld — through real SQL: a tick
// at 14:00 of an in_maintenance_window schedule over an incarnation whose window
// opens at 02:00 inserts a Voyage scheduled for 02:00; the next tick of the same
// parallel schedule, while that child waits, adds nothing and still advances.
func TestIntegration_Spawn_DeferredToTheWindowThenHeld(t *testing.T) {
	resetAll(t)
	ctx := context.Background()
	const inc = "redis-window-it"
	t.Cleanup(func() {
		_, _ = integrationPool.Exec(context.Background(), `DELETE FROM incarnation WHERE id = $1`, inc)
	})
	if err := incarnation.Create(ctx, integrationPool, &incarnation.Incarnation{
		ID: inc, Service: "redis", ServiceVersion: "v1.0.0", StateSchemaVersion: 1,
		State: map[string]any{}, Status: incarnation.StatusReady,
	}); err != nil {
		t.Fatalf("seed incarnation: %v", err)
	}
	if _, err := maintenance.InsertWindow(ctx, integrationPool, inc, maintenance.WindowSpec{
		Days: everyDay, StartTime: "02:00", DurationMinutes: 180, Timezone: "UTC",
	}); err != nil {
		t.Fatalf("InsertWindow: %v", err)
	}

	c := seedCadence(t, cadenceID(1), cadence.OverlapPolicyParallel, -time.Minute)
	c.InMaintenanceWindow = true
	if err := cadence.Update(ctx, integrationPool, c); err != nil {
		t.Fatalf("set in_maintenance_window: %v", err)
	}

	s := newSpawner(t, []string{inc})
	s.now = func() time.Time { return nov(2, 14, 0) }
	if n, err := s.Run(ctx, 0, 0); err != nil || n != 1 {
		t.Fatalf("first tick: spawned %d, err %v; want 1", n, err)
	}
	var status string
	var scheduleAt *time.Time
	if err := integrationPool.QueryRow(ctx,
		`SELECT status, schedule_at FROM voyages WHERE cadence_id = $1`, c.ID).Scan(&status, &scheduleAt); err != nil {
		t.Fatalf("read child: %v", err)
	}
	if status != "scheduled" || scheduleAt == nil || !scheduleAt.Equal(nov(3, 2, 0)) {
		t.Fatalf("child status = %s schedule_at = %v; want scheduled at %v", status, scheduleAt, nov(3, 2, 0))
	}

	if _, err := integrationPool.Exec(ctx, `UPDATE cadences SET next_run_at = now() - interval '1 minute' WHERE id = $1`, c.ID); err != nil {
		t.Fatalf("make due again: %v", err)
	}
	s.now = func() time.Time { return nov(2, 15, 0) }
	if n, err := s.Run(ctx, 0, 0); err != nil || n != 0 {
		t.Fatalf("second tick: spawned %d, err %v; want 0 behind the deferred child", n, err)
	}
	if got := countVoyages(t, c.ID); got != 1 {
		t.Fatalf("children = %d; want 1", got)
	}
	if next, _ := readSchedule(t, c.ID); !next.After(nov(2, 15, 0)) {
		t.Fatalf("next_run_at = %v; the held tick must still advance", next)
	}
}

// TestIntegration_Spawn_AFreezeAddedAfterTheTickDefersAtTheClaim — a Cadence
// child spawned pending carries no consent, so a freeze added before a worker
// claims it defers the row to the freeze's end: the claim asks the same gate
// the tick asked (NIM-1074), and the row keeps no start and spends no attempt.
func TestIntegration_Spawn_AFreezeAddedAfterTheTickDefersAtTheClaim(t *testing.T) {
	resetAll(t)
	ctx := context.Background()
	const inc = "redis-freeze-it"
	t.Cleanup(func() {
		_, _ = integrationPool.Exec(context.Background(), `DELETE FROM incarnation WHERE id = $1`, inc)
	})
	if err := incarnation.Create(ctx, integrationPool, &incarnation.Incarnation{
		ID: inc, Service: "redis", ServiceVersion: "v1.0.0", StateSchemaVersion: 1,
		State: map[string]any{}, Status: incarnation.StatusReady,
	}); err != nil {
		t.Fatalf("seed incarnation: %v", err)
	}

	c := seedCadence(t, cadenceID(2), cadence.OverlapPolicySkip, -time.Minute)
	if n, err := newSpawner(t, []string{inc}).Run(ctx, 0, 0); err != nil || n != 1 {
		t.Fatalf("tick: spawned %d, err %v; want 1", n, err)
	}
	var status string
	if err := integrationPool.QueryRow(ctx, `SELECT status FROM voyages WHERE cadence_id = $1`, c.ID).Scan(&status); err != nil || status != "pending" {
		t.Fatalf("child status = %q, err %v; want pending with no freeze", status, err)
	}

	end := time.Now().UTC().Add(2 * time.Hour).Truncate(time.Second)
	if _, err := maintenance.InsertFreeze(ctx, integrationPool, inc, maintenance.FreezeSpec{
		StartsAt: time.Now().UTC().Add(-time.Hour), EndsAt: end, Comment: "sale",
	}); err != nil {
		t.Fatalf("InsertFreeze: %v", err)
	}
	claimed, outcomes, err := voyage.ClaimNext(ctx, integrationPool, "keeper-conductor-it", time.Minute, voyage.MaintenanceGate{})
	if err != nil {
		t.Fatalf("ClaimNext: %v", err)
	}
	if claimed != nil || len(outcomes) != 1 || !outcomes[0].Verdict.DeferTo.Equal(end) {
		t.Fatalf("claimed = %v outcomes = %+v; want the child deferred to %v", claimed, outcomes, end)
	}
	var scheduleAt *time.Time
	var startedAt *time.Time
	var attempt int
	if err := integrationPool.QueryRow(ctx, `SELECT status, schedule_at, started_at, attempt FROM voyages WHERE cadence_id = $1`, c.ID).
		Scan(&status, &scheduleAt, &startedAt, &attempt); err != nil {
		t.Fatalf("read child: %v", err)
	}
	if status != "scheduled" || scheduleAt == nil || !scheduleAt.Equal(end) || startedAt != nil || attempt != 0 {
		t.Fatalf("child = %s schedule_at %v started_at %v attempt %d; want scheduled at %v, not started", status, scheduleAt, startedAt, attempt, end)
	}
}
