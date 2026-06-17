//go:build integration

package reaper_test

// End-to-end integration правила reap_orphan_vault_keys (live Vault + PG).
// Записываем приватник в Vault под secret/keeper/sigil-keys/<key_id> и проверяем:
//   - секрет без строки в sigil_signing_keys + состаренный created_time → 1 сирота;
//   - секрет с живой PG-строкой → 0 сирот.
//
// Vault-контейнер поднимается в TestMain (best-effort); при его отсутствии тест
// skip-ится.

import (
	"context"
	"log/slog"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/souls-guild/soul-stack/keeper/internal/reaper"
	"github.com/souls-guild/soul-stack/keeper/internal/sigil"
)

// sigilKeysReader адаптирует pool под reaper-зависимость live-keys-reader
// (sigil.ListAllKeyIDs над pool). Зеркалит wiring-склейку в cmd/keeper.
type sigilKeysReader struct{ pool *pgxpool.Pool }

func (r sigilKeysReader) ListAllKeyIDs(ctx context.Context) (map[string]struct{}, error) {
	return sigil.ListAllKeyIDs(ctx, r.pool)
}

func requireVaultIntegration(t *testing.T) {
	t.Helper()
	if integrationVaultClient == nil {
		t.Skip("vault integration client is nil (docker unavailable, REQUIRE_DOCKER not set)")
	}
}

// seedSigilSecret кладёт приватник в Vault под secret/keeper/sigil-keys/<keyID>.
func seedSigilSecret(t *testing.T, ctx context.Context, keyID string) {
	t.Helper()
	kv := integrationVaultAPI.KVv2("secret")
	if _, err := kv.Put(ctx, "keeper/sigil-keys/"+keyID, map[string]any{
		"signing_key": "test-private-key-pem",
	}); err != nil {
		t.Fatalf("seed vault sigil-key %s: %v", keyID, err)
	}
}

// seedSigilRow вставляет строку sigil_signing_keys с заданным статусом.
func seedSigilRow(t *testing.T, ctx context.Context, pool *pgxpool.Pool, keyID, status string) {
	t.Helper()
	isPrimary := status == "active"
	const q = `INSERT INTO sigil_signing_keys
		(key_id, pubkey_pem, vault_ref, is_primary, status)
		VALUES ($1, $2, $3, $4, $5)`
	if _, err := pool.Exec(ctx, q, keyID, "test-pubkey-pem", "secret/keeper/sigil-keys/"+keyID, isPrimary, status); err != nil {
		t.Fatalf("seed sigil_signing_keys %s (%s): %v", keyID, status, err)
	}
}

// resetSigilState очищает реестр PG И Vault-секреты sigil-keys между тестами.
// Vault-очистка обязательна: контейнер общий на пакет, секреты от предыдущего
// подтеста иначе считались бы сиротами в следующем.
func resetSigilState(t *testing.T, ctx context.Context, pool *pgxpool.Pool) {
	t.Helper()
	if _, err := pool.Exec(ctx, "TRUNCATE sigil_signing_keys"); err != nil {
		t.Fatalf("truncate sigil_signing_keys: %v", err)
	}
	// Полностью удаляем metadata каждого секрета под sigil-keys/ (DeleteMetadata
	// сносит секрет со всеми версиями — LIST его больше не отдаёт).
	names, err := integrationVaultClient.ListKV(ctx, "keeper/sigil-keys")
	if err != nil {
		t.Fatalf("list vault sigil-keys for reset: %v", err)
	}
	kv := integrationVaultAPI.KVv2("secret")
	for _, name := range names {
		if err := kv.DeleteMetadata(ctx, "keeper/sigil-keys/"+name); err != nil {
			t.Fatalf("delete vault metadata %s: %v", name, err)
		}
	}
}

func newReconciler(pool *pgxpool.Pool, now func() time.Time) *reaper.VaultReconciler {
	return reaper.NewVaultReconciler(
		integrationVaultClient,
		sigilKeysReader{pool: pool},
		slog.Default(),
		now,
	)
}

// TestIntegration_ReapOrphan_DetectsOrphan — секрет в Vault без PG-строки,
// состаренный за пределы grace → правило репортит ровно 1.
func TestIntegration_ReapOrphan_DetectsOrphan(t *testing.T) {
	requireVaultIntegration(t)
	pool := fixturePool(t)
	ctx := context.Background()
	resetSigilState(t, ctx, pool)

	const orphanKey = "orphan-key-detect"
	seedSigilSecret(t, ctx, orphanKey)

	// now() в будущем относительно created_time секрета → секрет «старый».
	future := func() time.Time { return time.Now().Add(48 * time.Hour) }
	vr := newReconciler(pool, future)

	got, err := vr.ReportOrphanVaultKeys(ctx, 24*time.Hour, 100)
	if err != nil {
		t.Fatalf("ReportOrphanVaultKeys: %v", err)
	}
	if got != 1 {
		t.Errorf("want 1 orphan, got %d", got)
	}
}

// TestIntegration_ReapOrphan_LivePGRow_NotOrphan — секрет с живой строкой в
// sigil_signing_keys (active) → 0 сирот.
func TestIntegration_ReapOrphan_LivePGRow_NotOrphan(t *testing.T) {
	requireVaultIntegration(t)
	pool := fixturePool(t)
	ctx := context.Background()
	resetSigilState(t, ctx, pool)

	const liveKey = "live-key-active"
	seedSigilSecret(t, ctx, liveKey)
	seedSigilRow(t, ctx, pool, liveKey, "active")

	future := func() time.Time { return time.Now().Add(48 * time.Hour) }
	vr := newReconciler(pool, future)

	got, err := vr.ReportOrphanVaultKeys(ctx, 24*time.Hour, 100)
	if err != nil {
		t.Fatalf("ReportOrphanVaultKeys: %v", err)
	}
	if got != 0 {
		t.Errorf("live key must not be orphan, got %d", got)
	}
}

// TestIntegration_ReapOrphan_RetiredPGRow_NotOrphan — retired-строка тоже живая
// (ListAllKeyIDs все статусы) → 0 сирот.
func TestIntegration_ReapOrphan_RetiredPGRow_NotOrphan(t *testing.T) {
	requireVaultIntegration(t)
	pool := fixturePool(t)
	ctx := context.Background()
	resetSigilState(t, ctx, pool)

	const retiredKey = "retired-key-still-live"
	seedSigilSecret(t, ctx, retiredKey)
	seedSigilRow(t, ctx, pool, retiredKey, "retired")

	future := func() time.Time { return time.Now().Add(48 * time.Hour) }
	vr := newReconciler(pool, future)

	got, err := vr.ReportOrphanVaultKeys(ctx, 24*time.Hour, 100)
	if err != nil {
		t.Fatalf("ReportOrphanVaultKeys: %v", err)
	}
	if got != 0 {
		t.Errorf("retired key (still live) must not be orphan, got %d", got)
	}
}

// TestIntegration_ReapOrphan_WithinGrace_NotOrphan — свежий секрет без PG-строки,
// но внутри grace → не сирота (гонка Introduce).
func TestIntegration_ReapOrphan_WithinGrace_NotOrphan(t *testing.T) {
	requireVaultIntegration(t)
	pool := fixturePool(t)
	ctx := context.Background()
	resetSigilState(t, ctx, pool)

	const freshKey = "fresh-orphan-candidate"
	seedSigilSecret(t, ctx, freshKey)

	// now() = реальное время: секрет только что записан, моложе grace 24h.
	vr := newReconciler(pool, time.Now)

	got, err := vr.ReportOrphanVaultKeys(ctx, 24*time.Hour, 100)
	if err != nil {
		t.Fatalf("ReportOrphanVaultKeys: %v", err)
	}
	if got != 0 {
		t.Errorf("fresh candidate within grace must not be orphan, got %d", got)
	}
}
