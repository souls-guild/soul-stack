package cert

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
	certID   string
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
	*(dest[0].(*string)) = r.certID
	*(dest[1].(*time.Time)) = r.issuedAt
	return nil
}

// --- FingerprintFromCert / ValidFingerprintFormat ---

func TestFingerprintFromCert_StableAndHexFormat(t *testing.T) {
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

func TestValidFingerprintFormat(t *testing.T) {
	good := []string{strings.Repeat("0", 64), strings.Repeat("f", 64)}
	bad := []string{"", strings.Repeat("0", 63), strings.Repeat("0", 65), strings.Repeat("Z", 64), strings.Repeat("A", 64)}
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

func makeWarrant() *Warrant {
	return &Warrant{
		IncarnationID: "redis-prod",
		Kind:          KindCert,
		VaultRef:      "secret/redis/redis-prod/tls/cert#cert",
		SerialNumber:  "01:23:45:67",
		Fingerprint:   strings.Repeat("a", 64),
		NotAfter:      time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC),
		AutoRotate:    true,
	}
}

func TestInsert_HappyPath(t *testing.T) {
	now := time.Date(2026, 6, 30, 12, 0, 0, 0, time.UTC)
	f := &fakeDB{
		rowFunc: func() pgx.Row {
			return insertRow{certID: "00000000-0000-0000-0000-000000000001", issuedAt: now}
		},
	}
	w := makeWarrant()
	if err := Insert(context.Background(), f, w); err != nil {
		t.Fatalf("Insert: %v", err)
	}
	if w.CertID == "" {
		t.Errorf("CertID empty")
	}
	if !w.IssuedAt.Equal(now) {
		t.Errorf("IssuedAt = %v, want %v", w.IssuedAt, now)
	}
	if w.Status != StatusActive {
		t.Errorf("default Status = %q, want %q", w.Status, StatusActive)
	}
	if !strings.Contains(f.lastQuerySQL, "INSERT INTO warrant") {
		t.Errorf("SQL: %q", f.lastQuerySQL)
	}
}

func TestInsert_RejectsBadFingerprint(t *testing.T) {
	f := &fakeDB{}
	w := makeWarrant()
	w.Fingerprint = "short"
	err := Insert(context.Background(), f, w)
	if !errors.Is(err, ErrInvalidFingerprint) {
		t.Errorf("err = %v, want ErrInvalidFingerprint", err)
	}
	if f.queryCalls != 0 {
		t.Errorf("queryCalls = %d; want 0", f.queryCalls)
	}
}

func TestInsert_RejectsBadKind(t *testing.T) {
	f := &fakeDB{}
	w := makeWarrant()
	w.Kind = "bogus"
	if err := Insert(context.Background(), f, w); err == nil {
		t.Fatal("Insert with bad kind returned nil err")
	}
	if f.queryCalls != 0 {
		t.Errorf("queryCalls = %d; want 0", f.queryCalls)
	}
}

func TestInsert_RejectsZeroNotAfter(t *testing.T) {
	f := &fakeDB{}
	w := makeWarrant()
	w.NotAfter = time.Time{}
	if err := Insert(context.Background(), f, w); err == nil {
		t.Fatal("Insert with zero NotAfter returned nil err")
	}
}

func TestInsert_RejectsEmptyVaultRef(t *testing.T) {
	f := &fakeDB{}
	w := makeWarrant()
	w.VaultRef = ""
	if err := Insert(context.Background(), f, w); err == nil {
		t.Fatal("Insert with empty vault_ref returned nil err")
	}
}

func TestInsert_MapsActiveExistsToSentinel(t *testing.T) {
	f := &fakeDB{
		rowFunc: func() pgx.Row {
			return insertRow{err: &pgconn.PgError{
				Code:           pgErrCodeUniqueViolation,
				ConstraintName: "warrant_active_by_incarnation_kind_idx",
			}}
		},
	}
	err := Insert(context.Background(), f, makeWarrant())
	if !errors.Is(err, ErrActiveExists) {
		t.Errorf("err = %v, want ErrActiveExists", err)
	}
}

func TestInsert_MapsFKToIncarnationNotFound(t *testing.T) {
	f := &fakeDB{
		rowFunc: func() pgx.Row {
			return insertRow{err: &pgconn.PgError{
				Code:           pgErrCodeForeignKeyViolation,
				ConstraintName: "warrant_incarnation_fk",
			}}
		},
	}
	err := Insert(context.Background(), f, makeWarrant())
	if !errors.Is(err, ErrIncarnationNotFound) {
		t.Errorf("err = %v, want ErrIncarnationNotFound", err)
	}
}

// --- SupersedeActive ---

func TestSupersedeActive_NoActiveIsOK(t *testing.T) {
	f := &fakeDB{execTag: pgconn.NewCommandTag("UPDATE 0")}
	if err := SupersedeActive(context.Background(), f, "redis-prod", KindCert); err != nil {
		t.Errorf("Supersede with no active should return nil, got %v", err)
	}
	if !strings.Contains(f.lastExecSQL, "status = 'superseded'") {
		t.Errorf("SQL must set superseded; got %q", f.lastExecSQL)
	}
	if !strings.Contains(f.lastExecSQL, "status = 'active'") {
		t.Errorf("SQL must filter active; got %q", f.lastExecSQL)
	}
}

func TestSupersedeActive_RejectsEmptyIncarnation(t *testing.T) {
	f := &fakeDB{}
	if err := SupersedeActive(context.Background(), f, "", KindCert); err == nil {
		t.Fatal("SupersedeActive with empty incarnation returned nil err")
	}
	if f.execCalls != 0 {
		t.Errorf("execCalls=%d; want 0 (validation before round-trip)", f.execCalls)
	}
}

// --- MarkStatus (CAS) ---

// TestMarkStatus_ActiveToRotating_HappyPath — успешный захват single-winner:
// active→rotating затрагивает ровно 1 строку.
func TestMarkStatus_ActiveToRotating_HappyPath(t *testing.T) {
	f := &fakeDB{execTag: pgconn.NewCommandTag("UPDATE 1")}
	n, err := MarkStatus(context.Background(), f, "cert-1", StatusActive, StatusRotating)
	if err != nil {
		t.Fatalf("MarkStatus: %v", err)
	}
	if n != 1 {
		t.Errorf("rows-affected = %d, want 1 (won the CAS)", n)
	}
	// CAS: WHERE cert_id AND status = from; SET status = to.
	if !strings.Contains(f.lastExecSQL, "WHERE cert_id = $1 AND status = $3") {
		t.Errorf("SQL must CAS on expected status; got %q", f.lastExecSQL)
	}
}

// TestMarkStatus_LostCASReturnsZero — проигравший гонку получает 0 (строка уже
// не в статусе from — другой тик/инстанс перехватил). Это опора single-winner-
// и idempotency-guard-ов: второй захват rotating не проходит.
func TestMarkStatus_LostCASReturnsZero(t *testing.T) {
	f := &fakeDB{execTag: pgconn.NewCommandTag("UPDATE 0")}
	n, err := MarkStatus(context.Background(), f, "cert-1", StatusActive, StatusRotating)
	if err != nil {
		t.Fatalf("MarkStatus: %v", err)
	}
	if n != 0 {
		t.Errorf("rows-affected = %d, want 0 (lost the CAS)", n)
	}
}

func TestMarkStatus_RejectsEmptyCertID(t *testing.T) {
	f := &fakeDB{}
	if _, err := MarkStatus(context.Background(), f, "", StatusActive, StatusRotating); err == nil {
		t.Fatal("MarkStatus with empty cert_id returned nil err")
	}
	if f.execCalls != 0 {
		t.Errorf("execCalls=%d; want 0", f.execCalls)
	}
}

func TestMarkStatus_RejectsBadStatus(t *testing.T) {
	f := &fakeDB{}
	if _, err := MarkStatus(context.Background(), f, "cert-1", "bogus", StatusRotating); err == nil {
		t.Fatal("MarkStatus with bad from-status returned nil err")
	}
}

// --- SelectActive ---

func TestSelectActive_NoRowsMapsToNotFound(t *testing.T) {
	f := &fakeDB{rowFunc: func() pgx.Row { return errRow{err: pgx.ErrNoRows} }}
	_, err := SelectActive(context.Background(), f, "redis-prod", KindCert)
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("err = %v, want ErrNotFound", err)
	}
}
