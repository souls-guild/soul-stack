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

// Unit-тесты KeyService покрывают operator-facing-логику ВНЕ транзакционных
// инвариантов реестра (те — keys_integration_test.go): key-gen, Vault-write
// (путь + содержимое), security-инвариант «приватник не утекает», порядок
// «Vault раньше Introduce» и проброс sentinel-ов. Транзакция мокается минимальным
// fake-pool-ом (BeginTx → QueryRow.Scan → Commit).

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
// Минимальный fake под Introduce-путь keys.go: BeginTx → (опц. clearActivePrimary
// Exec) → insert QueryRow.Scan(id, introduced_at) → Commit. Для SetPrimary/Retire
// CRUD достаточно integration-тестов; здесь покрываем Introduce и проброс ошибки
// BeginTx (порядок Vault→Introduce).

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

// errRow — pgx.Row, возвращающий заданную ошибку из Scan.
type errRow struct{ err error }

func (r errRow) Scan(...any) error { return r.err }

// fakeKeyTx — мок pgx.Tx под insert-путь Introduce: Exec — no-op (clear primary),
// QueryRow на insert отдаёт id+introduced_at в Scan.
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

// insertRow отдаёт (id, introduced_at) в Scan (порядок RETURNING).
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

	// key_id — 64 hex (SHA-256 SPKI) и совпадает с derive из вернувшегося pubkey.
	if len(res.KeyID) != 64 {
		t.Errorf("key_id len = %d, want 64", len(res.KeyID))
	}
	block, _ := pem.Decode([]byte(res.PubkeyPEM))
	if block == nil || block.Type != "PUBLIC KEY" {
		t.Fatalf("pubkey_pem не SPKI PEM: %q", res.PubkeyPEM)
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

	// Vault-write: ровно один секрет под `<mount>/<key_id>`, поле signing_key —
	// валидный PKCS#8 PEM-приватник.
	wantPath := "secret/keeper/sigil-keys/" + res.KeyID
	data, ok := vw.writes[wantPath]
	if !ok {
		t.Fatalf("Vault не получил запись по %q (writes=%v)", wantPath, keysOf(vw.writes))
	}
	privVal, _ := data[vaultSigningKeyField].(string)
	if privVal == "" {
		t.Fatal("vault signing_key пуст")
	}
	pb, _ := pem.Decode([]byte(privVal))
	if pb == nil || pb.Type != "PRIVATE KEY" {
		t.Fatalf("vault signing_key не PKCS#8 PEM: %q", privVal)
	}
	priv, kerr := x509.ParsePKCS8PrivateKey(pb.Bytes)
	if kerr != nil {
		t.Fatalf("parse private key: %v", kerr)
	}
	edPriv, ok := priv.(ed25519.PrivateKey)
	if !ok {
		t.Fatalf("private key type %T, want ed25519", priv)
	}
	// Приватник соответствует публичному (одна пара).
	if !edPriv.Public().(ed25519.PublicKey).Equal(pub.(ed25519.PublicKey)) {
		t.Error("приватник из Vault не соответствует pubkey из ответа")
	}

	// SECURITY: приватник НЕ присутствует в публичном результате (никакое поле
	// IntroduceResult не несёт байт приватника).
	for _, field := range []string{res.KeyID, res.PubkeyPEM, res.Status} {
		if strings.Contains(field, "PRIVATE KEY") {
			t.Errorf("private key leaked into result field: %q", field)
		}
	}
}

func TestKeyService_Introduce_VaultWriteBeforeRegistry(t *testing.T) {
	// Vault-write падает → Introduce обязан вернуть ошибку ДО обращения к pool
	// (Introduce-CRUD не должен вызваться). Проверяем по committed=false.
	vw := newCaptureVaultWriter()
	vw.failErr = errors.New("vault down")
	tx := &fakeKeyTx{insertID: 1, insertTime: time.Now()}
	pool := &fakeKeyPool{tx: tx}
	svc, _ := NewKeyService(KeyServiceDeps{Pool: pool, Vault: vw})

	_, err := svc.Introduce(context.Background(), false, "archon-a")
	if err == nil {
		t.Fatal("Introduce должен вернуть ошибку при сбое Vault-write")
	}
	if tx.committed {
		t.Error("Introduce закоммитил tx несмотря на сбой Vault-write (порядок нарушен)")
	}
	// SECURITY: текст ошибки не содержит приватник (только key_id + обёртка).
	if strings.Contains(err.Error(), "PRIVATE KEY") {
		t.Errorf("private key leaked into error: %v", err)
	}
}

func TestKeyService_Introduce_ConcurrentPrimaryMapped(t *testing.T) {
	// Insert-QueryRow.Scan возвращает 23505 на one_primary-constraint → Introduce
	// маппит в ErrConcurrentPrimary (mapKeyInsertError), KeyService прокидывает его.
	vw := newCaptureVaultWriter()
	svc, _ := NewKeyService(KeyServiceDeps{Pool: &racePool{}, Vault: vw})

	_, err := svc.Introduce(context.Background(), true, "archon-a")
	if !errors.Is(err, ErrConcurrentPrimary) {
		t.Fatalf("err = %v, want ErrConcurrentPrimary", err)
	}
}

// racePool — fakeKeyPool, чей insert-QueryRow возвращает 23505 на
// one_primary-constraint (гонка primary).
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
