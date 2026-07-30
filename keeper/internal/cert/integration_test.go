//go:build integration

// L1 integration for the Warrant CRUD layer (NIM-313, sibling of the
// pushprovider suite from NIM-239).
//
// WHY. This package owns four SQL statements against `warrant` and had no live
// Postgres behind any of them. `crud_test.go` runs entirely on a fake, and the
// two tests there that look like they cover the interesting half —
// TestInsert_MapsActiveExistsToSentinel and TestInsert_MapsFKToIncarnationNotFound
// — FABRICATE a *pgconn.PgError with the constraint name typed into the test.
// They pin mapInsertError against a name written down twice in Go, which catches
// an edit to the Go side. What they cannot catch is an edit to the MIGRATION:
// rename `warrant_active_by_incarnation_kind_idx` in 092 and both stay green
// while the handler's 409 silently becomes a 500. That direction is the likely
// one — a migration gets rewritten and the Go constant does not follow.
//
// Two more things only a real database can answer, and neither is exercised
// anywhere in the repo:
//
//   - `rotate_threshold_override` is a PG INTERVAL written from a
//     *time.Duration. No test in the tree ever sets that field non-nil, so the
//     encode/scan round-trip is unverified. pgx 5.10's interval codec does not
//     know time.Duration; it reaches Postgres only through the fmt.Stringer
//     fallback, i.e. as the text "24h0m0s". Whether Postgres parses that is a
//     property of Postgres, not of Go.
//   - the partial unique index only permits supersede-then-insert in that
//     order. RegisterActive's whole reason to exist is that ordering, and a fake
//     tx accepts either.
package cert

import (
	"context"
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
			log.Fatalf("cert integration: setup failed (docker required): %v", err)
		}
		log.Printf("cert integration: skipping, docker unavailable: %v", err)
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

const (
	testAID         = "archon-cert-it"
	testIncarnation = "redis-eu"
	testFingerprint = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
)

// resetAll truncates and re-seeds the operator and the incarnation that
// `warrant_incarnation_fk` points at.
func resetAll(t *testing.T) {
	t.Helper()
	ctx := context.Background()
	if _, err := integrationPool.Exec(ctx,
		`TRUNCATE TABLE warrant, incarnation, operators CASCADE`); err != nil {
		t.Fatalf("TRUNCATE: %v", err)
	}
	if _, err := integrationPool.Exec(ctx, `
		INSERT INTO operators (aid, display_name, auth_method, created_by_aid)
		VALUES ($1, 'Cert IT', 'jwt', NULL)`, testAID); err != nil {
		t.Fatalf("seed operator: %v", err)
	}
	seedIncarnation(t, testIncarnation)
}

func seedIncarnation(t *testing.T, name string) {
	t.Helper()
	if _, err := integrationPool.Exec(context.Background(), `
		INSERT INTO incarnation (name, service, service_version, status, created_by_aid)
		VALUES ($1, 'redis', 'v1.0.0', 'ready', $2)`, name, testAID); err != nil {
		t.Fatalf("seed incarnation(%s): %v", name, err)
	}
}

// fp builds a distinct 64-lower-hex fingerprint from a one-character suffix, so
// rows in the same test are distinguishable without repeating the literal.
func fp(suffix byte) string {
	return testFingerprint[:FingerprintHexLen-1] + string(suffix)
}

func newWarrant(kind Kind, serial string) *Warrant {
	return &Warrant{
		IncarnationID: testIncarnation,
		Kind:          kind,
		VaultRef:      "secret/keeper/warrant/" + testIncarnation + "/" + string(kind),
		SerialNumber:  serial,
		Fingerprint:   testFingerprint,
		NotAfter:      time.Now().UTC().Add(90 * 24 * time.Hour).Truncate(time.Microsecond),
		AutoRotate:    true,
	}
}

// TestIntegration_Warrant_RoundTrip — the baseline a CRUD L1 exists for: every
// column comes back the way it went in, and the DB defaults fire.
//
// The INTERVAL column is the interesting half and gets its own test below; here
// it stays nil so a failure localises to the ordinary columns.
func TestIntegration_Warrant_RoundTrip(t *testing.T) {
	resetAll(t)
	ctx := context.Background()

	mount, role, kid := "pki_int", "redis-server", "keeper-01"
	in := newWarrant(KindCert, "3f:aa:01")
	in.PKIMount = &mount
	in.PKIRole = &role
	in.IssuedByKID = &kid

	if err := Insert(ctx, integrationPool, in); err != nil {
		t.Fatalf("Insert: %v", err)
	}
	if in.CertID == "" {
		t.Error("CertID is empty — the RETURNING clause did not deliver the generated UUID")
	}
	if in.IssuedAt.IsZero() {
		t.Error("IssuedAt is zero — COALESCE($7, NOW()) did not default")
	}

	got, err := SelectActive(ctx, integrationPool, testIncarnation, KindCert)
	if err != nil {
		t.Fatalf("SelectActive: %v", err)
	}
	if got.CertID != in.CertID {
		t.Errorf("CertID = %q, want %q", got.CertID, in.CertID)
	}
	if got.IncarnationID != testIncarnation {
		t.Errorf("IncarnationID = %q, want %q", got.IncarnationID, testIncarnation)
	}
	// Kind and Status travel as TEXT and are re-typed on scan. Pinning them here
	// is what catches a column swap: `kind` and `status` are adjacent TEXT
	// columns in both the INSERT list and selectColumns, so transposing the pair
	// would still scan cleanly and only show up as a value mismatch (NIM-289).
	if got.Kind != KindCert {
		t.Errorf("Kind = %q, want %q", got.Kind, KindCert)
	}
	if got.Status != StatusActive {
		t.Errorf("Status = %q, want %q — the schema default is 'active'", got.Status, StatusActive)
	}
	// vault_ref and serial_number are likewise adjacent TEXT columns.
	if got.VaultRef != in.VaultRef {
		t.Errorf("VaultRef = %q, want %q", got.VaultRef, in.VaultRef)
	}
	if got.SerialNumber != "3f:aa:01" {
		t.Errorf("SerialNumber = %q, want 3f:aa:01", got.SerialNumber)
	}
	if got.Fingerprint != testFingerprint {
		t.Errorf("Fingerprint = %q, want %q", got.Fingerprint, testFingerprint)
	}
	if !got.NotAfter.Equal(in.NotAfter) {
		t.Errorf("NotAfter = %v, want %v", got.NotAfter, in.NotAfter)
	}
	if got.PKIMount == nil || *got.PKIMount != mount {
		t.Errorf("PKIMount = %v, want %q", got.PKIMount, mount)
	}
	if got.PKIRole == nil || *got.PKIRole != role {
		t.Errorf("PKIRole = %v, want %q", got.PKIRole, role)
	}
	if got.IssuedByKID == nil || *got.IssuedByKID != kid {
		t.Errorf("IssuedByKID = %v, want %q", got.IssuedByKID, kid)
	}
	if !got.AutoRotate {
		t.Error("AutoRotate = false, want true")
	}
	// The three nullable columns left unset must come back as NULL, not as a
	// zero value: `last_rotation_voyage_id` distinguishes "never rotated" from
	// "rotated by voyage \"\"", and the reaper reads it.
	if got.LastRotationVoyageID != nil {
		t.Errorf("LastRotationVoyageID = %q on a fresh row, want nil", *got.LastRotationVoyageID)
	}
	if got.RotateThresholdOverride != nil {
		t.Errorf("RotateThresholdOverride = %v on a fresh row, want nil", *got.RotateThresholdOverride)
	}
}

// TestIntegration_Warrant_RotateThresholdOverrideRoundTrip — the INTERVAL
// column, which nothing in the repo has ever written non-nil.
//
// `durationArg` hands pgx a bare time.Duration for a column Postgres types as
// INTERVAL. pgx 5.10's interval codec has no time.Duration plan, so the value
// only survives via the fmt.Stringer fallback ("24h0m0s") — whether Postgres
// accepts that spelling, and whether it scans back into *time.Duration with the
// same magnitude, is entirely a database property. A fake tx records the
// argument and reports success either way.
//
// This matters because the field is a per-warrant override of the reaper's
// rotation threshold. A silent failure is not a crash: it is a certificate that
// rotates on the cluster default instead of the tighter window an operator set,
// and expires while the dashboard says auto-rotate is on.
func TestIntegration_Warrant_RotateThresholdOverrideRoundTrip(t *testing.T) {
	resetAll(t)
	ctx := context.Background()

	// A value with hour, minute and second parts: a codec that truncates to
	// whole days or drops sub-minute precision fails here rather than passing by
	// accident on a round number.
	const override = 26*time.Hour + 30*time.Minute + 15*time.Second

	in := newWarrant(KindCert, "3f:aa:02")
	in.RotateThresholdOverride = ptrDuration(override)
	if err := Insert(ctx, integrationPool, in); err != nil {
		t.Fatalf("Insert with rotate_threshold_override=%v: %v", override, err)
	}

	got, err := SelectActive(ctx, integrationPool, testIncarnation, KindCert)
	if err != nil {
		t.Fatalf("SelectActive: %v", err)
	}
	if got.RotateThresholdOverride == nil {
		t.Fatal("RotateThresholdOverride = nil after writing a non-nil INTERVAL — the value did not survive the round-trip")
	}
	if *got.RotateThresholdOverride != override {
		t.Errorf("RotateThresholdOverride = %v, want %v", *got.RotateThresholdOverride, override)
	}
}

func ptrDuration(d time.Duration) *time.Duration { return &d }

// TestIntegration_Warrant_ActiveIsUniquePerIncarnationKind — the partial unique
// index is the invariant the whole package is built around ("exactly one active
// per (incarnation, kind)"), and the constraint NAME is what mapInsertError
// dispatches on to produce ErrActiveExists.
//
// The stub test asserts the mapping given a hand-written constraint name. This
// asserts Postgres produces that name. Rename the index and only this test
// notices.
func TestIntegration_Warrant_ActiveIsUniquePerIncarnationKind(t *testing.T) {
	resetAll(t)
	ctx := context.Background()

	first := newWarrant(KindCert, "3f:aa:03")
	if err := Insert(ctx, integrationPool, first); err != nil {
		t.Fatalf("first Insert: %v", err)
	}

	second := newWarrant(KindCert, "3f:aa:04")
	second.Fingerprint = fp('1')
	err := Insert(ctx, integrationPool, second)
	if !errors.Is(err, ErrActiveExists) {
		t.Fatalf("second active Insert: err = %v, want ErrActiveExists", err)
	}

	// The index is partial (WHERE status = 'active') and scoped to the pair. A
	// second row for a DIFFERENT kind, and a non-active row for the SAME kind,
	// must both be admitted — otherwise rotation history could not exist.
	otherKind := newWarrant(KindKey, "3f:aa:05")
	otherKind.Fingerprint = fp('2')
	if err := Insert(ctx, integrationPool, otherKind); err != nil {
		t.Errorf("Insert of a different kind: %v — the unique index is not scoped to (incarnation, kind)", err)
	}
	superseded := newWarrant(KindCert, "3f:aa:06")
	superseded.Fingerprint = fp('3')
	superseded.Status = StatusSuperseded
	if err := Insert(ctx, integrationPool, superseded); err != nil {
		t.Errorf("Insert of a superseded row for a live kind: %v — the unique index is not partial on status", err)
	}
}

// TestIntegration_Warrant_UnknownIncarnationMapsToSentinel — the FK half of the
// same argument: `warrant_incarnation_fk` is the name ErrIncarnationNotFound
// hangs off, and only the database says it.
func TestIntegration_Warrant_UnknownIncarnationMapsToSentinel(t *testing.T) {
	resetAll(t)
	ctx := context.Background()

	w := newWarrant(KindCert, "3f:aa:07")
	w.IncarnationID = "no-such-incarnation"
	err := Insert(ctx, integrationPool, w)
	if !errors.Is(err, ErrIncarnationNotFound) {
		t.Fatalf("Insert against a missing incarnation: err = %v, want ErrIncarnationNotFound", err)
	}
}

// TestIntegration_Warrant_SelectActiveNotFound — SelectActive must report
// absence as ErrNotFound, and must not answer with a row that merely exists.
// The handler maps that one sentinel to 404.
func TestIntegration_Warrant_SelectActiveNotFound(t *testing.T) {
	resetAll(t)
	ctx := context.Background()

	if _, err := SelectActive(ctx, integrationPool, testIncarnation, KindCert); !errors.Is(err, ErrNotFound) {
		t.Errorf("SelectActive on an empty registry: err = %v, want ErrNotFound", err)
	}

	// A row that exists but is not active is still "no active material": the
	// query's `status = 'active'` predicate is the thing under test, and a fake
	// row source cannot evaluate it.
	w := newWarrant(KindCert, "3f:aa:08")
	w.Status = StatusRotating
	if err := Insert(ctx, integrationPool, w); err != nil {
		t.Fatalf("Insert(rotating): %v", err)
	}
	if _, err := SelectActive(ctx, integrationPool, testIncarnation, KindCert); !errors.Is(err, ErrNotFound) {
		t.Errorf("SelectActive with only a rotating row: err = %v, want ErrNotFound", err)
	}
}

// TestIntegration_Warrant_MarkStatusIsCAS — the optimistic barrier. RowsAffected
// is a real command tag here, so "lost the race" is a fact about the database
// rather than about the fake's return value.
func TestIntegration_Warrant_MarkStatusIsCAS(t *testing.T) {
	resetAll(t)
	ctx := context.Background()

	w := newWarrant(KindCert, "3f:aa:09")
	if err := Insert(ctx, integrationPool, w); err != nil {
		t.Fatalf("Insert: %v", err)
	}

	// First claim wins: active → rotating.
	n, err := MarkStatus(ctx, integrationPool, w.CertID, StatusActive, StatusRotating)
	if err != nil {
		t.Fatalf("MarkStatus(active→rotating): %v", err)
	}
	if n != 1 {
		t.Fatalf("MarkStatus(active→rotating) affected %d rows, want 1", n)
	}

	// Second claim from the same expected status loses: the row is no longer
	// active. This is the single-winner barrier the reaper leans on.
	n, err = MarkStatus(ctx, integrationPool, w.CertID, StatusActive, StatusRotating)
	if err != nil {
		t.Fatalf("second MarkStatus: %v", err)
	}
	if n != 0 {
		t.Errorf("second MarkStatus(active→rotating) affected %d rows, want 0 — the CAS guard is not holding", n)
	}

	// The chain may then fail forward from the status it actually holds.
	n, err = MarkStatus(ctx, integrationPool, w.CertID, StatusRotating, StatusFailed)
	if err != nil {
		t.Fatalf("MarkStatus(rotating→failed): %v", err)
	}
	if n != 1 {
		t.Errorf("MarkStatus(rotating→failed) affected %d rows, want 1", n)
	}

	// An unknown cert_id is indistinguishable from a lost race by design: both
	// are 0 rows, and callers treat them alike.
	n, err = MarkStatus(ctx, integrationPool, "00000000-0000-0000-0000-000000000000", StatusActive, StatusRotating)
	if err != nil {
		t.Fatalf("MarkStatus on a missing cert_id: %v", err)
	}
	if n != 0 {
		t.Errorf("MarkStatus on a missing cert_id affected %d rows, want 0", n)
	}
}

// TestIntegration_Warrant_RegisterActiveRotates — RegisterActive exists solely
// because the partial unique index forbids two active rows for one
// (incarnation, kind), so supersede and insert have to share a transaction.
//
// A fake tx cannot fail the way this protects against, so this is the only place
// the ordering is actually verified: the old row must be superseded BEFORE the
// new one lands.
func TestIntegration_Warrant_RegisterActiveRotates(t *testing.T) {
	resetAll(t)
	ctx := context.Background()

	old := newWarrant(KindCert, "3f:aa:0a")
	if err := Insert(ctx, integrationPool, old); err != nil {
		t.Fatalf("Insert(old): %v", err)
	}

	fresh := newWarrant(KindCert, "3f:aa:0b")
	fresh.Fingerprint = fp('4')
	if err := RegisterActive(ctx, integrationPool, fresh); err != nil {
		t.Fatalf("RegisterActive: %v", err)
	}

	got, err := SelectActive(ctx, integrationPool, testIncarnation, KindCert)
	if err != nil {
		t.Fatalf("SelectActive after rotation: %v", err)
	}
	if got.CertID != fresh.CertID {
		t.Errorf("active CertID = %q, want the freshly registered %q", got.CertID, fresh.CertID)
	}
	if got.SerialNumber != "3f:aa:0b" {
		t.Errorf("active SerialNumber = %q, want 3f:aa:0b", got.SerialNumber)
	}

	// The old row is retained as history, moved to superseded — not deleted.
	var status string
	if err := integrationPool.QueryRow(ctx,
		`SELECT status FROM warrant WHERE cert_id = $1`, old.CertID).Scan(&status); err != nil {
		t.Fatalf("read old row: %v", err)
	}
	if status != string(StatusSuperseded) {
		t.Errorf("old row status = %q, want %q", status, StatusSuperseded)
	}

	// Exactly two rows: rotation is history-preserving, and nothing was
	// duplicated by the two statements sharing a tx.
	var total int
	if err := integrationPool.QueryRow(ctx,
		`SELECT COUNT(*) FROM warrant WHERE incarnation_id = $1 AND kind = 'cert'`,
		testIncarnation).Scan(&total); err != nil {
		t.Fatalf("count rows: %v", err)
	}
	if total != 2 {
		t.Errorf("row count = %d, want 2 (superseded + active)", total)
	}
}

// TestIntegration_Warrant_InsertBeforeSupersedeIsRejected — the mechanism behind
// RegisterActive, stated directly.
//
// Doing the two statements in the other order violates the partial unique index,
// so the ordering inside RegisterActive is load-bearing rather than stylistic.
// The test above is what CATCHES a reordering (its Insert would start failing
// with ErrActiveExists); this one records WHY, against the live index, so the
// next reader does not have to take the comment on faith.
//
// Note on what is deliberately absent: there is no test that a failed Insert
// rolls back the preceding supersede. RegisterActive does defer a Rollback, but
// the failure cannot be provoked from outside — every DB-side constraint on
// `warrant` (kind, status, fingerprint) is mirrored by a Go guard in Insert that
// returns before the tx is touched, and the one DB-only constraint
// (warrant_incarnation_fk) would make the supersede a no-op, leaving nothing to
// roll back. Reaching it needs fault injection, which would test pgx rather than
// this package.
func TestIntegration_Warrant_InsertBeforeSupersedeIsRejected(t *testing.T) {
	resetAll(t)
	ctx := context.Background()

	if err := Insert(ctx, integrationPool, newWarrant(KindCert, "3f:aa:0c")); err != nil {
		t.Fatalf("Insert(old): %v", err)
	}

	tx, err := integrationPool.Begin(ctx)
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	fresh := newWarrant(KindCert, "3f:aa:0d")
	fresh.Fingerprint = fp('5')
	if err := Insert(ctx, tx, fresh); !errors.Is(err, ErrActiveExists) {
		t.Fatalf("Insert before SupersedeActive: err = %v, want ErrActiveExists — "+
			"the partial unique index no longer forces the supersede-first ordering", err)
	}
}
