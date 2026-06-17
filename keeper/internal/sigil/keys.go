package sigil

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Файл keys.go — CRUD реестра sigil_signing_keys (миграция 037, ADR-026(h),
// R3 multi-anchor). Отдельный от store.go: store.go ведёт plugin_sigils
// (печати-допуски конкретных бинарей), keys.go — trust-anchor-ключи ПОДПИСИ
// этих печатей. Сущности независимы, общий пакет — только потому что обе про
// Sigil.
//
// Инвариант безопасности: ПРИВАТНИК НИКОГДА не в Postgres. SigningKey несёт
// только публичную часть (PubkeyPEM) и ссылку на приватник в Vault (VaultRef).
//
// Два инварианта целостности (держатся транзакционно, образец — operator.Revoke
// с FOR UPDATE):
//   - ≥1 active: нельзя [Retire] последний active-ключ ([ErrLastActiveKey]) —
//     иначе verify Sigil-ов лишается всех якорей (симметрия self-lockout RBAC);
//   - ровно один primary среди active: [Introduce] с makePrimary, [SetPrimary]
//     снимают прежний primary и ставят новый в ОДНОЙ транзакции; partial unique
//     index sigil_signing_keys_one_primary — последний рубеж против гонок.

// SigningKey — строка реестра sigil_signing_keys (миграция 037).
//
// PubkeyPEM — ПУБЛИЧНАЯ часть (SPKI PEM), едет Soul-у как trust-anchor. VaultRef
// — где лежит приватник в Vault KV. Приватника в этой структуре нет и быть не
// может (security-инвариант ADR-026(d)).
type SigningKey struct {
	ID              int64
	KeyID           string // стабильный id: SHA-256 от SPKI-DER, hex
	PubkeyPEM       string // ТОЛЬКО публичная часть (SPKI PEM)
	VaultRef        string // ссылка на приватник в Vault KV
	IsPrimary       bool
	Status          string // active | retired
	IntroducedAt    time.Time
	IntroducedByAID *string
	RetiredAt       *time.Time
	RetiredByAID    *string
}

// Sentinel-ошибки CRUD-а ключей.
var (
	// ErrKeyNotFound — ключ с таким key_id не найден (или не active там, где
	// операция требует active).
	ErrKeyNotFound = errors.New("sigil: signing key not found")

	// ErrKeyAlreadyExists — Introduce при уже существующем key_id (UNIQUE).
	ErrKeyAlreadyExists = errors.New("sigil: signing key with this key_id already exists")

	// ErrLastActiveKey — Retire последнего active-ключа запрещён: набор
	// trust-anchor-ов не должен опустеть (verify Sigil-ов остался бы без якорей).
	// Симметрия self-lockout RBAC.
	ErrLastActiveKey = errors.New("sigil: cannot retire the last active signing key")

	// ErrRetirePrimary — Retire primary-ключа запрещён напрямую: сперва нужно
	// передать primary другому active-ключу через [SetPrimary]. Так primary
	// никогда не «исчезает», и инвариант «ровно один primary среди active»
	// держится без промежуточного состояния «active-ключи есть, primary нет».
	ErrRetirePrimary = errors.New("sigil: cannot retire the primary key; SetPrimary to another active key first")

	// ErrKeyRetired — операция требует active-ключ (SetPrimary), а целевой
	// retired.
	ErrKeyRetired = errors.New("sigil: signing key is retired")

	// ErrConcurrentPrimary — гонка установки primary: partial unique index
	// sigil_signing_keys_one_primary дал 23505 (две конкурентные транзакции
	// одновременно ставили primary разным ключам — clearActivePrimary одной не
	// видел insert/update другой до commit-а). Это НЕ ErrKeyAlreadyExists
	// (key_id-конфликт): сам ключ валиден, конфликтует только инвариант «ровно
	// один primary». API маппит в 409 (retry-able: повторный Introduce/SetPrimary
	// уже увидит зафиксированный primary).
	ErrConcurrentPrimary = errors.New("sigil: concurrent primary-key change (one_primary index conflict); retry")
)

// onePrimaryConstraint — имя partial unique index «ровно один primary среди
// active» (миграция 037). По нему [mapKeyInsertError] отличает гонку primary
// ([ErrConcurrentPrimary]) от конфликта key_id ([ErrKeyAlreadyExists]) — оба
// дают SQLSTATE 23505.
const onePrimaryConstraint = "sigil_signing_keys_one_primary"

// KeyStorePool — узкое подмножество pgxpool.Pool, нужное keys-CRUD-у: read/exec
// плюс BeginTx для атомарных мульти-стейтментных операций (Introduce-makePrimary
// / Retire / SetPrimary). Объявлено локально (как operator.ServicePool); реальный
// pool и pgx.Tx удовлетворяют автоматически — тестируется через fake-pool.
type KeyStorePool interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	BeginTx(ctx context.Context, opts pgx.TxOptions) (pgx.Tx, error)
}

var _ KeyStorePool = (*pgxpool.Pool)(nil)

const (
	insertSigningKeySQL = `
INSERT INTO sigil_signing_keys (key_id, pubkey_pem, vault_ref, is_primary, introduced_by_aid)
VALUES ($1, $2, $3, $4, $5)
RETURNING id, introduced_at
`

	// clearActivePrimarySQL снимает primary-флаг со ВСЕХ active-primary строк
	// (их максимум одна по инварианту). Выполняется ПЕРЕД установкой нового
	// primary в той же транзакции — так partial unique index не срабатывает.
	clearActivePrimarySQL = `
UPDATE sigil_signing_keys
SET is_primary = false
WHERE status = 'active' AND is_primary
`

	// setPrimaryByKeyIDSQL ставит primary активному ключу. WHERE status='active'
	// — атомарная защита: retired-ключ primary стать не может (rows-affected = 0).
	setPrimaryByKeyIDSQL = `
UPDATE sigil_signing_keys
SET is_primary = true
WHERE key_id = $1 AND status = 'active'
`

	// lockActiveKeysSQL блокирует все active-строки (FOR UPDATE без агрегата —
	// PG запрещает count(*) FOR UPDATE) и отдаёт их id для подсчёта в Go.
	// Сериализует конкурентные Retire так же, как LockEffectiveClusterAdmins в
	// RBAC: захватывает блокировки на весь active-набор до проверки инварианта.
	lockActiveKeysSQL = `
SELECT id FROM sigil_signing_keys WHERE status = 'active' FOR UPDATE
`

	// selectKeyByIDForUpdateSQL читает целевую строку под блокировкой для
	// проверки инвариантов в той же транзакции, что и UPDATE.
	selectKeyByIDForUpdateSQL = `
SELECT id, key_id, pubkey_pem, vault_ref, is_primary, status,
       introduced_at, introduced_by_aid, retired_at, retired_by_aid
FROM sigil_signing_keys
WHERE key_id = $1
FOR UPDATE
`

	retireByKeyIDSQL = `
UPDATE sigil_signing_keys
SET status = 'retired', is_primary = false, retired_at = NOW(), retired_by_aid = $2
WHERE key_id = $1 AND status = 'active'
`

	listActiveKeysSQL = `
SELECT id, key_id, pubkey_pem, vault_ref, is_primary, status,
       introduced_at, introduced_by_aid, retired_at, retired_by_aid
FROM sigil_signing_keys
WHERE status = 'active'
ORDER BY is_primary DESC, introduced_at ASC, id ASC
`

	getPrimarySQL = `
SELECT id, key_id, pubkey_pem, vault_ref, is_primary, status,
       introduced_at, introduced_by_aid, retired_at, retired_by_aid
FROM sigil_signing_keys
WHERE status = 'active' AND is_primary
`

	// listAllKeyIDsSQL отдаёт key_id ВСЕХ строк без фильтра по status —
	// авторитетный набор «живых» приватников для orphan-reconcile (retired
	// тоже живой: приватник нужен для verify старых Sigil-ов).
	listAllKeyIDsSQL = `
SELECT key_id FROM sigil_signing_keys
`
)

// Introduce вводит новый trust-anchor-ключ как active. Если makePrimary —
// прежний active-primary снимается и новый становится primary в ОДНОЙ
// транзакции (инвариант «ровно один primary среди active»).
//
// Хранится только публичная часть (pubkeyPEM) + ссылка на приватник в Vault
// (vaultRef); приватник в Postgres не попадает.
//
// Ошибки:
//   - keyID/pubkeyPEM/vaultRef пусты → валидационная ошибка (до запроса);
//   - key_id уже существует → [ErrKeyAlreadyExists];
//   - introducedByAID указывает на несуществующий оператор → FK-violation
//     (wrapped). NULL допустим (bootstrap/seed без инициатора).
func Introduce(ctx context.Context, pool KeyStorePool, keyID, pubkeyPEM, vaultRef string, makePrimary bool, introducedByAID *string) (*SigningKey, error) {
	if keyID == "" {
		return nil, fmt.Errorf("sigil: key_id is empty")
	}
	if pubkeyPEM == "" {
		return nil, fmt.Errorf("sigil: pubkey_pem is empty")
	}
	if vaultRef == "" {
		return nil, fmt.Errorf("sigil: vault_ref is empty")
	}

	tx, err := pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return nil, fmt.Errorf("sigil: begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// makePrimary: снять прежний primary ДО вставки нового primary (иначе
	// partial unique index sigil_signing_keys_one_primary даст 23505).
	if makePrimary {
		if _, err := tx.Exec(ctx, clearActivePrimarySQL); err != nil {
			return nil, fmt.Errorf("sigil: clear active primary: %w", err)
		}
	}

	key := &SigningKey{
		KeyID:           keyID,
		PubkeyPEM:       pubkeyPEM,
		VaultRef:        vaultRef,
		IsPrimary:       makePrimary,
		Status:          "active",
		IntroducedByAID: introducedByAID,
	}
	err = tx.QueryRow(ctx, insertSigningKeySQL,
		keyID, pubkeyPEM, vaultRef, makePrimary, nullableAID(introducedByAID),
	).Scan(&key.ID, &key.IntroducedAt)
	if err != nil {
		return nil, mapKeyInsertError(err)
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("sigil: commit tx: %w", err)
	}
	return key, nil
}

// SetPrimary делает active-ключ primary: снимает прежний primary и ставит новый
// в одной транзакции. Целевой ключ обязан быть active.
//
// Ошибки:
//   - key_id не найден → [ErrKeyNotFound];
//   - целевой ключ retired → [ErrKeyRetired].
func SetPrimary(ctx context.Context, pool KeyStorePool, keyID, callerAID string) error {
	if keyID == "" {
		return fmt.Errorf("sigil: key_id is empty")
	}

	tx, err := pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return fmt.Errorf("sigil: begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	target, err := selectKeyForUpdate(ctx, tx, keyID)
	if err != nil {
		return err
	}
	if target.Status != "active" {
		return ErrKeyRetired
	}
	if target.IsPrimary {
		// Уже primary — no-op, но валидный (идемпотентно). Коммитим пустую tx.
		return tx.Commit(ctx)
	}

	if _, err := tx.Exec(ctx, clearActivePrimarySQL); err != nil {
		return fmt.Errorf("sigil: clear active primary: %w", err)
	}
	if _, err := tx.Exec(ctx, setPrimaryByKeyIDSQL, keyID); err != nil {
		return mapSetPrimaryError(err)
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("sigil: commit tx: %w", err)
	}
	return nil
}

// Retire выводит ключ из набора trust-anchor-ов (status=retired). Два
// инварианта, проверяемые в транзакции под FOR UPDATE:
//   - нельзя retire последний active-ключ → [ErrLastActiveKey];
//   - нельзя retire primary напрямую → [ErrRetirePrimary] (сперва SetPrimary
//     на другой active-ключ).
//
// Ошибки:
//   - callerAID пуст → ошибка (audit-инвариант: кто вывел, обязателен);
//   - key_id не найден → [ErrKeyNotFound];
//   - ключ уже retired → [ErrKeyNotFound] (active-записи по ключу нет).
func Retire(ctx context.Context, pool KeyStorePool, keyID, callerAID string) error {
	if keyID == "" {
		return fmt.Errorf("sigil: key_id is empty")
	}
	if callerAID == "" {
		return fmt.Errorf("sigil: retired_by_aid is empty")
	}

	tx, err := pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return fmt.Errorf("sigil: begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// Сериализуем конкурентные Retire: блокируем весь active-набор под FOR
	// UPDATE (образец — rbac.LockEffectiveClusterAdmins). Lock-порядок
	// детерминирован: сначала все active, затем целевая строка.
	activeCount, err := countLockedActive(ctx, tx)
	if err != nil {
		return fmt.Errorf("sigil: lock active keys: %w", err)
	}

	target, err := selectKeyForUpdate(ctx, tx, keyID)
	if err != nil {
		return err
	}
	if target.Status != "active" {
		// retired-ключ — active-записи по ключу нет.
		return ErrKeyNotFound
	}
	if target.IsPrimary {
		return ErrRetirePrimary
	}
	// activeCount включает target; запрет — если он единственный active.
	if activeCount <= 1 {
		return ErrLastActiveKey
	}

	tag, err := tx.Exec(ctx, retireByKeyIDSQL, keyID, callerAID)
	if err != nil {
		return fmt.Errorf("sigil: retire: %w", err)
	}
	if tag.RowsAffected() == 0 {
		// Гонка: между select-FOR-UPDATE и UPDATE никто не мог изменить строку
		// (она под блокировкой) — defensive, не ожидается.
		return ErrKeyNotFound
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("sigil: commit tx: %w", err)
	}
	return nil
}

// ListActiveKeys возвращает все active trust-anchor-ключи. Порядок стабилен:
// primary первым, далее по времени ввода (introduced_at, id). Это будущий набор
// для SigilTrustAnchors (R3-S6 broadcast).
//
// Имя с суффиксом Keys — чтобы не пересекаться с [ListActive] для plugin_sigils
// (store.go): в пакете живут оба реестра (печати и ключи их подписи).
func ListActiveKeys(ctx context.Context, db ExecQueryRower) ([]*SigningKey, error) {
	rows, err := db.Query(ctx, listActiveKeysSQL)
	if err != nil {
		return nil, fmt.Errorf("sigil: list active keys: %w", err)
	}
	defer rows.Close()

	var out []*SigningKey
	for rows.Next() {
		k, err := scanSigningKey(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, k)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("sigil: list active keys rows: %w", err)
	}
	return out, nil
}

// ListAllKeyIDs возвращает key_id ВСЕХ строк sigil_signing_keys независимо от
// статуса (active И retired — retired тоже живой: его приватник нужен для verify
// старых Sigil-ов). Это авторитетный набор «живых» для orphan-reconcile: всё, что
// есть в Vault под `secret/keeper/sigil-keys/<key_id>`, но НЕ в этом наборе —
// кандидат-сирота.
//
// Возврат — set (map[string]struct{}) для O(1)-lookup в reconcile-цикле.
func ListAllKeyIDs(ctx context.Context, db ExecQueryRower) (map[string]struct{}, error) {
	rows, err := db.Query(ctx, listAllKeyIDsSQL)
	if err != nil {
		return nil, fmt.Errorf("sigil: list all key ids: %w", err)
	}
	defer rows.Close()

	out := make(map[string]struct{})
	for rows.Next() {
		var keyID string
		if err := rows.Scan(&keyID); err != nil {
			return nil, fmt.Errorf("sigil: scan key id: %w", err)
		}
		out[keyID] = struct{}{}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("sigil: list all key ids rows: %w", err)
	}
	return out, nil
}

// GetPrimaryKey возвращает active primary-ключ (тот, которым Keeper подписывает
// новые Sigil-ы). [ErrKeyNotFound], если primary нет (набор пуст).
func GetPrimaryKey(ctx context.Context, db ExecQueryRower) (*SigningKey, error) {
	row := db.QueryRow(ctx, getPrimarySQL)
	k, err := scanSigningKey(row)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrKeyNotFound
		}
		return nil, err
	}
	return k, nil
}

// countLockedActive блокирует все active-строки (FOR UPDATE) и возвращает их
// число. Считаем в Go, т.к. PG запрещает count(*) с FOR UPDATE.
func countLockedActive(ctx context.Context, tx pgx.Tx) (int, error) {
	rows, err := tx.Query(ctx, lockActiveKeysSQL)
	if err != nil {
		return 0, err
	}
	defer rows.Close()
	var n int
	for rows.Next() {
		n++
	}
	if err := rows.Err(); err != nil {
		return 0, err
	}
	return n, nil
}

// selectKeyForUpdate читает строку под FOR UPDATE; pgx.ErrNoRows → ErrKeyNotFound.
func selectKeyForUpdate(ctx context.Context, tx pgx.Tx, keyID string) (*SigningKey, error) {
	row := tx.QueryRow(ctx, selectKeyByIDForUpdateSQL, keyID)
	k, err := scanSigningKey(row)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrKeyNotFound
		}
		return nil, err
	}
	return k, nil
}

// scanSigningKey — общий Scan строки sigil_signing_keys.
func scanSigningKey(row pgx.Row) (*SigningKey, error) {
	var k SigningKey
	err := row.Scan(
		&k.ID,
		&k.KeyID,
		&k.PubkeyPEM,
		&k.VaultRef,
		&k.IsPrimary,
		&k.Status,
		&k.IntroducedAt,
		&k.IntroducedByAID,
		&k.RetiredAt,
		&k.RetiredByAID,
	)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, err
		}
		return nil, fmt.Errorf("sigil: scan signing key: %w", err)
	}
	return &k, nil
}

// mapKeyInsertError маппит pgx-ошибки Introduce в sentinel-ы.
func mapKeyInsertError(err error) error {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		switch pgErr.Code {
		case pgErrCodeUniqueViolation:
			// UNIQUE(key_id) либо partial one_primary — оба SQLSTATE 23505. По
			// имени constraint различаем: one_primary — конкурентная гонка
			// установки primary ([ErrConcurrentPrimary], retry-able 409); иначе
			// конфликт key_id ([ErrKeyAlreadyExists]).
			if pgErr.ConstraintName == onePrimaryConstraint {
				return fmt.Errorf("%w (constraint %s): %w", ErrConcurrentPrimary, pgErr.ConstraintName, err)
			}
			return fmt.Errorf("%w (constraint %s): %w", ErrKeyAlreadyExists, pgErr.ConstraintName, err)
		case pgErrCodeForeignKeyViolation:
			return fmt.Errorf("sigil: FK violation on %s: %w", pgErr.ConstraintName, err)
		}
	}
	return fmt.Errorf("sigil: insert signing key: %w", err)
}

// mapSetPrimaryError маппит pgx-ошибки UPDATE-а primary в sentinel-ы. Гонка
// one_primary-index (23505) → [ErrConcurrentPrimary] (как в [mapKeyInsertError]);
// прочее — обёрнутый err.
func mapSetPrimaryError(err error) error {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == pgErrCodeUniqueViolation &&
		pgErr.ConstraintName == onePrimaryConstraint {
		return fmt.Errorf("%w (constraint %s): %w", ErrConcurrentPrimary, pgErr.ConstraintName, err)
	}
	return fmt.Errorf("sigil: set primary: %w", err)
}

// nullableAID превращает *string в any для pgx-аргумента (nil → SQL NULL).
func nullableAID(aid *string) any {
	if aid == nil {
		return nil
	}
	return *aid
}
