//go:build integration

// Integration-тесты sigil-Store (CRUD plugin_sigils) через testcontainers-go
// (postgres:16-alpine). Паттерн совпадает с keeper/internal/operator/integration_test.go.

package sigil

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"log"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"

	"github.com/souls-guild/soul-stack/keeper/internal/migrate"
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
			log.Fatalf("sigil integration: setup failed (REQUIRE_DOCKER): %v", err)
		}
		log.Printf("sigil integration: skipping, docker unavailable: %v", err)
		return 0
	}
	defer func() {
		termCtx, termCancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer termCancel()
		_ = ctr.Terminate(termCtx)
	}()

	dsn, err := ctr.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		log.Printf("sigil integration: ConnectionString: %v", err)
		return 1
	}
	if err := migrate.Apply(ctx, dsn, migrations.FS, "."); err != nil {
		log.Printf("sigil integration: migrate.Apply: %v", err)
		return 1
	}
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		log.Printf("sigil integration: pgxpool.New: %v", err)
		return 1
	}
	defer pool.Close()
	integrationPool = pool

	return m.Run()
}

// reset стирает plugin_sigils и пере-засевает оператора, на которого ссылаются
// FK allowed_by_aid / revoked_by_aid.
func reset(t *testing.T) string {
	t.Helper()
	ctx := context.Background()
	_, err := integrationPool.Exec(ctx, `TRUNCATE TABLE plugin_sigils RESTART IDENTITY`)
	if err != nil {
		t.Fatalf("TRUNCATE plugin_sigils: %v", err)
	}
	const aid = "archon-sigil-test"
	_, err = integrationPool.Exec(ctx,
		`INSERT INTO operators (aid, display_name, auth_method, created_by_aid)
		 VALUES ($1, 'Sigil Test', 'jwt', NULL)
		 ON CONFLICT (aid) DO NOTHING`, aid)
	if err != nil {
		t.Fatalf("seed operator: %v", err)
	}
	return aid
}

// rawManifestYAML — сырые байты manifest.yaml (канон). Заведомо НЕ совпадают с
// JSONB-проекцией ниже (другой синтаксис, перевод строк), чтобы round-trip
// доказывал: manifest_raw отдаётся byte-exact, а не из JSONB-колонки.
var rawManifestYAML = []byte("kind: cloud_driver\nname: hetzner\n")

func newRecord(aid string) *Sigil {
	digest := sha256.Sum256([]byte("binary-bytes"))
	return &Sigil{
		Namespace:    "cloud",
		Name:         "hetzner",
		Ref:          "v1.0.0",
		SHA256:       hex.EncodeToString(digest[:]),
		Signature:    ed25519.Sign(genIntegrationKey(), []byte("block")),
		ManifestRaw:  rawManifestYAML,
		Manifest:     []byte(`{"kind":"cloud_driver","name":"hetzner"}`),
		AllowedByAID: aid,
	}
}

func genIntegrationKey() ed25519.PrivateKey {
	_, priv, _ := ed25519.GenerateKey(nil)
	return priv
}

func TestIntegration_Insert_GetActive(t *testing.T) {
	aid := reset(t)
	ctx := context.Background()

	rec := newRecord(aid)
	if err := Insert(ctx, integrationPool, rec); err != nil {
		t.Fatalf("Insert: %v", err)
	}
	if rec.ID == 0 {
		t.Error("Insert did not populate ID")
	}
	if rec.AllowedAt.IsZero() {
		t.Error("Insert did not populate AllowedAt")
	}

	got, err := GetActive(ctx, integrationPool, "cloud", "hetzner", "v1.0.0")
	if err != nil {
		t.Fatalf("GetActive: %v", err)
	}
	if got.SHA256 != rec.SHA256 {
		t.Errorf("SHA256 = %q, want %q", got.SHA256, rec.SHA256)
	}
	if !bytes.Equal(got.Signature, rec.Signature) {
		t.Error("signature roundtrip mismatch")
	}
	// manifest_raw round-trip: byte-exact исходным сырым байтам (канон verify).
	if !bytes.Equal(got.ManifestRaw, rawManifestYAML) {
		t.Errorf("manifest_raw roundtrip:\n got=%q\nwant=%q", got.ManifestRaw, rawManifestYAML)
	}
	// JSONB-проекция непуста и отлична от raw (производный слой, не канон).
	if len(got.Manifest) == 0 {
		t.Error("manifest JSONB пуст")
	}
	if bytes.Equal(got.ManifestRaw, got.Manifest) {
		t.Error("manifest_raw совпал с JSONB manifest — raw обязан нести сырой YAML")
	}
	if got.RevokedAt != nil {
		t.Error("fresh record should be active")
	}
}

// TestIntegration_GetActive_GuardEmptyManifestRaw — Insert на реальном PG-пути
// отклоняет пустой ManifestRaw (guard до запроса), запись не создаётся.
func TestIntegration_Insert_GuardEmptyManifestRaw(t *testing.T) {
	aid := reset(t)
	ctx := context.Background()

	rec := newRecord(aid)
	rec.ManifestRaw = nil
	if err := Insert(ctx, integrationPool, rec); err == nil {
		t.Fatal("Insert с пустым ManifestRaw должен вернуть ошибку")
	}
	if _, err := GetActive(ctx, integrationPool, "cloud", "hetzner", "v1.0.0"); !errors.Is(err, ErrSigilNotFound) {
		t.Errorf("после отклонённого Insert активной записи быть не должно, err = %v", err)
	}
}

// TestIntegration_ListActive_ManifestRaw — ListActive отдаёт manifest_raw
// byte-exact (его читает S6-sender/broadcast).
func TestIntegration_ListActive_ManifestRaw(t *testing.T) {
	aid := reset(t)
	ctx := context.Background()

	if err := Insert(ctx, integrationPool, newRecord(aid)); err != nil {
		t.Fatalf("Insert: %v", err)
	}
	list, err := ListActive(ctx, integrationPool)
	if err != nil {
		t.Fatalf("ListActive: %v", err)
	}
	if len(list) != 1 {
		t.Fatalf("ListActive returned %d, want 1", len(list))
	}
	if !bytes.Equal(list[0].ManifestRaw, rawManifestYAML) {
		t.Errorf("ListActive manifest_raw:\n got=%q\nwant=%q", list[0].ManifestRaw, rawManifestYAML)
	}
}

// TestIntegration_CommitSha_RoundTrip — non-пустая CommitSHA сохраняется и
// читается byte-exact через GetActive/ListActive (audit-метка происхождения,
// миграция 038 / ADR-026(g)). Verify это поле НЕ использует — здесь чисто
// store-round-trip audit-слоя.
func TestIntegration_CommitSha_RoundTrip(t *testing.T) {
	aid := reset(t)
	ctx := context.Background()

	const commit = "1234567890abcdef1234567890abcdef12345678"
	rec := newRecord(aid)
	rec.CommitSHA = commit
	if err := Insert(ctx, integrationPool, rec); err != nil {
		t.Fatalf("Insert: %v", err)
	}

	got, err := GetActive(ctx, integrationPool, "cloud", "hetzner", "v1.0.0")
	if err != nil {
		t.Fatalf("GetActive: %v", err)
	}
	if got.CommitSHA != commit {
		t.Errorf("GetActive CommitSHA = %q, want %q", got.CommitSHA, commit)
	}

	list, err := ListActive(ctx, integrationPool)
	if err != nil {
		t.Fatalf("ListActive: %v", err)
	}
	if len(list) != 1 {
		t.Fatalf("ListActive returned %d, want 1", len(list))
	}
	if list[0].CommitSHA != commit {
		t.Errorf("ListActive CommitSHA = %q, want %q", list[0].CommitSHA, commit)
	}
}

// TestIntegration_CommitSha_LegacyNull — пустая CommitSHA при Insert ложится в БД
// NULL-ом (NULLIF) и читается обратно как "" (COALESCE). Покрывает legacy
// operator-asserted / до-S4-allow-путь: происхождение неизвестно.
func TestIntegration_CommitSha_LegacyNull(t *testing.T) {
	aid := reset(t)
	ctx := context.Background()

	rec := newRecord(aid) // CommitSHA не задан → ""
	if err := Insert(ctx, integrationPool, rec); err != nil {
		t.Fatalf("Insert: %v", err)
	}
	// На DB-уровне колонка должна быть именно NULL (а не пустая строка).
	var isNull bool
	if err := integrationPool.QueryRow(ctx,
		`SELECT commit_sha IS NULL FROM plugin_sigils WHERE id = $1`, rec.ID,
	).Scan(&isNull); err != nil {
		t.Fatalf("probe commit_sha IS NULL: %v", err)
	}
	if !isNull {
		t.Error("пустая CommitSHA должна писаться в БД как NULL (NULLIF)")
	}

	got, err := GetActive(ctx, integrationPool, "cloud", "hetzner", "v1.0.0")
	if err != nil {
		t.Fatalf("GetActive: %v", err)
	}
	if got.CommitSHA != "" {
		t.Errorf("legacy NULL должен читаться как \"\", got %q", got.CommitSHA)
	}
}

func TestIntegration_DuplicateActive(t *testing.T) {
	aid := reset(t)
	ctx := context.Background()

	if err := Insert(ctx, integrationPool, newRecord(aid)); err != nil {
		t.Fatalf("first Insert: %v", err)
	}
	err := Insert(ctx, integrationPool, newRecord(aid))
	if !errors.Is(err, ErrSigilAlreadyActive) {
		t.Fatalf("second Insert err = %v, want ErrSigilAlreadyActive", err)
	}
}

func TestIntegration_Revoke(t *testing.T) {
	aid := reset(t)
	ctx := context.Background()

	if err := Insert(ctx, integrationPool, newRecord(aid)); err != nil {
		t.Fatalf("Insert: %v", err)
	}
	if err := Revoke(ctx, integrationPool, "cloud", "hetzner", "v1.0.0", aid); err != nil {
		t.Fatalf("Revoke: %v", err)
	}
	// После revoke активной записи нет.
	if _, err := GetActive(ctx, integrationPool, "cloud", "hetzner", "v1.0.0"); !errors.Is(err, ErrSigilNotFound) {
		t.Errorf("GetActive after revoke err = %v, want ErrSigilNotFound", err)
	}
	// Повторный revoke → not found.
	if err := Revoke(ctx, integrationPool, "cloud", "hetzner", "v1.0.0", aid); !errors.Is(err, ErrSigilNotFound) {
		t.Errorf("second Revoke err = %v, want ErrSigilNotFound", err)
	}
}

func TestIntegration_ReAllowAfterRevoke(t *testing.T) {
	aid := reset(t)
	ctx := context.Background()

	first := newRecord(aid)
	if err := Insert(ctx, integrationPool, first); err != nil {
		t.Fatalf("first Insert: %v", err)
	}
	if err := Revoke(ctx, integrationPool, "cloud", "hetzner", "v1.0.0", aid); err != nil {
		t.Fatalf("Revoke: %v", err)
	}
	// Re-allow после revoke — новый INSERT, partial-unique не мешает.
	second := newRecord(aid)
	if err := Insert(ctx, integrationPool, second); err != nil {
		t.Fatalf("re-allow Insert: %v", err)
	}
	if second.ID == first.ID {
		t.Error("re-allow should produce a new row id")
	}
	got, err := GetActive(ctx, integrationPool, "cloud", "hetzner", "v1.0.0")
	if err != nil {
		t.Fatalf("GetActive after re-allow: %v", err)
	}
	if got.ID != second.ID {
		t.Errorf("active id = %d, want re-allowed %d", got.ID, second.ID)
	}
}

func TestIntegration_ListActive(t *testing.T) {
	aid := reset(t)
	ctx := context.Background()

	r1 := newRecord(aid)
	r1.Name = "aws"
	r2 := newRecord(aid)
	r2.Name = "gcp"
	r3 := newRecord(aid)
	r3.Name = "azure"
	for _, r := range []*Sigil{r1, r2, r3} {
		if err := Insert(ctx, integrationPool, r); err != nil {
			t.Fatalf("Insert %s: %v", r.Name, err)
		}
	}
	// Отзываем один — он не должен попасть в ListActive.
	if err := Revoke(ctx, integrationPool, "cloud", "gcp", "v1.0.0", aid); err != nil {
		t.Fatalf("Revoke gcp: %v", err)
	}

	list, err := ListActive(ctx, integrationPool)
	if err != nil {
		t.Fatalf("ListActive: %v", err)
	}
	if len(list) != 2 {
		t.Fatalf("ListActive returned %d, want 2", len(list))
	}
	for _, s := range list {
		if s.Name == "gcp" {
			t.Error("revoked record appeared in ListActive")
		}
		if s.RevokedAt != nil {
			t.Error("ListActive returned a revoked record")
		}
	}
}

func TestIntegration_GetActive_NotFound(t *testing.T) {
	reset(t)
	if _, err := GetActive(context.Background(), integrationPool, "cloud", "nope", "v1"); !errors.Is(err, ErrSigilNotFound) {
		t.Errorf("err = %v, want ErrSigilNotFound", err)
	}
}
