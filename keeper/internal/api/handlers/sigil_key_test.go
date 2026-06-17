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

// validKeyID — 64 hex (формат key_id). Используется в path-param-ах тестов.
const validKeyID = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

// --- fake VaultWriter / pool под sigil.KeyService ---

type fkVault struct{ err error }

func (v fkVault) WriteKV(context.Context, string, map[string]any) error { return v.err }

// fkPool — настраиваемый sigil.KeyStorePool. txFactory строит tx под мутацию;
// nil-tx → BeginTx-ошибка не нужна (вернём fkTx, дающий sentinel).
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

// fkTx — pgx.Tx, чьи QueryRow управляются per-SQL hook-ами:
//   - insert → insertScan (id, introduced_at) либо insertErr (sentinel);
//   - select-for-update → selectErr (напр. pgx.ErrNoRows → ErrKeyNotFound).
type fkTx struct {
	pgx.Tx
	insertErr  error // не-nil → insert-QueryRow.Scan вернёт это
	selectErr  error // не-nil → select-for-update QueryRow.Scan вернёт это
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
	// SECURITY: проекция не несёт приватник (KeyService его не возвращает; форма
	// View не имеет private-поля by construction).
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
	// Retire-путь keys.go: countLockedActive (tx.Query → 2 строки) → select-for-
	// update (tx.QueryRow → ErrNoRows → ErrKeyNotFound → 404). retirePool/retireTx
	// моделируют эти два DB-обращения.
	h := newKeyHandler(t, &retirePool{selectErr: pgx.ErrNoRows, activeCount: 2}, fkVault{})
	_, err := h.RetireTyped(context.Background(), claimsFor("archon-a"), validKeyID)
	wantProblem(t, err, problem.TypeSigilKeyNotFound)
}

func TestSigilKeyHandler_Retire_BadKeyID_422(t *testing.T) {
	h := newKeyHandler(t, &fkPool{tx: &fkTx{}}, fkVault{})
	_, err := h.RetireTyped(context.Background(), claimsFor("archon-a"), "zzz")
	wantProblem(t, err, problem.TypeValidationFailed)
}

// retirePool/retireTx — fake под Retire-путь keys.go (countLockedActive Query →
// select-for-update QueryRow). Возвращает activeCount строк из Query и selectErr
// из select-QueryRow.
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

// countRows — pgx.Rows, отдающий n строк (для countLockedActive).
type countRows struct {
	pgx.Rows
	n   int
	pos int
}

func (r *countRows) Next() bool { r.pos++; return r.pos <= r.n }
func (r *countRows) Close()     {}
func (r *countRows) Err() error { return nil }
