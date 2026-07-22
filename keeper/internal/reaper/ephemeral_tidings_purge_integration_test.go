//go:build integration

package reaper_test

// Integration test for `purge_orphan_ephemeral_tidings` (ADR-052(g) N2) on real
// PG. The epic's most important guard: deleting an ephemeral Tiding must NOT
// outrun terminal notification delivery. The rule deletes only after the grace
// period from terminal Voyage; during grace, tap-dispatcher has guaranteed time
// to match and enqueue the notification. Verify the matrix: terminal<grace
// survives; terminal>grace is deleted; orphan without voyage is deleted; running
// survives.

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/souls-guild/soul-stack/keeper/internal/reaper"
)

// discardLogger is slog to io.Discard; silentLogger lives in internal package reaper.
func discardLogger() *slog.Logger {
	return slog.New(slog.NewJSONHandler(io.Discard, nil))
}

// seedOperatorRaw is a minimal operator row, FK for voyages.started_by_aid.
func seedOperatorRaw(t *testing.T, ctx context.Context, aid string) {
	t.Helper()
	if _, err := integrationPool.Exec(ctx,
		`INSERT INTO operators (aid, display_name, auth_method)
		 VALUES ($1, $1, 'jwt') ON CONFLICT (aid) DO NOTHING`, aid); err != nil {
		t.Fatalf("seedOperatorRaw(%s): %v", aid, err)
	}
}

// seedHeraldRaw is a webhook Herald, FK for tidings.herald.
func seedHeraldRaw(t *testing.T, ctx context.Context, name, aid string) {
	t.Helper()
	cfg, _ := json.Marshal(map[string]any{"url": "https://hooks.example.com/" + name})
	if _, err := integrationPool.Exec(ctx,
		`INSERT INTO heralds (name, type, config, enabled, created_by_aid)
		 VALUES ($1, 'webhook', $2, true, $3)`, name, cfg, aid); err != nil {
		t.Fatalf("seedHeraldRaw(%s): %v", name, err)
	}
}

// seedVoyageTerminal creates a voyage in a terminal status with given finished_at.
func seedVoyageTerminal(t *testing.T, ctx context.Context, voyageID, aid string, finishedAt time.Time) {
	t.Helper()
	if _, err := integrationPool.Exec(ctx,
		`INSERT INTO voyages (voyage_id, kind, module, target_resolved, target_origin,
		                      total_batches, started_by_aid, status, finished_at)
		 VALUES ($1, 'command', 'core.cmd.shell', '[]'::jsonb, '{}'::jsonb, 1, $2, 'succeeded', $3)`,
		voyageID, aid, finishedAt); err != nil {
		t.Fatalf("seedVoyageTerminal(%s): %v", voyageID, err)
	}
}

// seedVoyageRunning creates a running voyage, NOT terminal, with finished_at
// NULL. running requires claim fields to be NOT NULL
// (voyages_running_claim_consistency).
func seedVoyageRunning(t *testing.T, ctx context.Context, voyageID, aid string) {
	t.Helper()
	if _, err := integrationPool.Exec(ctx,
		`INSERT INTO voyages (voyage_id, kind, module, target_resolved, target_origin,
		                      total_batches, started_by_aid, status,
		                      claimed_by_kid, claim_expires_at)
		 VALUES ($1, 'command', 'core.cmd.shell', '[]'::jsonb, '{}'::jsonb, 1, $2, 'running',
		         'kid-test', NOW() + INTERVAL '5 minutes')`,
		voyageID, aid); err != nil {
		t.Fatalf("seedVoyageRunning(%s): %v", voyageID, err)
	}
}

// seedEphemeralTiding creates an ephemeral Tiding bound to voyage_id.
func seedEphemeralTiding(t *testing.T, ctx context.Context, name, herald, voyageID string) {
	t.Helper()
	if _, err := integrationPool.Exec(ctx,
		`INSERT INTO tidings (name, herald, event_types, ephemeral, voyage_id, enabled)
		 VALUES ($1, $2, ARRAY['command_run.completed'], true, $3, true)`,
		name, herald, voyageID); err != nil {
		t.Fatalf("seedEphemeralTiding(%s): %v", name, err)
	}
}

// seedPersistentTiding creates a regular, NOT ephemeral rule. purger must never
// touch it under any conditions.
func seedPersistentTiding(t *testing.T, ctx context.Context, name, herald string) {
	t.Helper()
	if _, err := integrationPool.Exec(ctx,
		`INSERT INTO tidings (name, herald, event_types, ephemeral, enabled)
		 VALUES ($1, $2, ARRAY['command_run.completed'], false, true)`,
		name, herald); err != nil {
		t.Fatalf("seedPersistentTiding(%s): %v", name, err)
	}
}

func tidingExists(t *testing.T, ctx context.Context, name string) bool {
	t.Helper()
	var n int
	if err := integrationPool.QueryRow(ctx,
		`SELECT COUNT(*) FROM tidings WHERE name = $1`, name).Scan(&n); err != nil {
		t.Fatalf("tidingExists(%s): %v", name, err)
	}
	return n > 0
}

func resetHeraldVoyageTables(t *testing.T, ctx context.Context) {
	t.Helper()
	if _, err := integrationPool.Exec(ctx,
		`TRUNCATE tidings, heralds, voyage_targets, voyages, operators RESTART IDENTITY CASCADE`); err != nil {
		t.Fatalf("reset herald/voyage tables: %v", err)
	}
}

// TestIntegration_PurgeOrphanEphemeralTidings_GraceMatrix is the main cleanup
// guard. With grace=10m, the rule for a run finished 1m ago SURVIVES because it
// is inside the delivery window; deletion would outrun tap-dispatcher. A run
// finished 20m ago is deleted; a nonexistent run is deleted; running survives;
// a persistent rule is never touched.
func TestIntegration_PurgeOrphanEphemeralTidings_GraceMatrix(t *testing.T) {
	ctx := context.Background()
	resetHeraldVoyageTables(t, ctx)
	seedOperatorRaw(t, ctx, "archon-r")
	seedHeraldRaw(t, ctx, "ops", "archon-r")

	now := time.Now().UTC()
	// (a) terminal 1m ago is INSIDE the delivery window (grace=10m), so it must SURVIVE.
	seedVoyageTerminal(t, ctx, "01HVOYAGEFRESH00000000000A", "archon-r", now.Add(-1*time.Minute))
	seedEphemeralTiding(t, ctx, "eph-fresh", "ops", "01HVOYAGEFRESH00000000000A")
	// (b) terminal 20m ago is beyond grace, so it must be DELETED.
	seedVoyageTerminal(t, ctx, "01HVOYAGEOLD0000000000000B", "archon-r", now.Add(-20*time.Minute))
	seedEphemeralTiding(t, ctx, "eph-old", "ops", "01HVOYAGEOLD0000000000000B")
	// (c) voyage does not exist (orphan), so it must be DELETED.
	seedEphemeralTiding(t, ctx, "eph-orphan", "ops", "01HVOYAGEGONE000000000000C")
	// (d) running voyage is not terminal, so it must SURVIVE.
	seedVoyageRunning(t, ctx, "01HVOYAGERUN0000000000000D", "archon-r")
	seedEphemeralTiding(t, ctx, "eph-running", "ops", "01HVOYAGERUN0000000000000D")
	// (e) persistent rule is NEVER touched.
	seedPersistentTiding(t, ctx, "persistent-rule", "ops")

	p := reaper.NewEphemeralTidingsPurger(integrationPool, discardLogger())
	affected, err := p.Run(ctx, 10*time.Minute, 1000)
	if err != nil {
		t.Fatalf("purger.Run: %v", err)
	}
	if affected != 2 {
		t.Errorf("affected = %d, want 2 (eph-old + eph-orphan)", affected)
	}

	// Delivery guard: a fresh terminal inside the grace window MUST survive.
	// Otherwise deletion could outrun tap-dispatcher and completion notification
	// might not be delivered.
	if !tidingExists(t, ctx, "eph-fresh") {
		t.Error("eph-fresh was deleted TOO EARLY (terminal within grace); notification might not have been delivered")
	}
	if !tidingExists(t, ctx, "eph-running") {
		t.Error("eph-running was deleted; running is not terminal and must not be deleted")
	}
	if !tidingExists(t, ctx, "persistent-rule") {
		t.Error("persistent rule was deleted; purger must touch only ephemeral rules")
	}
	if tidingExists(t, ctx, "eph-old") {
		t.Error("eph-old was NOT deleted (terminal beyond grace); orphaned rule leaked")
	}
	if tidingExists(t, ctx, "eph-orphan") {
		t.Error("eph-orphan was NOT deleted (run does not exist); orphaned rule leaked")
	}
}

// TestIntegration_PurgeOrphanEphemeralTidings_NoGraceWindow: with grace=0, the
// boundary case, even a fresh terminal is deleted. This confirms that grace, not
// something else, protects the delivery window. Keep it as a counterpoint to the
// main guard: grace is required for correctness, not cosmetic.
func TestIntegration_PurgeOrphanEphemeralTidings_NoGraceWindow(t *testing.T) {
	ctx := context.Background()
	resetHeraldVoyageTables(t, ctx)
	seedOperatorRaw(t, ctx, "archon-r")
	seedHeraldRaw(t, ctx, "ops", "archon-r")

	now := time.Now().UTC()
	seedVoyageTerminal(t, ctx, "01HVOYAGEJUST0000000000000E", "archon-r", now.Add(-1*time.Second))
	seedEphemeralTiding(t, ctx, "eph-just", "ops", "01HVOYAGEJUST0000000000000E")

	p := reaper.NewEphemeralTidingsPurger(integrationPool, discardLogger())
	// grace=0 means terminal 1s ago is already "older" (finished_at < NOW() - 0), so delete.
	if _, err := p.Run(ctx, 0, 1000); err != nil {
		t.Fatalf("purger.Run: %v", err)
	}
	if tidingExists(t, ctx, "eph-just") {
		t.Error("with grace=0, a fresh terminal must be deleted (proves grace holds the window)")
	}
}
