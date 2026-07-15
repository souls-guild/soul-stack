package handlers

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/souls-guild/soul-stack/keeper/internal/api/problem"
	"github.com/souls-guild/soul-stack/keeper/internal/sigil"
)

// validKeyID — 64 hex (key_id format). Used in test path params.
const validKeyID = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

// --- fake VaultWriter / pool for sigil.KeyService ---

type fkVault struct{ err error }

func (v fkVault) WriteKV(context.Context, string, map[string]any) error { return v.err }

// fkPool is a configurable sigil.KeyStorePool. txFactory builds a tx for the mutation;
// nil tx → no BeginTx error needed (we return fkTx yielding the sentinel).
type fkPool struct {
	tx *fkTx
}

func (p *fkPool) Exec(context.Context, string, ...any) (pgconn.CommandTag, error) {
	return pgconn.CommandTag{}, nil
}
func (p *fkPool) QueryRow(context.Context, string, ...any) pgx.Row { return scanErrRow{pgx.ErrNoRows} }
func (p *fkPool) Query(context.Context, string, ...any) (pgx.Rows, error) {
	return nil, errors.New("fkPool.Query unused")
}
func (p *fkPool) BeginTx(context.Context, pgx.TxOptions) (pgx.Tx, error) { return p.tx, nil }

type scanErrRow struct{ err error }

func (r scanErrRow) Scan(...any) error { return r.err }

// fkTx is a pgx.Tx whose QueryRow is driven by per-SQL hooks:
//   - insert → insertScan (id, introduced_at) or insertErr (sentinel);
//   - select-for-update → selectErr (e.g. pgx.ErrNoRows → ErrKeyNotFound).
type fkTx struct {
	pgx.Tx
	insertErr  error // non-nil → insert-QueryRow.Scan returns this
	selectErr  error // non-nil → select-for-update QueryRow.Scan returns this
	insertID   int64
	insertTime time.Time
}

func (tx *fkTx) Exec(context.Context, string, ...any) (pgconn.CommandTag, error) {
	return pgconn.CommandTag{}, nil
}
func (tx *fkTx) QueryRow(_ context.Context, sql string, _ ...any) pgx.Row {
	switch {
	case strings.Contains(sql, "INSERT INTO sigil_signing_keys"):
		if tx.insertErr != nil {
			return scanErrRow{tx.insertErr}
		}
		return kvInsertRow{id: tx.insertID, at: tx.insertTime}
	case strings.Contains(sql, "FOR UPDATE"):
		if tx.selectErr != nil {
			return scanErrRow{tx.selectErr}
		}
		return scanErrRow{pgx.ErrNoRows}
	}
	return scanErrRow{pgx.ErrNoRows}
}
func (tx *fkTx) Commit(context.Context) error   { return nil }
func (tx *fkTx) Rollback(context.Context) error { return nil }

type kvInsertRow struct {
	id int64
	at time.Time
}

func (r kvInsertRow) Scan(dest ...any) error {
	*(dest[0].(*int64)) = r.id
	*(dest[1].(*time.Time)) = r.at
	return nil
}

func newKeyHandler(t *testing.T, pool sigil.KeyStorePool, vault sigil.VaultWriter) *SigilKeyHandler {
	t.Helper()
	svc, err := sigil.NewKeyService(sigil.KeyServiceDeps{Pool: pool, Vault: vault})
	if err != nil {
		t.Fatalf("sigil.NewKeyService: %v", err)
	}
	return NewSigilKeyHandler(svc, nil)
}

// --- IntroduceTyped ---

func TestSigilKeyHandler_Introduce_201_NoPrivateKey(t *testing.T) {
	pool := &fkPool{tx: &fkTx{insertID: 1, insertTime: time.Unix(1700000000, 0).UTC()}}
	h := newKeyHandler(t, pool, fkVault{})
	reply, err := h.IntroduceTyped(context.Background(), claimsFor("archon-alice"), true)
	if err != nil {
		t.Fatalf("IntroduceTyped: %v", err)
	}
	if len(reply.View.KeyID) != 64 {
		t.Errorf("key_id len = %d, want 64", len(reply.View.KeyID))
	}
	if !strings.Contains(reply.View.PubkeyPEM, "BEGIN PUBLIC KEY") {
		t.Errorf("pubkey_pem не SPKI: %q", reply.View.PubkeyPEM)
	}
	// SECURITY: the projection carries no private key (KeyService doesn't return it;
	// the View form has no private field by construction).
	if strings.Contains(reply.View.PubkeyPEM, "PRIVATE KEY") {
		t.Errorf("private key leaked into reply: %q", reply.View.PubkeyPEM)
	}
}

func TestSigilKeyHandler_Introduce_ConcurrentPrimary_409(t *testing.T) {
	pool := &fkPool{tx: &fkTx{insertErr: &pgconn.PgError{Code: "23505", ConstraintName: "sigil_signing_keys_one_primary"}}}
	h := newKeyHandler(t, pool, fkVault{})
	_, err := h.IntroduceTyped(context.Background(), claimsFor("archon-alice"), true)
	wantProblem(t, err, problem.TypeSigilKeyConcurrentChange)
}

// --- SetPrimaryTyped ---

func TestSigilKeyHandler_SetPrimary_NotFound_404(t *testing.T) {
	// select-for-update → pgx.ErrNoRows → ErrKeyNotFound → 404.
	pool := &fkPool{tx: &fkTx{selectErr: pgx.ErrNoRows}}
	h := newKeyHandler(t, pool, fkVault{})
	_, err := h.SetPrimaryTyped(context.Background(), claimsFor("archon-a"), validKeyID)
	wantProblem(t, err, problem.TypeSigilKeyNotFound)
}

func TestSigilKeyHandler_SetPrimary_BadKeyID_422(t *testing.T) {
	h := newKeyHandler(t, &fkPool{tx: &fkTx{}}, fkVault{})
	_, err := h.SetPrimaryTyped(context.Background(), claimsFor("archon-a"), "NOTHEX")
	wantProblem(t, err, problem.TypeValidationFailed)
}

// --- RetireTyped ---

func TestSigilKeyHandler_Retire_NotFound_404(t *testing.T) {
	// Retire path keys.go: countLockedActive (tx.Query → 2 rows) → select-for-update
	// (tx.QueryRow → ErrNoRows → ErrKeyNotFound → 404). retirePool/retireTx model these
	// two DB calls.
	h := newKeyHandler(t, &retirePool{selectErr: pgx.ErrNoRows, activeCount: 2}, fkVault{})
	_, err := h.RetireTyped(context.Background(), claimsFor("archon-a"), validKeyID)
	wantProblem(t, err, problem.TypeSigilKeyNotFound)
}

func TestSigilKeyHandler_Retire_BadKeyID_422(t *testing.T) {
	h := newKeyHandler(t, &fkPool{tx: &fkTx{}}, fkVault{})
	_, err := h.RetireTyped(context.Background(), claimsFor("archon-a"), "zzz")
	wantProblem(t, err, problem.TypeValidationFailed)
}

// retirePool/retireTx — fake for the Retire path keys.go (countLockedActive Query →
// select-for-update QueryRow). Returns activeCount rows from Query and selectErr from
// the select-QueryRow.
type retirePool struct {
	selectErr   error
	activeCount int
}

func (p *retirePool) Exec(context.Context, string, ...any) (pgconn.CommandTag, error) {
	return pgconn.CommandTag{}, nil
}
func (p *retirePool) QueryRow(context.Context, string, ...any) pgx.Row {
	return scanErrRow{pgx.ErrNoRows}
}
func (p *retirePool) Query(context.Context, string, ...any) (pgx.Rows, error) {
	return nil, errors.New("retirePool.Query unused (tx.Query used)")
}
func (p *retirePool) BeginTx(context.Context, pgx.TxOptions) (pgx.Tx, error) {
	return &retireTx{selectErr: p.selectErr, activeCount: p.activeCount}, nil
}

type retireTx struct {
	pgx.Tx
	selectErr   error
	activeCount int
}

func (tx *retireTx) Exec(context.Context, string, ...any) (pgconn.CommandTag, error) {
	return pgconn.CommandTag{}, nil
}
func (tx *retireTx) Query(context.Context, string, ...any) (pgx.Rows, error) {
	return &countRows{n: tx.activeCount}, nil
}
func (tx *retireTx) QueryRow(context.Context, string, ...any) pgx.Row {
	return scanErrRow{tx.selectErr}
}
func (tx *retireTx) Commit(context.Context) error   { return nil }
func (tx *retireTx) Rollback(context.Context) error { return nil }

// countRows is a pgx.Rows returning n rows (for countLockedActive).
type countRows struct {
	pgx.Rows
	n   int
	pos int
}

func (r *countRows) Next() bool { r.pos++; return r.pos <= r.n }
func (r *countRows) Close()     {}
func (r *countRows) Err() error { return nil }
