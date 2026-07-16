//go:build integration

package reaper_test

// End-to-end integration for rule reap_orphan_vault_keys (live Vault + PG).
// Write a private key to Vault under secret/keeper/sigil-keys/<key_id> and
// verify:
//   - secret without a row in sigil_signing_keys + aged created_time -> 1 orphan;
//   - secret with a live PG row -> 0 orphans.
//
// Vault container is started in TestMain on a best-effort basis; the test skips
// when it is absent.

import (
	"context"
	"log/slog"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/souls-guild/soul-stack/keeper/internal/reaper"
	"github.com/souls-guild/soul-stack/keeper/internal/sigil"
)

// sigilKeysReader adapts pool to the reaper live-keys-reader dependency
// (sigil.ListAllKeyIDs over pool). Mirrors the wiring glue in cmd/keeper.
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

// seedSigilSecret writes a private key to Vault under secret/keeper/sigil-keys/<keyID>.
func seedSigilSecret(t *testing.T, ctx context.Context, keyID string) {
	t.Helper()
	kv := integrationVaultAPI.KVv2("secret")
	if _, err := kv.Put(ctx, "keeper/sigil-keys/"+keyID, map[string]any{
		"signing_key": "test-private-key-pem",
	}); err != nil {
		t.Fatalf("seed vault sigil-key %s: %v", keyID, err)
	}
}

// seedSigilRow inserts a sigil_signing_keys row with the given status.
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

// resetSigilState clears the PG registry AND Vault sigil-keys secrets between
// tests. Vault cleanup is required because the container is shared by the
// package; otherwise secrets from the previous subtest would count as orphans in
// the next one.
func resetSigilState(t *testing.T, ctx context.Context, pool *pgxpool.Pool) {
	t.Helper()
	if _, err := pool.Exec(ctx, "TRUNCATE sigil_signing_keys"); err != nil {
		t.Fatalf("truncate sigil_signing_keys: %v", err)
	}
	// Fully delete metadata for each secret under sigil-keys/. DeleteMetadata
	// removes the secret with all versions, so LIST no longer returns it.
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

// TestIntegration_ReapOrphan_DetectsOrphan: secret in Vault without PG row,
// aged beyond grace, makes the rule report exactly 1.
func TestIntegration_ReapOrphan_DetectsOrphan(t *testing.T) {
	requireVaultIntegration(t)
	pool := fixturePool(t)
	ctx := context.Background()
	resetSigilState(t, ctx, pool)

	const orphanKey = "orphan-key-detect"
	seedSigilSecret(t, ctx, orphanKey)

	// now() is in the future relative to secret created_time, so the secret is old.
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

// TestIntegration_ReapOrphan_LivePGRow_NotOrphan: secret with live row in
// sigil_signing_keys (active) yields 0 orphans.
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

// TestIntegration_ReapOrphan_RetiredPGRow_NotOrphan: retired row is also live
// because ListAllKeyIDs includes all statuses, so there are 0 orphans.
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

// TestIntegration_ReapOrphan_WithinGrace_NotOrphan: fresh secret without PG row
// but inside grace is not an orphan because Introduce may be racing.
func TestIntegration_ReapOrphan_WithinGrace_NotOrphan(t *testing.T) {
	requireVaultIntegration(t)
	pool := fixturePool(t)
	ctx := context.Background()
	resetSigilState(t, ctx, pool)

	const freshKey = "fresh-orphan-candidate"
	seedSigilSecret(t, ctx, freshKey)

	// now() = real time: secret was just written and is younger than 24h grace.
	vr := newReconciler(pool, time.Now)

	got, err := vr.ReportOrphanVaultKeys(ctx, 24*time.Hour, 100)
	if err != nil {
		t.Fatalf("ReportOrphanVaultKeys: %v", err)
	}
	if got != 0 {
		t.Errorf("fresh candidate within grace must not be orphan, got %d", got)
	}
}
