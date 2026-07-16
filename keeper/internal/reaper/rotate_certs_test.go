package reaper

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"math/big"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// --- fake pool/tx for CertRotator ---
//
// fakeCertDB models the minimal SQL set touched by CertRotator:
//   - SELECT ... FROM warrant ... FOR UPDATE SKIP LOCKED (due scan) -> Query.
//   - UPDATE warrant SET status ... WHERE cert_id ... AND status = ... (CAS) -> Exec.
//   - INSERT INTO warrant ... RETURNING cert_id, issued_at -> QueryRow.
//   - INSERT INTO voyages ... RETURNING created_at -> QueryRow.
//   - INSERT INTO voyage_targets ... -> Exec.
//   - UPDATE warrant SET status='superseded'/... (Supersede) -> Exec.
type fakeCertDB struct {
	dueRows [][]any // due scan rows, see selectDueCertsSQL columns

	// casResults is the RowsAffected sequence for UPDATE ... status CAS. The
	// first Exec with "AND status = $3" takes casResults[0], and so on. Empty
	// means 1 (always won). Models single-winner: 0 means lost the race.
	casResults []int64
	casIdx     int

	// fact counters for asserts.
	insertedWarrants int
	insertedVoyages  int
	insertedTargets  int
	supersedes       int
	casCalls         int
	markFailedCalls  int

	execErr    error // if set, all Exec calls fail, except scan Query
	insertVErr error // INSERT INTO voyages error
}

func (f *fakeCertDB) BeginTx(_ context.Context, _ pgx.TxOptions) (pgx.Tx, error) {
	return &fakeCertTx{db: f}, nil
}

func (f *fakeCertDB) exec(sql string) (pgconn.CommandTag, error) {
	switch {
	case strings.Contains(sql, "AND status = $3"):
		// CAS transition by cert_id. Distinguish target status for counters.
		f.casCalls++
		n := int64(1)
		if f.casIdx < len(f.casResults) {
			n = f.casResults[f.casIdx]
		}
		f.casIdx++
		if strings.Contains(sql, "SET status = $2") {
			// markFailed uses the same markStatusSQL; it cannot be distinguished by
			// context, so markFailedCalls is counted in a separate Exec path below
			// (rotating->failed goes exactly through CAS). Here only return
			// RowsAffected.
		}
		return pgconn.NewCommandTag("UPDATE " + itoaForTag(n)), nil
	case strings.Contains(sql, "SET status = 'superseded'"):
		f.supersedes++
		if f.execErr != nil {
			return pgconn.CommandTag{}, f.execErr
		}
		return pgconn.NewCommandTag("UPDATE 1"), nil
	}
	// voyage_targets are inserted through CopyFrom, see fakeCertTx.CopyFrom, not Exec.
	return pgconn.CommandTag{}, nil
}

func (f *fakeCertDB) queryRow(sql string) pgx.Row {
	switch {
	case strings.Contains(sql, "INSERT INTO warrant"):
		f.insertedWarrants++
		return certStaticRow{values: []any{"new-cert-id", time.Now()}}
	case strings.Contains(sql, "INSERT INTO voyages"):
		f.insertedVoyages++
		if f.insertVErr != nil {
			return certErrRow{err: f.insertVErr}
		}
		return certStaticRow{values: []any{time.Now()}}
	}
	return certErrRow{err: pgx.ErrNoRows}
}

func (f *fakeCertDB) query(sql string) (pgx.Rows, error) {
	if strings.Contains(sql, "FROM warrant") && strings.Contains(sql, "FOR UPDATE SKIP LOCKED") {
		return &fakeCertRows{rows: f.dueRows}, nil
	}
	return &fakeCertRows{}, nil
}

// fakeCertTx is a pgx.Tx wrapper delegated to fakeCertDB.
type fakeCertTx struct{ db *fakeCertDB }

func (t *fakeCertTx) Begin(context.Context) (pgx.Tx, error) { return t, nil }
func (t *fakeCertTx) Commit(context.Context) error          { return nil }
func (t *fakeCertTx) Rollback(context.Context) error        { return nil }

// CopyFrom lets voyage.InsertTargets batch targets through CopyFrom. Drain the
// source, count rows (one per target), and record the insertion fact.
func (t *fakeCertTx) CopyFrom(_ context.Context, _ pgx.Identifier, _ []string, src pgx.CopyFromSource) (int64, error) {
	var n int64
	for src.Next() {
		if _, err := src.Values(); err != nil {
			return n, err
		}
		n++
	}
	t.db.insertedTargets += int(n)
	return n, t.db.execErr
}
func (t *fakeCertTx) SendBatch(context.Context, *pgx.Batch) pgx.BatchResults {
	panic("unexpected SendBatch")
}
func (t *fakeCertTx) LargeObjects() pgx.LargeObjects { panic("unexpected LargeObjects") }
func (t *fakeCertTx) Prepare(context.Context, string, string) (*pgconn.StatementDescription, error) {
	panic("unexpected Prepare")
}
func (t *fakeCertTx) Exec(_ context.Context, sql string, _ ...any) (pgconn.CommandTag, error) {
	return t.db.exec(sql)
}
func (t *fakeCertTx) Query(_ context.Context, sql string, _ ...any) (pgx.Rows, error) {
	return t.db.query(sql)
}
func (t *fakeCertTx) QueryRow(_ context.Context, sql string, _ ...any) pgx.Row {
	return t.db.queryRow(sql)
}
func (t *fakeCertTx) Conn() *pgx.Conn { return nil }

// fakeCertRows is pgx.Rows over [][]any.
type fakeCertRows struct {
	rows [][]any
	idx  int
}

func (r *fakeCertRows) Next() bool {
	if r.idx >= len(r.rows) {
		return false
	}
	r.idx++
	return true
}

func (r *fakeCertRows) Scan(dest ...any) error {
	row := r.rows[r.idx-1]
	for i, d := range dest {
		switch dst := d.(type) {
		case *string:
			*dst = row[i].(string)
		case *time.Time:
			*dst = row[i].(time.Time)
		case **string:
			if row[i] == nil {
				*dst = nil
				continue
			}
			s := row[i].(string)
			*dst = &s
		}
	}
	return nil
}
func (r *fakeCertRows) Close()                                       {}
func (r *fakeCertRows) Err() error                                   { return nil }
func (r *fakeCertRows) CommandTag() pgconn.CommandTag                { return pgconn.CommandTag{} }
func (r *fakeCertRows) FieldDescriptions() []pgconn.FieldDescription { return nil }
func (r *fakeCertRows) Values() ([]any, error)                       { return nil, nil }
func (r *fakeCertRows) RawValues() [][]byte                          { return nil }
func (r *fakeCertRows) Conn() *pgx.Conn                              { return nil }

type certStaticRow struct{ values []any }

func (r certStaticRow) Scan(dest ...any) error {
	for i, d := range dest {
		switch dst := d.(type) {
		case *string:
			*dst = r.values[i].(string)
		case *time.Time:
			*dst = r.values[i].(time.Time)
		}
	}
	return nil
}

type certErrRow struct{ err error }

func (r certErrRow) Scan(_ ...any) error { return r.err }

// itoaForTag is reused from errand_purge_test.go in the same package.

// --- fake PKI signer / vault writer / csrgen ---

type fakeSigner struct {
	err  error
	cert []byte
}

func (s *fakeSigner) SignCSR(_ context.Context, _, _, _ string) (*SignedCert, error) {
	if s.err != nil {
		return nil, s.err
	}
	return &SignedCert{
		CertificatePEM: s.cert,
		SerialNumber:   "0a0b0c",
		NotAfter:       time.Now().Add(365 * 24 * time.Hour),
	}, nil
}

type fakeVaultWriter struct {
	err    error
	writes []string // write paths
}

func (w *fakeVaultWriter) WriteKV(_ context.Context, path string, _ map[string]any) error {
	w.writes = append(w.writes, path)
	return w.err
}

// makeTestCertPEM generates a self-signed cert for fakeSigner.cert / fingerprint.
func makeTestCertPEM(t *testing.T) []byte {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("rsa: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "svc.tls"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(365 * 24 * time.Hour),
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("create cert: %v", err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
}

// fakeCSRGen is a deterministic keypair+CSR. It is real enough that the SignCSR
// mock does not parse it; the private key is not used in asserts.
func fakeCSRGen(_ string, _ []string) (privateKeyPEM, csrPEM []byte, err error) {
	return []byte("-----BEGIN PRIVATE KEY-----\nfake\n-----END PRIVATE KEY-----"),
		[]byte("-----BEGIN CERTIFICATE REQUEST-----\nfake\n-----END CERTIFICATE REQUEST-----"), nil
}

// dueRow builds a due scan row with selectDueCertsSQL columns.
func dueRow(certID, incarnation string, notAfter time.Time) []any {
	return []any{certID, incarnation, "cert", "secret/redis/x/tls/cert#cert", "old-serial", strings.Repeat("a", 64), notAfter, nil, nil}
}

func testRotatorCfg() CertRotatorConfig {
	return CertRotatorConfig{
		Threshold:           30 * 24 * time.Hour,
		DefaultPKIMount:     "pki",
		DefaultPKIRole:      "service-tls",
		MaxRotationsPerTick: 20,
	}
}

// buildRotator builds CertRotator over fakes.
func buildRotator(db *fakeCertDB, signer PKISigner, vw CertVaultWriter, cfg CertRotatorConfig) *CertRotator {
	return newCertRotatorFromDB(db, CertRotatorDeps{
		Signer: signer,
		Vault:  vw,
		CSRGen: fakeCSRGen,
		Cfg:    func() CertRotatorConfig { return cfg },
		Logger: silentLogger(),
	})
}

// --- guard tests ---

// TestCertRotator_HappyRotation checks that a due cert passes the full chain:
// CAS won -> SignCSR -> WriteKV cert+key -> insert new warrant rows (cert+key)
// -> spawn Voyage.
func TestCertRotator_HappyRotation(t *testing.T) {
	certPEM := makeTestCertPEM(t)
	db := &fakeCertDB{
		dueRows:    [][]any{dueRow("cert-1", "redis-prod", time.Now().Add(24*time.Hour))},
		casResults: []int64{1}, // CAS active→rotating won
	}
	signer := &fakeSigner{cert: certPEM}
	vw := &fakeVaultWriter{}
	r := buildRotator(db, signer, vw, testRotatorCfg())

	n, err := r.Run(context.Background(), 0, 0)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if n != 1 {
		t.Fatalf("rotated = %d, want 1", n)
	}
	if db.insertedVoyages != 1 {
		t.Errorf("Voyage(rotate_tls) must be spawned exactly once, got %d", db.insertedVoyages)
	}
	if db.insertedTargets != 1 {
		t.Errorf("voyage target must be inserted, got %d", db.insertedTargets)
	}
	// cert+key are written to Vault (E3 paths).
	if len(vw.writes) != 2 {
		t.Errorf("expected 2 Vault writes (cert+key), got %d: %v", len(vw.writes), vw.writes)
	}
	// cert+key warrant rows are inserted.
	if db.insertedWarrants != 2 {
		t.Errorf("expected 2 warrant inserts (cert+key), got %d", db.insertedWarrants)
	}
}

// TestCertRotator_SingleWinner_LostCAS guards single-winner behavior
// (design.md): if CAS active->rotating returns 0 because another tick/instance
// intercepted it, rotation does NOT happen: no Voyage, no Vault writes, no
// inserts. Two ticks for one due cert must not spawn two rotations.
func TestCertRotator_SingleWinner_LostCAS(t *testing.T) {
	certPEM := makeTestCertPEM(t)
	db := &fakeCertDB{
		dueRows:    [][]any{dueRow("cert-1", "redis-prod", time.Now().Add(24*time.Hour))},
		casResults: []int64{0}, // lost the race
	}
	vw := &fakeVaultWriter{}
	r := buildRotator(db, &fakeSigner{cert: certPEM}, vw, testRotatorCfg())

	n, err := r.Run(context.Background(), 0, 0)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if n != 0 {
		t.Fatalf("rotated = %d, want 0 (lost CAS)", n)
	}
	if db.insertedVoyages != 0 {
		t.Errorf("no Voyage on lost CAS, got %d", db.insertedVoyages)
	}
	if len(vw.writes) != 0 {
		t.Errorf("no Vault writes on lost CAS, got %d", len(vw.writes))
	}
	if db.insertedWarrants != 0 {
		t.Errorf("no warrant inserts on lost CAS, got %d", db.insertedWarrants)
	}
}

// TestCertRotator_FailedPath_SignerDown guards the failed path (design.md):
// SignCSR failed -> cert is marked failed (CAS rotating->failed), does NOT stay
// active, and Voyage is not spawned. One rotation error does not fail the tick
// because Run returns nil.
func TestCertRotator_FailedPath_SignerDown(t *testing.T) {
	db := &fakeCertDB{
		dueRows:    [][]any{dueRow("cert-1", "redis-prod", time.Now().Add(24*time.Hour))},
		casResults: []int64{1, 1}, // [0]=active→rotating won, [1]=rotating→failed
	}
	vw := &fakeVaultWriter{}
	r := buildRotator(db, &fakeSigner{err: errors.New("vault pki down")}, vw, testRotatorCfg())

	n, err := r.Run(context.Background(), 0, 0)
	if err != nil {
		t.Fatalf("Run must not fail the whole tick on one cert error: %v", err)
	}
	if n != 0 {
		t.Fatalf("rotated = %d, want 0 (signer down)", n)
	}
	if db.insertedVoyages != 0 {
		t.Errorf("no Voyage on signer failure, got %d", db.insertedVoyages)
	}
	// The second CAS call is markFailed (rotating->failed): the chain captured
	// the row (casCalls>=2) but did not insert active.
	if db.casCalls < 2 {
		t.Errorf("expected CAS to rotating THEN markFailed (>=2 CAS calls), got %d", db.casCalls)
	}
	if db.insertedWarrants != 0 {
		t.Errorf("no new active warrant on failure, got %d", db.insertedWarrants)
	}
}

// TestCertRotator_FailClosed_VaultDown guards fail-closed behavior (design.md):
// Vault WriteKV failed -> markFailed, Voyage is not spawned, and Run does NOT
// fail, so the tick survives.
func TestCertRotator_FailClosed_VaultDown(t *testing.T) {
	certPEM := makeTestCertPEM(t)
	db := &fakeCertDB{
		dueRows:    [][]any{dueRow("cert-1", "redis-prod", time.Now().Add(24*time.Hour))},
		casResults: []int64{1, 1},
	}
	vw := &fakeVaultWriter{err: errors.New("vault write denied")}
	r := buildRotator(db, &fakeSigner{cert: certPEM}, vw, testRotatorCfg())

	n, err := r.Run(context.Background(), 0, 0)
	if err != nil {
		t.Fatalf("Run must survive Vault-down: %v", err)
	}
	if n != 0 {
		t.Fatalf("rotated = %d, want 0 (vault down)", n)
	}
	if db.insertedVoyages != 0 {
		t.Errorf("no Voyage on Vault failure, got %d", db.insertedVoyages)
	}
}

// TestCertRotator_Idempotent_NoDueSkipsWork: no due certs means a no-op tick
// with 0 rotations and no spawned work.
func TestCertRotator_Idempotent_NoDueSkipsWork(t *testing.T) {
	db := &fakeCertDB{dueRows: nil}
	vw := &fakeVaultWriter{}
	r := buildRotator(db, &fakeSigner{cert: makeTestCertPEM(t)}, vw, testRotatorCfg())

	n, err := r.Run(context.Background(), 0, 0)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if n != 0 || db.insertedVoyages != 0 {
		t.Errorf("no work without due certs; rotated=%d voyages=%d", n, db.insertedVoyages)
	}
}

// TestCertRotator_Jitter_SpreadsSameExpiry guards jitter (design.md): N certs
// with the SAME not_after are not all considered due in one tick because
// individual jitter (hash%window) filters out some of them. Verify that with a
// narrow threshold and wide jitter window not all N rotate in one tick.
func TestCertRotator_Jitter_SpreadsSameExpiry(t *testing.T) {
	// not_after exactly on the threshold boundary: threshold=30d, cert expires in
	// 30d. Jitter shifts effective not_after backward by hash%window; with
	// window=15d some certs are not "still not due" because the backward shift
	// brings them closer and makes them due. Build the opposite case instead:
	// not_after slightly AFTER the threshold (now+threshold+10d), so jitter
	// returns some certs to due.
	now := time.Now()
	cfg := CertRotatorConfig{
		Threshold:           30 * 24 * time.Hour,
		JitterWindow:        20 * 24 * time.Hour,
		DefaultPKIMount:     "pki",
		DefaultPKIRole:      "svc",
		MaxRotationsPerTick: 100,
	}
	// not_after = now + threshold + 10d: without jitter NOBODY is due
	// (not_after > now+threshold). Jitter shifts not_after backward by hash%20d;
	// only certs with hash%20d > 10d move under the threshold.
	notAfter := now.Add(cfg.Threshold + 10*24*time.Hour)

	dueCandidates := 0
	total := 40
	rr := &CertRotator{nowFunc: func() time.Time { return now }}
	for i := 0; i < total; i++ {
		c := dueCert{certID: "cert-" + itoaForTag(int64(i%6)) + "-" + strings.Repeat("x", i), notAfter: notAfter}
		if rr.isDue(c, now, cfg) {
			dueCandidates++
		}
	}
	// Key invariant: not all and not zero, so jitter really spreads work.
	if dueCandidates == 0 {
		t.Errorf("jitter filtered out EVERYONE: window/threshold are wrong (expected a partial set)")
	}
	if dueCandidates == total {
		t.Errorf("jitter filtered out NOBODY (%d/%d due): no spreading", dueCandidates, total)
	}
}

// TestCertRotator_ThresholdZero_NoScan: threshold=0 means the rule scans
// nothing, protecting against invalid config.
func TestCertRotator_ThresholdZero_NoScan(t *testing.T) {
	db := &fakeCertDB{dueRows: [][]any{dueRow("cert-1", "x", time.Now())}}
	cfg := testRotatorCfg()
	cfg.Threshold = 0
	r := buildRotator(db, &fakeSigner{}, &fakeVaultWriter{}, cfg)

	n, err := r.Run(context.Background(), 0, 0)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if n != 0 {
		t.Errorf("threshold=0 must scan nothing, rotated=%d", n)
	}
}

// TestCertRotator_MissingDeps: signer/vault/csrgen/cfg are required.
func TestCertRotator_MissingDeps(t *testing.T) {
	r := newCertRotatorFromDB(&fakeCertDB{}, CertRotatorDeps{}) // all nil
	if _, err := r.Run(context.Background(), 0, 0); err == nil {
		t.Fatal("Run without deps must error")
	}
}
