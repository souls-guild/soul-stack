//go:build integration

// Integration tests for sigil-Store (CRUD plugin_sigils) via testcontainers-go
// (postgres:16-alpine). Pattern matches keeper/internal/operator/integration_test.go.

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

// reset truncates plugin_sigils and re-seeds operator referenced by
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

// rawSchemaDoc is the byte-exact canonical schema document the signature covers. It is
// stored as BYTEA rather than JSONB precisely so this round-trips byte for byte: a
// JSONB column would re-serialize it, and the bytes a Soul re-hashes at verify would no
// longer be the bytes Keeper signed.
var rawSchemaDoc = []byte(`{"kind":"cloud_driver","profile_schema":{"type":"object"},"protocol_version":1}`)

func newRecord(aid string) *Sigil {
	digest := sha256.Sum256([]byte("binary-bytes"))
	return &Sigil{
		Alias:        "hetzner",
		Source:       testSource,
		Ref:          "v1.0.0",
		SHA256:       hex.EncodeToString(digest[:]),
		Signature:    ed25519.Sign(genIntegrationKey(), []byte("block")),
		Schema:       rawSchemaDoc,
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

	got, err := GetActive(ctx, integrationPool, "hetzner")
	if err != nil {
		t.Fatalf("GetActive: %v", err)
	}
	if got.Alias != "hetzner" || got.Source != testSource || got.Ref != "v1.0.0" {
		t.Errorf("identity roundtrip = (%q,%q,%q)", got.Alias, got.Source, got.Ref)
	}
	if got.SHA256 != rec.SHA256 {
		t.Errorf("SHA256 = %q, want %q", got.SHA256, rec.SHA256)
	}
	if !bytes.Equal(got.Signature, rec.Signature) {
		t.Error("signature roundtrip mismatch")
	}
	// schema round-trips byte-exact — the canon for verify.
	if !bytes.Equal(got.Schema, rawSchemaDoc) {
		t.Errorf("schema roundtrip:\n got=%q\nwant=%q", got.Schema, rawSchemaDoc)
	}
	if got.RevokedAt != nil {
		t.Error("fresh record should be active")
	}
}

// TestIntegration_Insert_GuardEmptySchema verifies that on the real PG path Insert
// rejects an empty Schema (a guard before the query) and creates no row.
func TestIntegration_Insert_GuardEmptySchema(t *testing.T) {
	aid := reset(t)
	ctx := context.Background()

	rec := newRecord(aid)
	rec.Schema = nil
	if err := Insert(ctx, integrationPool, rec); err == nil {
		t.Fatal("Insert with an empty schema must return an error")
	}
	if _, err := GetActive(ctx, integrationPool, "hetzner"); !errors.Is(err, ErrSigilNotFound) {
		t.Errorf("after a rejected Insert there must be no active record, err = %v", err)
	}
}

// TestIntegration_Insert_RejectsReservedAliasAtSchemaLevel — the reserved list is
// enforced in Go, but the ALIAS SHAPE is also a CHECK constraint, so a path-shaped or
// uppercase alias cannot reach the table even if a caller skipped the service.
func TestIntegration_Insert_RejectsMalformedAliasAtSchemaLevel(t *testing.T) {
	aid := reset(t)
	ctx := context.Background()

	for _, alias := range []string{"Redis", "redis.acl", "a/b", "9lives"} {
		rec := newRecord(aid)
		rec.Alias = alias
		if err := Insert(ctx, integrationPool, rec); err == nil {
			t.Errorf("alias %q was accepted by the schema", alias)
		}
	}
}

// TestIntegration_ListActive_Schema verifies ListActive returns the schema byte-exact
// (this is what the S6 broadcast sends to Souls).
func TestIntegration_ListActive_Schema(t *testing.T) {
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
	if !bytes.Equal(list[0].Schema, rawSchemaDoc) {
		t.Errorf("ListActive schema:\n got=%q\nwant=%q", list[0].Schema, rawSchemaDoc)
	}
}

// TestIntegration_CommitSha_RoundTrip verifies non-empty CommitSHA is persisted
// and read byte-exact via GetActive/ListActive (audit origin marker,
// migration 038 / ADR-026(g)). Verify does not use this field — pure
// store round-trip of audit layer.
func TestIntegration_CommitSha_RoundTrip(t *testing.T) {
	aid := reset(t)
	ctx := context.Background()

	const commit = "1234567890abcdef1234567890abcdef12345678"
	rec := newRecord(aid)
	rec.CommitSHA = commit
	if err := Insert(ctx, integrationPool, rec); err != nil {
		t.Fatalf("Insert: %v", err)
	}

	got, err := GetActive(ctx, integrationPool, "hetzner")
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

// TestIntegration_CommitSha_LegacyNull verifies empty CommitSHA on Insert goes to DB
// as NULL (NULLIF) and reads back as "" (COALESCE). Covers legacy
// operator-asserted / pre-S4-allow path: origin unknown.
func TestIntegration_CommitSha_LegacyNull(t *testing.T) {
	aid := reset(t)
	ctx := context.Background()

	rec := newRecord(aid) // CommitSHA not set → ""
	if err := Insert(ctx, integrationPool, rec); err != nil {
		t.Fatalf("Insert: %v", err)
	}
	// At DB level column must be exactly NULL (not empty string).
	var isNull bool
	if err := integrationPool.QueryRow(ctx,
		`SELECT commit_sha IS NULL FROM plugin_sigils WHERE id = $1`, rec.ID,
	).Scan(&isNull); err != nil {
		t.Fatalf("probe commit_sha IS NULL: %v", err)
	}
	if !isNull {
		t.Error("empty CommitSHA must be written to DB as NULL (NULLIF)")
	}

	got, err := GetActive(ctx, integrationPool, "hetzner")
	if err != nil {
		t.Fatalf("GetActive: %v", err)
	}
	if got.CommitSHA != "" {
		t.Errorf("legacy NULL must read as \"\", got %q", got.CommitSHA)
	}
}

// TestIntegration_DuplicateActiveSourceRef — the TRUST key. Approving the same
// (source, ref) twice is a conflict even under a different alias: an approval must not
// be silently duplicated, or revoking one copy would leave the other standing.
func TestIntegration_DuplicateActiveSourceRef(t *testing.T) {
	aid := reset(t)
	ctx := context.Background()

	if err := Insert(ctx, integrationPool, newRecord(aid)); err != nil {
		t.Fatalf("first Insert: %v", err)
	}
	second := newRecord(aid)
	second.Alias = "hetzner-mirror"
	err := Insert(ctx, integrationPool, second)
	if !errors.Is(err, ErrSigilAlreadyActive) {
		t.Fatalf("second Insert err = %v, want ErrSigilAlreadyActive", err)
	}
}

// TestIntegration_DuplicateActiveAlias — the REGISTRATION invariant. One alias is one
// address space and one host slot, so a second live grant may not claim it. Reported as
// a DIFFERENT sentinel: the operator's fix is a different alias, not a revoke.
func TestIntegration_DuplicateActiveAlias(t *testing.T) {
	aid := reset(t)
	ctx := context.Background()

	if err := Insert(ctx, integrationPool, newRecord(aid)); err != nil {
		t.Fatalf("first Insert: %v", err)
	}
	second := newRecord(aid)
	second.Source = "https://example.com/somebody-else.git"
	second.Ref = "v2.0.0"
	err := Insert(ctx, integrationPool, second)
	if !errors.Is(err, ErrAliasAlreadyRegistered) {
		t.Fatalf("second Insert err = %v, want ErrAliasAlreadyRegistered", err)
	}
}

func TestIntegration_Revoke(t *testing.T) {
	aid := reset(t)
	ctx := context.Background()

	if err := Insert(ctx, integrationPool, newRecord(aid)); err != nil {
		t.Fatalf("Insert: %v", err)
	}
	if err := Revoke(ctx, integrationPool, "hetzner", aid); err != nil {
		t.Fatalf("Revoke: %v", err)
	}
	// After revoke no active record exists.
	if _, err := GetActive(ctx, integrationPool, "hetzner"); !errors.Is(err, ErrSigilNotFound) {
		t.Errorf("GetActive after revoke err = %v, want ErrSigilNotFound", err)
	}
	// Re-revoke → not found.
	if err := Revoke(ctx, integrationPool, "hetzner", aid); !errors.Is(err, ErrSigilNotFound) {
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
	if err := Revoke(ctx, integrationPool, "hetzner", aid); err != nil {
		t.Fatalf("Revoke: %v", err)
	}
	// Re-allow after revoke — a plain INSERT; both partial-unique indexes count only
	// active rows, so revoked history never gets in the way.
	second := newRecord(aid)
	if err := Insert(ctx, integrationPool, second); err != nil {
		t.Fatalf("re-allow Insert: %v", err)
	}
	if second.ID == first.ID {
		t.Error("re-allow should produce a new row id")
	}
	got, err := GetActive(ctx, integrationPool, "hetzner")
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

	mk := func(alias string) *Sigil {
		r := newRecord(aid)
		r.Alias = alias
		// Distinct sources too: the trust key is (source, ref), so three grants under
		// one source would collide on it rather than on the alias.
		r.Source = "https://example.com/soul-cloud-" + alias + ".git"
		return r
	}
	for _, r := range []*Sigil{mk("aws"), mk("gcp"), mk("azure")} {
		if err := Insert(ctx, integrationPool, r); err != nil {
			t.Fatalf("Insert %s: %v", r.Alias, err)
		}
	}
	// Revoke one — it should not appear in ListActive.
	if err := Revoke(ctx, integrationPool, "gcp", aid); err != nil {
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
		if s.Alias == "gcp" {
			t.Error("revoked record appeared in ListActive")
		}
		if s.RevokedAt != nil {
			t.Error("ListActive returned a revoked record")
		}
	}
}

func TestIntegration_GetActive_NotFound(t *testing.T) {
	reset(t)
	if _, err := GetActive(context.Background(), integrationPool, "nope"); !errors.Is(err, ErrSigilNotFound) {
		t.Errorf("err = %v, want ErrSigilNotFound", err)
	}
}

// TestIntegration_Revoke_DoesNotMatchAcrossIdentities is the guard for the revoke
// KEY choice. Revoke takes the ALIAS — the name the operator registered, the one
// every address in their destinies uses, and the one thing they hold when they
// decide a plugin should stop running. It must therefore be impossible for a
// revoke to reach a row it was not aimed at.
//
// Two ways that could go wrong, both covered here:
//
//   - revoking alias A must not touch a live grant held under alias B, even when
//     B is the same artifact identity (source, ref). "Revoke redis" must mean that
//     one registration and no other;
//   - after an alias is re-pointed (revoke, then approve the same alias on a
//     DIFFERENT artifact), revoking it must hit the CURRENT row. The old row stays
//     revoked with its own audit trail rather than being reachable a second time.
//
// The second case is what makes the alias safe as a revoke key at all: the partial
// unique index means at most one live row ever holds an alias, so "the active grant
// for this alias" is never a choice between candidates.
func TestIntegration_Revoke_DoesNotMatchAcrossIdentities(t *testing.T) {
	aid := reset(t)
	ctx := context.Background()

	// Two live grants, distinct aliases AND distinct sources (the trust key forbids
	// sharing a source+ref between live rows).
	a := newRecord(aid)
	a.Alias, a.Source = "redis", "https://example.com/redis.git"
	b := newRecord(aid)
	b.Alias, b.Source = "redis-community", "https://example.com/redis-community.git"
	for _, r := range []*Sigil{a, b} {
		if err := Insert(ctx, integrationPool, r); err != nil {
			t.Fatalf("Insert %s: %v", r.Alias, err)
		}
	}

	// Revoking one alias leaves the other completely untouched.
	if err := Revoke(ctx, integrationPool, "redis", aid); err != nil {
		t.Fatalf("Revoke redis: %v", err)
	}
	if _, err := GetActive(ctx, integrationPool, "redis"); !errors.Is(err, ErrSigilNotFound) {
		t.Errorf("redis still active after revoke: %v", err)
	}
	other, err := GetActive(ctx, integrationPool, "redis-community")
	if err != nil {
		t.Fatalf("revoking one alias took down another: %v", err)
	}
	if other.ID != b.ID || other.Source != b.Source {
		t.Errorf("surviving grant = %+v, want the untouched redis-community row", other)
	}

	// Re-point the freed alias onto a DIFFERENT artifact, then revoke it again: the
	// revoke must hit the new row, and the old one must keep its own revocation.
	repointed := newRecord(aid)
	repointed.Alias, repointed.Source = "redis", "https://example.com/redis-fork.git"
	if err := Insert(ctx, integrationPool, repointed); err != nil {
		t.Fatalf("re-point redis: %v", err)
	}
	live, err := GetActive(ctx, integrationPool, "redis")
	if err != nil {
		t.Fatalf("GetActive after re-point: %v", err)
	}
	if live.ID != repointed.ID {
		t.Fatalf("GetActive returned id=%d, want the re-pointed row %d — the alias lookup is ambiguous", live.ID, repointed.ID)
	}
	if err := Revoke(ctx, integrationPool, "redis", aid); err != nil {
		t.Fatalf("Revoke re-pointed redis: %v", err)
	}

	// The original row's audit trail survived the second revoke untouched: revoking
	// an alias must not rewrite the history of a grant that already ended.
	var revokedCount int
	if err := integrationPool.QueryRow(ctx,
		`SELECT count(*) FROM plugin_sigils WHERE alias = 'redis' AND revoked_at IS NOT NULL`,
	).Scan(&revokedCount); err != nil {
		t.Fatalf("count revoked redis rows: %v", err)
	}
	if revokedCount != 2 {
		t.Errorf("revoked redis rows = %d, want 2 (the original and the re-pointed one, each with its own record)", revokedCount)
	}
}
