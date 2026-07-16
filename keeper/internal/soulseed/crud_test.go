package soulseed

import (
	"context"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"errors"
	"math/big"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

type fakeDB struct {
	execCalls    int
	execTag      pgconn.CommandTag
	execErr      error
	lastExecSQL  string
	lastExecArgs []any

	queryCalls   int
	lastQuerySQL string
	rowFunc      func() pgx.Row
}

func (f *fakeDB) Exec(_ context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	f.execCalls++
	f.lastExecSQL = sql
	f.lastExecArgs = args
	return f.execTag, f.execErr
}

func (f *fakeDB) QueryRow(_ context.Context, sql string, _ ...any) pgx.Row {
	f.queryCalls++
	f.lastQuerySQL = sql
	if f.rowFunc != nil {
		return f.rowFunc()
	}
	return errRow{err: pgx.ErrNoRows}
}

func (f *fakeDB) Query(_ context.Context, _ string, _ ...any) (pgx.Rows, error) {
	return nil, errors.New("fakeDB: Query not used in tests")
}

type errRow struct{ err error }

func (r errRow) Scan(_ ...any) error { return r.err }

type insertRow struct {
	seedID   string
	issuedAt time.Time
	err      error
}

func (r insertRow) Scan(dest ...any) error {
	if r.err != nil {
		return r.err
	}
	if len(dest) != 2 {
		return errors.New("insertRow: want 2 dest")
	}
	*(dest[0].(*string)) = r.seedID
	*(dest[1].(*time.Time)) = r.issuedAt
	return nil
}

// --- FingerprintFromCert ---

func TestFingerprintFromCert_StableAndHexFormat(t *testing.T) {
	// Minimal "certificate" with RawSubjectPublicKeyInfo filled.
	// Valid ASN.1 is not needed for this test; the function computes SHA-256
	// over bytes of this field.
	cert := &x509.Certificate{
		RawSubjectPublicKeyInfo: []byte("public-key-bytes"),
		SerialNumber:            big.NewInt(1),
	}
	fp := FingerprintFromCert(cert)
	sum := sha256.Sum256(cert.RawSubjectPublicKeyInfo)
	if fp != hex.EncodeToString(sum[:]) {
		t.Errorf("fingerprint = %q, want %q", fp, hex.EncodeToString(sum[:]))
	}
	if len(fp) != FingerprintHexLen {
		t.Errorf("fingerprint len = %d, want %d", len(fp), FingerprintHexLen)
	}
}

// --- ValidFingerprintFormat ---

func TestValidFingerprintFormat(t *testing.T) {
	good := []string{strings.Repeat("0", 64), strings.Repeat("f", 64)}
	bad := []string{
		"",
		strings.Repeat("0", 63),
		strings.Repeat("0", 65),
		strings.Repeat("Z", 64), // not hex.
		strings.Repeat("A", 64), // uppercase.
	}
	for _, s := range good {
		if !ValidFingerprintFormat(s) {
			t.Errorf("ValidFingerprintFormat(%q) = false; want true", s)
		}
	}
	for _, s := range bad {
		if ValidFingerprintFormat(s) {
			t.Errorf("ValidFingerprintFormat(%q) = true; want false", s)
		}
	}
}

// --- Insert ---

func makeSeed() *SoulSeed {
	return &SoulSeed{
		SID:          "host.example.com",
		Fingerprint:  strings.Repeat("a", 64),
		SerialNumber: "01:23:45:67",
		ExpiresAt:    time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC),
	}
}

func TestInsert_HappyPath(t *testing.T) {
	now := time.Date(2026, 5, 22, 12, 0, 0, 0, time.UTC)
	f := &fakeDB{
		rowFunc: func() pgx.Row {
			return insertRow{seedID: "00000000-0000-0000-0000-000000000001", issuedAt: now}
		},
	}
	s := makeSeed()
	if err := Insert(context.Background(), f, s); err != nil {
		t.Fatalf("Insert: %v", err)
	}
	if s.SeedID == "" {
		t.Errorf("SeedID empty")
	}
	if !s.IssuedAt.Equal(now) {
		t.Errorf("IssuedAt = %v, want %v", s.IssuedAt, now)
	}
	if s.Status != StatusActive {
		t.Errorf("default Status = %q, want %q", s.Status, StatusActive)
	}
	if !strings.Contains(f.lastQuerySQL, "INSERT INTO soul_seeds") {
		t.Errorf("SQL: %q", f.lastQuerySQL)
	}
}

func TestInsert_RejectsBadFingerprint(t *testing.T) {
	f := &fakeDB{}
	s := makeSeed()
	s.Fingerprint = "short"
	err := Insert(context.Background(), f, s)
	if !errors.Is(err, ErrSeedInvalidFingerprint) {
		t.Errorf("err = %v, want ErrSeedInvalidFingerprint", err)
	}
	if f.queryCalls != 0 {
		t.Errorf("queryCalls = %d; want 0", f.queryCalls)
	}
}

func TestInsert_RejectsZeroExpiresAt(t *testing.T) {
	f := &fakeDB{}
	s := makeSeed()
	s.ExpiresAt = time.Time{}
	if err := Insert(context.Background(), f, s); err == nil {
		t.Fatal("Insert with zero ExpiresAt returned nil err")
	}
}

func TestInsert_RejectsEmptySerial(t *testing.T) {
	f := &fakeDB{}
	s := makeSeed()
	s.SerialNumber = ""
	if err := Insert(context.Background(), f, s); err == nil {
		t.Fatal("Insert with empty serial returned nil err")
	}
}

func TestInsert_MapsActiveExistsToSentinel(t *testing.T) {
	f := &fakeDB{
		rowFunc: func() pgx.Row {
			return insertRow{err: &pgconn.PgError{
				Code:           pgErrCodeUniqueViolation,
				ConstraintName: "soul_seeds_active_by_sid_idx",
			}}
		},
	}
	err := Insert(context.Background(), f, makeSeed())
	if !errors.Is(err, ErrSeedActiveExists) {
		t.Errorf("err = %v, want ErrSeedActiveExists", err)
	}
}

func TestInsert_MapsFingerprintCollisionToSentinel(t *testing.T) {
	f := &fakeDB{
		rowFunc: func() pgx.Row {
			return insertRow{err: &pgconn.PgError{
				Code:           pgErrCodeUniqueViolation,
				ConstraintName: "soul_seeds_fingerprint_idx",
			}}
		},
	}
	err := Insert(context.Background(), f, makeSeed())
	if !errors.Is(err, ErrSeedFingerprintCollision) {
		t.Errorf("err = %v, want ErrSeedFingerprintCollision", err)
	}
}

func TestInsert_MapsFKToSoulNotFound(t *testing.T) {
	f := &fakeDB{
		rowFunc: func() pgx.Row {
			return insertRow{err: &pgconn.PgError{
				Code:           pgErrCodeForeignKeyViolation,
				ConstraintName: "soul_seeds_sid_fk",
			}}
		},
	}
	err := Insert(context.Background(), f, makeSeed())
	if !errors.Is(err, ErrSeedSoulNotFound) {
		t.Errorf("err = %v, want ErrSeedSoulNotFound", err)
	}
}

// --- SupersedeBySID ---

func TestSupersedeBySID_NoActiveIsOK(t *testing.T) {
	f := &fakeDB{execTag: pgconn.NewCommandTag("UPDATE 0")}
	if err := SupersedeBySID(context.Background(), f, "host.example.com"); err != nil {
		t.Errorf("Supersede with no active seed should return nil, got %v", err)
	}
}

func TestSupersedeBySID_RejectsEmptySID(t *testing.T) {
	f := &fakeDB{}
	if err := SupersedeBySID(context.Background(), f, ""); err == nil {
		t.Fatal("SupersedeBySID with empty SID returned nil err")
	}
}

// --- OrphanActiveBySID (ADR-017 cascade) ---

func TestOrphanActiveBySID_HappyPath(t *testing.T) {
	f := &fakeDB{execTag: pgconn.NewCommandTag("UPDATE 1")}
	n, err := OrphanActiveBySID(context.Background(), f, "host.example.com")
	if err != nil {
		t.Fatalf("OrphanActiveBySID: %v", err)
	}
	if n != 1 {
		t.Errorf("rows-affected = %d, want 1", n)
	}
	if !strings.Contains(f.lastExecSQL, "soul_seeds") {
		t.Errorf("SQL=%q", f.lastExecSQL)
	}
	if !strings.Contains(f.lastExecSQL, "status = 'active'") {
		t.Errorf("SQL must filter active seeds (revoked > orphaned precedence); got %q", f.lastExecSQL)
	}
	if !strings.Contains(f.lastExecSQL, "'orphaned'") {
		t.Errorf("SQL must set status='orphaned'; got %q", f.lastExecSQL)
	}
}

func TestOrphanActiveBySID_NoActive_IsOK(t *testing.T) {
	f := &fakeDB{execTag: pgconn.NewCommandTag("UPDATE 0")}
	n, err := OrphanActiveBySID(context.Background(), f, "host.example.com")
	if err != nil {
		t.Fatalf("OrphanActiveBySID: %v", err)
	}
	if n != 0 {
		t.Errorf("rows-affected = %d, want 0 (push-host / never onboarded)", n)
	}
}

func TestOrphanActiveBySID_RejectsEmptySID(t *testing.T) {
	f := &fakeDB{}
	if _, err := OrphanActiveBySID(context.Background(), f, ""); err == nil {
		t.Fatal("OrphanActiveBySID with empty sid returned nil err")
	}
	if f.execCalls != 0 {
		t.Errorf("execCalls=%d; want 0 (validation before round-trip)", f.execCalls)
	}
}

// --- Revoke ---

func TestRevoke_AffectedRowsReturned(t *testing.T) {
	f := &fakeDB{execTag: pgconn.NewCommandTag("UPDATE 1")}
	n, err := Revoke(context.Background(), f, "seed-1", "compromised")
	if err != nil {
		t.Fatalf("Revoke: %v", err)
	}
	if n != 1 {
		t.Errorf("affected = %d, want 1", n)
	}
}

func TestRevoke_RejectsEmptySeedID(t *testing.T) {
	f := &fakeDB{}
	if _, err := Revoke(context.Background(), f, "", "x"); err == nil {
		t.Fatal("Revoke with empty seed_id returned nil err")
	}
}

// --- SelectByFingerprint ---

func TestSelectByFingerprint_RejectsBadFP(t *testing.T) {
	f := &fakeDB{}
	if _, err := SelectByFingerprint(context.Background(), f, "short"); !errors.Is(err, ErrSeedInvalidFingerprint) {
		t.Errorf("err = %v, want ErrSeedInvalidFingerprint", err)
	}
}

func TestSelectByFingerprint_NoRowsMapsToNotFound(t *testing.T) {
	f := &fakeDB{rowFunc: func() pgx.Row { return errRow{err: pgx.ErrNoRows} }}
	_, err := SelectByFingerprint(context.Background(), f, strings.Repeat("a", 64))
	if !errors.Is(err, ErrSeedNotFound) {
		t.Errorf("err = %v, want ErrSeedNotFound", err)
	}
}

// --- SelectAll ---

func TestSelectAll_RejectsBadPagination(t *testing.T) {
	f := &fakeDB{}
	if _, _, err := SelectAll(context.Background(), f, ListFilter{}, -1, 10); err == nil {
		t.Fatal("offset=-1 returned nil err")
	}
	if _, _, err := SelectAll(context.Background(), f, ListFilter{}, 0, 0); err == nil {
		t.Fatal("limit=0 returned nil err")
	}
}
