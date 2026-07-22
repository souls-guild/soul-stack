package sigil

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// Unit tests for KeyService cover operator-facing logic OUTSIDE registry transaction
// invariants (those are in keys_integration_test.go): key-gen, Vault write
// (path + content), security invariant "private key doesn't leak", order
// "Vault before Introduce", and passing sentinel errors. Transaction is mocked with minimal
// fake-pool (BeginTx → QueryRow.Scan → Commit).

// --- fake VaultWriter ---

type captureVaultWriter struct {
	writes  map[string]map[string]any
	failErr error
}

func newCaptureVaultWriter() *captureVaultWriter {
	return &captureVaultWriter{writes: map[string]map[string]any{}}
}

func (w *captureVaultWriter) WriteKV(_ context.Context, path string, data map[string]any) error {
	if w.failErr != nil {
		return w.failErr
	}
	w.writes[path] = data
	return nil
}

// --- fake KeyStorePool / pgx.Tx ---
//
// Minimal fake for Introduce path in keys.go: BeginTx → (opt. clearActivePrimary
// Exec) → insert QueryRow.Scan(id, introduced_at) → Commit. For SetPrimary/Retire
// CRUD, integration tests are sufficient; here we cover Introduce and propagating
// BeginTx errors (order Vault→Introduce).

type fakeKeyPool struct {
	beginErr error
	tx       *fakeKeyTx
}

func (p *fakeKeyPool) Exec(context.Context, string, ...any) (pgconn.CommandTag, error) {
	return pgconn.CommandTag{}, nil
}
func (p *fakeKeyPool) QueryRow(context.Context, string, ...any) pgx.Row { return errRow{pgx.ErrNoRows} }
func (p *fakeKeyPool) Query(context.Context, string, ...any) (pgx.Rows, error) {
	return nil, errors.New("fakeKeyPool: Query not implemented")
}
func (p *fakeKeyPool) BeginTx(context.Context, pgx.TxOptions) (pgx.Tx, error) {
	if p.beginErr != nil {
		return nil, p.beginErr
	}
	return p.tx, nil
}

// errRow is pgx.Row returning a given error from Scan.
type errRow struct{ err error }

func (r errRow) Scan(...any) error { return r.err }

// fakeKeyTx mocks pgx.Tx for Introduce insert path: Exec is no-op (clear primary),
// QueryRow on insert returns id+introduced_at via Scan.
type fakeKeyTx struct {
	pgx.Tx
	insertID   int64
	insertTime time.Time
	committed  bool
}

func (tx *fakeKeyTx) Exec(context.Context, string, ...any) (pgconn.CommandTag, error) {
	return pgconn.CommandTag{}, nil
}
func (tx *fakeKeyTx) QueryRow(_ context.Context, sql string, _ ...any) pgx.Row {
	if strings.Contains(sql, "INSERT INTO sigil_signing_keys") {
		return insertRow{id: tx.insertID, at: tx.insertTime}
	}
	return errRow{pgx.ErrNoRows}
}
func (tx *fakeKeyTx) Commit(context.Context) error   { tx.committed = true; return nil }
func (tx *fakeKeyTx) Rollback(context.Context) error { return nil }

// insertRow returns (id, introduced_at) via Scan (RETURNING order).
type insertRow struct {
	id int64
	at time.Time
}

func (r insertRow) Scan(dest ...any) error {
	if len(dest) != 2 {
		return errors.New("insertRow: want 2 scan targets")
	}
	*(dest[0].(*int64)) = r.id
	*(dest[1].(*time.Time)) = r.at
	return nil
}

// --- tests ---

func TestKeyService_Introduce_KeyGenVaultWriteNoLeak(t *testing.T) {
	vw := newCaptureVaultWriter()
	pool := &fakeKeyPool{tx: &fakeKeyTx{insertID: 7, insertTime: time.Unix(1700000000, 0).UTC()}}
	svc, err := NewKeyService(KeyServiceDeps{Pool: pool, Vault: vw, VaultKeyMount: "secret/keeper/sigil-keys"})
	if err != nil {
		t.Fatalf("NewKeyService: %v", err)
	}

	res, err := svc.Introduce(context.Background(), true, "archon-alice")
	if err != nil {
		t.Fatalf("Introduce: %v", err)
	}

	// key_id is 64 hex (SHA-256 SPKI) and matches derived from returned pubkey.
	if len(res.KeyID) != 64 {
		t.Errorf("key_id len = %d, want 64", len(res.KeyID))
	}
	block, _ := pem.Decode([]byte(res.PubkeyPEM))
	if block == nil || block.Type != "PUBLIC KEY" {
		t.Fatalf("pubkey_pem not SPKI PEM: %q", res.PubkeyPEM)
	}
	pub, perr := x509.ParsePKIXPublicKey(block.Bytes)
	if perr != nil {
		t.Fatalf("parse pubkey: %v", perr)
	}
	der, _ := x509.MarshalPKIXPublicKey(pub.(ed25519.PublicKey))
	sum := sha256.Sum256(der)
	if want := hex.EncodeToString(sum[:]); res.KeyID != want {
		t.Errorf("key_id = %s, want SHA-256(SPKI) = %s", res.KeyID, want)
	}
	if !res.IsPrimary {
		t.Error("is_primary = false, want true (makePrimary)")
	}

	// Vault-write: exactly one secret under `<mount>/<key_id>`, field signing_key is
	// a valid PKCS#8 PEM private key.
	wantPath := "secret/keeper/sigil-keys/" + res.KeyID
	data, ok := vw.writes[wantPath]
	if !ok {
		t.Fatalf("Vault did not receive write to %q (writes=%v)", wantPath, keysOf(vw.writes))
	}
	privVal, _ := data[vaultSigningKeyField].(string)
	if privVal == "" {
		t.Fatal("vault signing_key is empty")
	}
	pb, _ := pem.Decode([]byte(privVal))
	if pb == nil || pb.Type != "PRIVATE KEY" {
		t.Fatalf("vault signing_key not PKCS#8 PEM: %q", privVal)
	}
	priv, kerr := x509.ParsePKCS8PrivateKey(pb.Bytes)
	if kerr != nil {
		t.Fatalf("parse private key: %v", kerr)
	}
	edPriv, ok := priv.(ed25519.PrivateKey)
	if !ok {
		t.Fatalf("private key type %T, want ed25519", priv)
	}
	// Private key matches public key (one pair).
	if !edPriv.Public().(ed25519.PublicKey).Equal(pub.(ed25519.PublicKey)) {
		t.Error("private key from Vault does not match pubkey from response")
	}

	// SECURITY: private key is NOT present in public result (no field of
	// IntroduceResult carries private key bytes).
	for _, field := range []string{res.KeyID, res.PubkeyPEM, res.Status} {
		if strings.Contains(field, "PRIVATE KEY") {
			t.Errorf("private key leaked into result field: %q", field)
		}
	}
}

func TestKeyService_Introduce_VaultWriteBeforeRegistry(t *testing.T) {
	// Vault-write fails → Introduce must return error BEFORE calling pool
	// (Introduce-CRUD should not be called). Check via committed=false.
	vw := newCaptureVaultWriter()
	vw.failErr = errors.New("vault down")
	tx := &fakeKeyTx{insertID: 1, insertTime: time.Now()}
	pool := &fakeKeyPool{tx: tx}
	svc, _ := NewKeyService(KeyServiceDeps{Pool: pool, Vault: vw})

	_, err := svc.Introduce(context.Background(), false, "archon-a")
	if err == nil {
		t.Fatal("Introduce must return error on Vault-write failure")
	}
	if tx.committed {
		t.Error("Introduce committed tx despite Vault-write failure (order violated)")
	}
	// SECURITY: error text does not contain private key (only key_id + wrapper).
	if strings.Contains(err.Error(), "PRIVATE KEY") {
		t.Errorf("private key leaked into error: %v", err)
	}
}

func TestKeyService_Introduce_ConcurrentPrimaryMapped(t *testing.T) {
	// Insert-QueryRow.Scan returns 23505 on one_primary constraint → Introduce
	// maps to ErrConcurrentPrimary (mapKeyInsertError), KeyService propagates it.
	vw := newCaptureVaultWriter()
	svc, _ := NewKeyService(KeyServiceDeps{Pool: &racePool{}, Vault: vw})

	_, err := svc.Introduce(context.Background(), true, "archon-a")
	if !errors.Is(err, ErrConcurrentPrimary) {
		t.Fatalf("err = %v, want ErrConcurrentPrimary", err)
	}
}

// racePool is fakeKeyPool whose insert-QueryRow returns 23505 on
// one_primary constraint (primary race).
type racePool struct{ fakeKeyPool }

func (p *racePool) BeginTx(context.Context, pgx.TxOptions) (pgx.Tx, error) {
	return &raceTx{}, nil
}

type raceTx struct{ fakeKeyTx }

func (tx *raceTx) Exec(context.Context, string, ...any) (pgconn.CommandTag, error) {
	return pgconn.CommandTag{}, nil
}
func (tx *raceTx) QueryRow(context.Context, string, ...any) pgx.Row {
	return errRow{&pgconn.PgError{Code: pgErrCodeUniqueViolation, ConstraintName: onePrimaryConstraint}}
}
func (tx *raceTx) Commit(context.Context) error   { return nil }
func (tx *raceTx) Rollback(context.Context) error { return nil }

func keysOf(m map[string]map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
