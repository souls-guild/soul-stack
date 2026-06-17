//go:build integration

package reaper_test

// Integration-тест правила `purge_orphan_ephemeral_tidings` (ADR-052(g) N2) на
// реальной PG. Самый важный guard эпика: снос ephemeral-Tiding НЕ опережает
// доставку терминального уведомления — правило сносит правило ТОЛЬКО после
// grace-периода от терминала Voyage (за grace tap-dispatcher гарантированно
// сматчил и заэнкьюил уведомление). Проверяем матрицу: terminal<grace →
// выживает; terminal>grace → снос; orphan (voyage нет) → снос; running → выживает.

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/souls-guild/soul-stack/keeper/internal/reaper"
)

// discardLogger — slog в io.Discard (silentLogger живёт в internal-package reaper).
func discardLogger() *slog.Logger {
	return slog.New(slog.NewJSONHandler(io.Discard, nil))
}

// seedOperatorRaw — минимальный operator-row (FK для voyages.started_by_aid).
func seedOperatorRaw(t *testing.T, ctx context.Context, aid string) {
	t.Helper()
	if _, err := integrationPool.Exec(ctx,
		`INSERT INTO operators (aid, display_name, auth_method)
		 VALUES ($1, $1, 'jwt') ON CONFLICT (aid) DO NOTHING`, aid); err != nil {
		t.Fatalf("seedOperatorRaw(%s): %v", aid, err)
	}
}

// seedHeraldRaw — webhook-Herald (FK для tidings.herald).
func seedHeraldRaw(t *testing.T, ctx context.Context, name, aid string) {
	t.Helper()
	cfg, _ := json.Marshal(map[string]any{"url": "https://hooks.example.com/" + name})
	if _, err := integrationPool.Exec(ctx,
		`INSERT INTO heralds (name, type, config, enabled, created_by_aid)
		 VALUES ($1, 'webhook', $2, true, $3)`, name, cfg, aid); err != nil {
		t.Fatalf("seedHeraldRaw(%s): %v", name, err)
	}
}

// seedVoyageTerminal — voyage в терминальном статусе с заданным finished_at.
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

// seedVoyageRunning — voyage в running (НЕ терминал, finished_at NULL). running
// требует claim-полей NOT NULL (voyages_running_claim_consistency).
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

// seedEphemeralTiding — ephemeral-Tiding, привязанный к voyage_id.
func seedEphemeralTiding(t *testing.T, ctx context.Context, name, herald, voyageID string) {
	t.Helper()
	if _, err := integrationPool.Exec(ctx,
		`INSERT INTO tidings (name, herald, event_types, ephemeral, voyage_id, enabled)
		 VALUES ($1, $2, ARRAY['command_run.completed'], true, $3, true)`,
		name, herald, voyageID); err != nil {
		t.Fatalf("seedEphemeralTiding(%s): %v", name, err)
	}
}

// seedPersistentTiding — обычное (НЕ ephemeral) правило: purger не должен его
// трогать ни при каких условиях.
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

// TestIntegration_PurgeOrphanEphemeralTidings_GraceMatrix — основной guard
// очистки. grace=10m: правило для прогона, завершённого 1m назад, ВЫЖИВАЕТ
// (окно доставки) — снос опередил бы tap-dispatcher; завершённого 20m назад —
// сносится; для несуществующего прогона — сносится; для running — выживает;
// постоянное правило — не трогается никогда.
func TestIntegration_PurgeOrphanEphemeralTidings_GraceMatrix(t *testing.T) {
	ctx := context.Background()
	resetHeraldVoyageTables(t, ctx)
	seedOperatorRaw(t, ctx, "archon-r")
	seedHeraldRaw(t, ctx, "ops", "archon-r")

	now := time.Now().UTC()
	// (a) терминал 1m назад — В ОКНЕ доставки (grace=10m) → должен ВЫЖИТЬ.
	seedVoyageTerminal(t, ctx, "01HVOYAGEFRESH00000000000A", "archon-r", now.Add(-1*time.Minute))
	seedEphemeralTiding(t, ctx, "eph-fresh", "ops", "01HVOYAGEFRESH00000000000A")
	// (b) терминал 20m назад — за grace → должен быть СНЕСЁН.
	seedVoyageTerminal(t, ctx, "01HVOYAGEOLD0000000000000B", "archon-r", now.Add(-20*time.Minute))
	seedEphemeralTiding(t, ctx, "eph-old", "ops", "01HVOYAGEOLD0000000000000B")
	// (c) voyage не существует (orphan) → должен быть СНЕСЁН.
	seedEphemeralTiding(t, ctx, "eph-orphan", "ops", "01HVOYAGEGONE000000000000C")
	// (d) running voyage → не терминал → должен ВЫЖИТЬ.
	seedVoyageRunning(t, ctx, "01HVOYAGERUN0000000000000D", "archon-r")
	seedEphemeralTiding(t, ctx, "eph-running", "ops", "01HVOYAGERUN0000000000000D")
	// (e) постоянное правило → НИКОГДА не трогается.
	seedPersistentTiding(t, ctx, "persistent-rule", "ops")

	p := reaper.NewEphemeralTidingsPurger(integrationPool, discardLogger())
	affected, err := p.Run(ctx, 10*time.Minute, 1000)
	if err != nil {
		t.Fatalf("purger.Run: %v", err)
	}
	if affected != 2 {
		t.Errorf("affected = %d, want 2 (eph-old + eph-orphan)", affected)
	}

	// Guard доставки: свежий терминал (в окне grace) ОБЯЗАН выжить — иначе снос
	// опередил бы tap-dispatcher и уведомление о завершении не ушло бы.
	if !tidingExists(t, ctx, "eph-fresh") {
		t.Error("eph-fresh снесён ПРЕЖДЕВРЕМЕННО (терминал в пределах grace) — уведомление могло не уйти")
	}
	if !tidingExists(t, ctx, "eph-running") {
		t.Error("eph-running снесён — running не терминал, сносить нельзя")
	}
	if !tidingExists(t, ctx, "persistent-rule") {
		t.Error("постоянное правило снесено — purger трогает только ephemeral")
	}
	if tidingExists(t, ctx, "eph-old") {
		t.Error("eph-old НЕ снесён (терминал за пределами grace) — осиротевшее правило протекло")
	}
	if tidingExists(t, ctx, "eph-orphan") {
		t.Error("eph-orphan НЕ снесён (прогон не существует) — осиротевшее правило протекло")
	}
}

// TestIntegration_PurgeOrphanEphemeralTidings_NoGraceWindow — при grace=0
// (граничный) свежий терминал тоже сносится: подтверждает, что именно grace
// защищает окно доставки, а не что-то иное. Держим как контрапункт основному
// guard-у (grace — обязательный, не косметический, параметр корректности).
func TestIntegration_PurgeOrphanEphemeralTidings_NoGraceWindow(t *testing.T) {
	ctx := context.Background()
	resetHeraldVoyageTables(t, ctx)
	seedOperatorRaw(t, ctx, "archon-r")
	seedHeraldRaw(t, ctx, "ops", "archon-r")

	now := time.Now().UTC()
	seedVoyageTerminal(t, ctx, "01HVOYAGEJUST0000000000000E", "archon-r", now.Add(-1*time.Second))
	seedEphemeralTiding(t, ctx, "eph-just", "ops", "01HVOYAGEJUST0000000000000E")

	p := reaper.NewEphemeralTidingsPurger(integrationPool, discardLogger())
	// grace=0 → terminal 1s назад уже «старше» (finished_at < NOW() - 0) → снос.
	if _, err := p.Run(ctx, 0, 1000); err != nil {
		t.Fatalf("purger.Run: %v", err)
	}
	if tidingExists(t, ctx, "eph-just") {
		t.Error("при grace=0 свежий терминал должен быть снесён (доказывает, что окно держит именно grace)")
	}
}
