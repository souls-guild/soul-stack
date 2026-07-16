//go:build integration

package soulseed

import (
	"context"
	"errors"
	"log"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"

	"github.com/souls-guild/soul-stack/keeper/internal/migrate"
	"github.com/souls-guild/soul-stack/keeper/internal/soul"
	"github.com/souls-guild/soul-stack/keeper/migrations"
)

var integrationPool *pgxpool.Pool

func TestMain(m *testing.M) { os.Exit(run(m)) }

func run(m *testing.M) int {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	ctr, err := tcpostgres.Run(ctx,
		"postgres:16-alpine",
		tcpostgres.WithDatabase("keeper_test"),
		tcpostgres.WithUsername("keeper"),
		tcpostgres.WithPassword("keeper"),
		tcpostgres.BasicWaitStrategies(),
	)
	if err != nil {
		if requireDocker() {
			log.Fatalf("soulseed integration: setup failed (REQUIRE_DOCKER): %v", err)
		}
		log.Printf("soulseed integration: skipping, docker unavailable: %v", err)
		return 0
	}
	defer func() {
		termCtx, termCancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer termCancel()
		_ = ctr.Terminate(termCtx)
	}()

	dsn, err := ctr.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		log.Printf("ConnectionString: %v", err)
		return 1
	}
	if err := migrate.Apply(ctx, dsn, migrations.FS, "."); err != nil {
		log.Printf("migrate.Apply: %v", err)
		return 1
	}
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		log.Printf("pgxpool.New: %v", err)
		return 1
	}
	defer pool.Close()
	integrationPool = pool

	return m.Run()
}

func resetAll(t *testing.T) {
	t.Helper()
	_, err := integrationPool.Exec(context.Background(),
		`TRUNCATE TABLE soul_seeds, bootstrap_tokens, souls, operators, audit_log CASCADE`)
	if err != nil {
		t.Fatalf("TRUNCATE: %v", err)
	}
}

func seedSoul(t *testing.T, sid string) {
	t.Helper()
	s := &soul.Soul{SID: sid, Status: soul.StatusPending}
	if err := soul.Insert(context.Background(), integrationPool, s); err != nil {
		t.Fatalf("seedSoul(%s): %v", sid, err)
	}
}

func uniqueFingerprint(c byte) string {
	// 64 hex (c-byte powered): each test gets its own fingerprint, otherwise
	// UNIQUE on fingerprint breaks parallel tests.
	return strings.Repeat(string(c), 64)
}

func TestIntegration_Insert_AndSelectActive(t *testing.T) {
	resetAll(t)
	seedSoul(t, "host1.example.com")
	ctx := context.Background()
	kid := "kid-1"
	s := &SoulSeed{
		SID:          "host1.example.com",
		Fingerprint:  uniqueFingerprint('a'),
		SerialNumber: "01",
		ExpiresAt:    time.Now().Add(24 * time.Hour),
		IssuedByKID:  &kid,
	}
	if err := Insert(ctx, integrationPool, s); err != nil {
		t.Fatalf("Insert: %v", err)
	}
	got, err := SelectActiveBySID(ctx, integrationPool, "host1.example.com")
	if err != nil {
		t.Fatalf("SelectActiveBySID: %v", err)
	}
	if got.SeedID == "" || got.Status != StatusActive {
		t.Errorf("got = %+v", got)
	}
	if got.IssuedByKID == nil || *got.IssuedByKID != "kid-1" {
		t.Errorf("IssuedByKID = %v", got.IssuedByKID)
	}
}

func TestIntegration_Insert_RejectsSecondActiveForSameSID(t *testing.T) {
	resetAll(t)
	seedSoul(t, "host1.example.com")
	ctx := context.Background()
	s1 := &SoulSeed{
		SID:          "host1.example.com",
		Fingerprint:  uniqueFingerprint('a'),
		SerialNumber: "01",
		ExpiresAt:    time.Now().Add(24 * time.Hour),
	}
	if err := Insert(ctx, integrationPool, s1); err != nil {
		t.Fatalf("first Insert: %v", err)
	}
	s2 := &SoulSeed{
		SID:          "host1.example.com",
		Fingerprint:  uniqueFingerprint('b'),
		SerialNumber: "02",
		ExpiresAt:    time.Now().Add(24 * time.Hour),
	}
	err := Insert(ctx, integrationPool, s2)
	if !errors.Is(err, ErrSeedActiveExists) {
		t.Errorf("err = %v, want ErrSeedActiveExists", err)
	}
}

func TestIntegration_SupersedeAndInsertNewActive(t *testing.T) {
	resetAll(t)
	seedSoul(t, "host1.example.com")
	ctx := context.Background()
	s1 := &SoulSeed{
		SID:          "host1.example.com",
		Fingerprint:  uniqueFingerprint('a'),
		SerialNumber: "01",
		ExpiresAt:    time.Now().Add(24 * time.Hour),
	}
	if err := Insert(ctx, integrationPool, s1); err != nil {
		t.Fatalf("first Insert: %v", err)
	}
	// Rotation: supersede + insert new active.
	if err := SupersedeBySID(ctx, integrationPool, "host1.example.com"); err != nil {
		t.Fatalf("SupersedeBySID: %v", err)
	}
	s2 := &SoulSeed{
		SID:          "host1.example.com",
		Fingerprint:  uniqueFingerprint('b'),
		SerialNumber: "02",
		ExpiresAt:    time.Now().Add(24 * time.Hour),
	}
	if err := Insert(ctx, integrationPool, s2); err != nil {
		t.Fatalf("second Insert after Supersede: %v", err)
	}
	got, err := SelectActiveBySID(ctx, integrationPool, "host1.example.com")
	if err != nil {
		t.Fatalf("SelectActiveBySID: %v", err)
	}
	if got.SerialNumber != "02" {
		t.Errorf("active serial = %q, want 02", got.SerialNumber)
	}

	// History contains both seeds.
	all, total, err := SelectAll(ctx, integrationPool, ListFilter{SID: "host1.example.com"}, 0, 10)
	if err != nil {
		t.Fatalf("SelectAll: %v", err)
	}
	if total != 2 || len(all) != 2 {
		t.Errorf("total/len = %d/%d, want 2/2", total, len(all))
	}
}

func TestIntegration_SelectByFingerprint(t *testing.T) {
	resetAll(t)
	seedSoul(t, "host1.example.com")
	ctx := context.Background()
	fp := uniqueFingerprint('c')
	s := &SoulSeed{
		SID:          "host1.example.com",
		Fingerprint:  fp,
		SerialNumber: "01",
		ExpiresAt:    time.Now().Add(24 * time.Hour),
	}
	if err := Insert(ctx, integrationPool, s); err != nil {
		t.Fatalf("Insert: %v", err)
	}
	got, err := SelectByFingerprint(ctx, integrationPool, fp)
	if err != nil {
		t.Fatalf("SelectByFingerprint: %v", err)
	}
	if got.SID != "host1.example.com" {
		t.Errorf("SID = %q", got.SID)
	}
	if _, err := SelectByFingerprint(ctx, integrationPool, uniqueFingerprint('d')); !errors.Is(err, ErrSeedNotFound) {
		t.Errorf("err = %v, want ErrSeedNotFound", err)
	}
}

func TestIntegration_Revoke(t *testing.T) {
	resetAll(t)
	seedSoul(t, "host1.example.com")
	ctx := context.Background()
	s := &SoulSeed{
		SID:          "host1.example.com",
		Fingerprint:  uniqueFingerprint('e'),
		SerialNumber: "01",
		ExpiresAt:    time.Now().Add(24 * time.Hour),
	}
	if err := Insert(ctx, integrationPool, s); err != nil {
		t.Fatalf("Insert: %v", err)
	}
	n, err := Revoke(ctx, integrationPool, s.SeedID, "compromised")
	if err != nil {
		t.Fatalf("Revoke: %v", err)
	}
	if n != 1 {
		t.Errorf("affected = %d, want 1", n)
	}
	// active no longer exists.
	if _, err := SelectActiveBySID(ctx, integrationPool, "host1.example.com"); !errors.Is(err, ErrSeedNotFound) {
		t.Errorf("err = %v, want ErrSeedNotFound after revoke", err)
	}
	// Record by fingerprint remained with status='revoked'.
	got, err := SelectByFingerprint(ctx, integrationPool, s.Fingerprint)
	if err != nil {
		t.Fatalf("SelectByFingerprint: %v", err)
	}
	if got.Status != StatusRevoked {
		t.Errorf("status = %q, want revoked", got.Status)
	}
	if got.RevocationReason == nil || *got.RevocationReason != "compromised" {
		t.Errorf("RevocationReason = %v", got.RevocationReason)
	}
}

func TestIntegration_OrphanActiveBySID(t *testing.T) {
	// ADR-017 cascade: active seed moves to `orphaned`, revoked is NOT
	// overwritten (precedence revoked > orphaned).
	resetAll(t)
	seedSoul(t, "host-active.example.com")
	seedSoul(t, "host-revoked.example.com")
	ctx := context.Background()
	active := &SoulSeed{
		SID:          "host-active.example.com",
		Fingerprint:  uniqueFingerprint('0'),
		SerialNumber: "01",
		ExpiresAt:    time.Now().Add(24 * time.Hour),
	}
	if err := Insert(ctx, integrationPool, active); err != nil {
		t.Fatalf("Insert active: %v", err)
	}
	revoked := &SoulSeed{
		SID:          "host-revoked.example.com",
		Fingerprint:  uniqueFingerprint('1'),
		SerialNumber: "02",
		ExpiresAt:    time.Now().Add(24 * time.Hour),
	}
	if err := Insert(ctx, integrationPool, revoked); err != nil {
		t.Fatalf("Insert revoked: %v", err)
	}
	if _, err := Revoke(ctx, integrationPool, revoked.SeedID, "test"); err != nil {
		t.Fatalf("Revoke: %v", err)
	}

	// active → orphaned (n=1).
	n, err := OrphanActiveBySID(ctx, integrationPool, "host-active.example.com")
	if err != nil {
		t.Fatalf("OrphanActiveBySID active: %v", err)
	}
	if n != 1 {
		t.Errorf("rows = %d, want 1 for active host", n)
	}
	got, err := SelectByFingerprint(ctx, integrationPool, active.Fingerprint)
	if err != nil {
		t.Fatalf("SelectByFingerprint active: %v", err)
	}
	if got.Status != StatusOrphaned {
		t.Errorf("active status = %q, want orphaned", got.Status)
	}

	// revoked → revoked (cascade no-op, rows=0; precedence).
	n2, err := OrphanActiveBySID(ctx, integrationPool, "host-revoked.example.com")
	if err != nil {
		t.Fatalf("OrphanActiveBySID revoked: %v", err)
	}
	if n2 != 0 {
		t.Errorf("rows = %d, want 0 (revoked must not be overwritten)", n2)
	}
	gotR, err := SelectByFingerprint(ctx, integrationPool, revoked.Fingerprint)
	if err != nil {
		t.Fatalf("SelectByFingerprint revoked: %v", err)
	}
	if gotR.Status != StatusRevoked {
		t.Errorf("revoked seed status = %q, want revoked (orphaned must NOT overwrite)", gotR.Status)
	}
}

func TestIntegration_CascadeOnSoulDelete(t *testing.T) {
	resetAll(t)
	seedSoul(t, "host1.example.com")
	ctx := context.Background()
	s := &SoulSeed{
		SID:          "host1.example.com",
		Fingerprint:  uniqueFingerprint('f'),
		SerialNumber: "01",
		ExpiresAt:    time.Now().Add(24 * time.Hour),
	}
	if err := Insert(ctx, integrationPool, s); err != nil {
		t.Fatalf("Insert: %v", err)
	}
	if _, err := integrationPool.Exec(ctx, `DELETE FROM souls WHERE sid = $1`, "host1.example.com"); err != nil {
		t.Fatalf("DELETE soul: %v", err)
	}
	if _, err := SelectByFingerprint(ctx, integrationPool, s.Fingerprint); !errors.Is(err, ErrSeedNotFound) {
		t.Errorf("err = %v, want ErrSeedNotFound after CASCADE", err)
	}
}
