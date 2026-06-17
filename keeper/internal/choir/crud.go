package choir

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// PG-коды (parity incarnation/voyage).
const (
	pgErrCodeUniqueViolation     = "23505"
	pgErrCodeForeignKeyViolation = "23503"
	pgErrCodeCheckViolation      = "23514"
)

// ExecQueryRower — узкое подмножество pgxpool.Pool для read-операций и для
// работы внутри транзакции (pgx.Tx удовлетворяет тот же интерфейс). Симметрично
// incarnation.ExecQueryRower.
type ExecQueryRower interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}

// TxBeginner — узкое подмножество pgxpool.Pool для транзакционных операций
// (FOR UPDATE → проверка → mutate → commit). Симметрично incarnation.TxBeginner.
type TxBeginner interface {
	BeginTx(ctx context.Context, opts pgx.TxOptions) (pgx.Tx, error)
}

// Compile-time checks.
var (
	_ ExecQueryRower = (*pgx.Conn)(nil)
	_ ExecQueryRower = (*pgxpool.Pool)(nil)
	_ ExecQueryRower = (pgx.Tx)(nil)
	_ TxBeginner     = (*pgxpool.Pool)(nil)
)

// ---------------------------------------------------------------------------
// Choir CRUD
// ---------------------------------------------------------------------------

const insertChoirSQL = `
INSERT INTO incarnation_choirs (
    incarnation_name, choir_name, description, min_size, max_size, created_by_aid
) VALUES ($1, $2, $3, $4, $5, $6)
RETURNING created_at
`

// CreateChoir создаёт новый Choir в инкарнации. choir_name валидируется на
// формат (parity CHECK миграции 060_create_choirs.up.sql) ещё до похода в БД; min/max — sane-bounds.
// Транзакция не нужна — единичный INSERT; FK на incarnation(name) гарантирует,
// что инкарнация существует (FK-violation → [ErrIncarnationNotFound]),
// UNIQUE по PK → [ErrChoirExists].
//
// Возврат:
//   - [ErrInvalidChoirName]    — choir_name не матчит формат.
//   - [ErrInvalidSizeBounds]   — min/max ≤ 0 или min > max.
//   - [ErrIncarnationNotFound] — incarnation_name не существует (FK-violation).
//   - [ErrChoirExists]         — Choir уже есть (UNIQUE по PK).
func CreateChoir(ctx context.Context, db ExecQueryRower, c *Choir) error {
	if c == nil {
		return fmt.Errorf("choir: nil choir")
	}
	if c.IncarnationName == "" {
		return fmt.Errorf("choir: empty incarnation_name")
	}
	if !ValidChoirName(c.ChoirName) {
		return fmt.Errorf("%w: %q", ErrInvalidChoirName, c.ChoirName)
	}
	if err := validateSizeBounds(c.MinSize, c.MaxSize); err != nil {
		return err
	}

	row := db.QueryRow(ctx, insertChoirSQL,
		c.IncarnationName,
		c.ChoirName,
		nullStr(c.Description),
		nullInt(c.MinSize),
		nullInt(c.MaxSize),
		nullStr(c.CreatedByAID),
	)
	if err := row.Scan(&c.CreatedAt); err != nil {
		return mapChoirInsertError(err)
	}
	return nil
}

func mapChoirInsertError(err error) error {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		switch pgErr.Code {
		case pgErrCodeUniqueViolation:
			return ErrChoirExists
		case pgErrCodeForeignKeyViolation:
			// Единственный FK, который может «не найти» цель при INSERT-е Choir-а с
			// валидным created_by_aid — это incarnation(name). created_by_aid имеет
			// ON DELETE SET NULL, но при INSERT-е несуществующего AID тоже даст FK-
			// violation; различаем по имени constraint.
			if pgErr.ConstraintName == "incarnation_choirs_incarnation_fk" {
				return ErrIncarnationNotFound
			}
			return fmt.Errorf("choir: FK violation on %s: %w", pgErr.ConstraintName, err)
		case pgErrCodeCheckViolation:
			return fmt.Errorf("choir: CHECK violation on %s: %w", pgErr.ConstraintName, err)
		}
	}
	return fmt.Errorf("choir: insert choir: %w", err)
}

const selectChoirSQL = `
SELECT incarnation_name, choir_name, description, min_size, max_size,
       created_at, created_by_aid
FROM incarnation_choirs
WHERE incarnation_name = $1 AND choir_name = $2
`

// GetChoir читает Choir по паре PK. [ErrChoirNotFound] при отсутствии.
func GetChoir(ctx context.Context, db ExecQueryRower, incarnation, choirName string) (*Choir, error) {
	if incarnation == "" || choirName == "" {
		return nil, fmt.Errorf("choir: empty incarnation_name or choir_name")
	}
	c, err := scanChoir(db.QueryRow(ctx, selectChoirSQL, incarnation, choirName))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrChoirNotFound
		}
		return nil, fmt.Errorf("choir: get choir: %w", err)
	}
	return c, nil
}

const listChoirsSQL = `
SELECT incarnation_name, choir_name, description, min_size, max_size,
       created_at, created_by_aid
FROM incarnation_choirs
WHERE incarnation_name = $1
ORDER BY choir_name
`

// ListChoirs возвращает все Choir-ы инкарнации в порядке имени. Пустой список —
// не ошибка (инкарнация без Choir-ов либо несуществующая: разграничение —
// забота caller-а через SelectByName, S-T3 handler).
func ListChoirs(ctx context.Context, db ExecQueryRower, incarnation string) ([]*Choir, error) {
	if incarnation == "" {
		return nil, fmt.Errorf("choir: empty incarnation_name")
	}
	rows, err := db.Query(ctx, listChoirsSQL, incarnation)
	if err != nil {
		return nil, fmt.Errorf("choir: list choirs: %w", err)
	}
	defer rows.Close()

	var out []*Choir
	for rows.Next() {
		c, scanErr := scanChoir(rows)
		if scanErr != nil {
			return nil, fmt.Errorf("choir: list choirs scan: %w", scanErr)
		}
		out = append(out, c)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("choir: list choirs iter: %w", err)
	}
	return out, nil
}

const deleteChoirSQL = `
DELETE FROM incarnation_choirs
WHERE incarnation_name = $1 AND choir_name = $2
`

// DeleteChoir удаляет Choir (и каскадом его Voice-ы — ON DELETE CASCADE на
// incarnation_choir_voices). [ErrChoirNotFound] если строки не было (RowsAffected
// == 0) — защита от тихого no-op на опечатке имени.
func DeleteChoir(ctx context.Context, db ExecQueryRower, incarnation, choirName string) error {
	if incarnation == "" || choirName == "" {
		return fmt.Errorf("choir: empty incarnation_name or choir_name")
	}
	tag, err := db.Exec(ctx, deleteChoirSQL, incarnation, choirName)
	if err != nil {
		return fmt.Errorf("choir: delete choir: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrChoirNotFound
	}
	return nil
}

// ---------------------------------------------------------------------------
// Voice CRUD
// ---------------------------------------------------------------------------

const selectChoirForUpdateSQL = `
SELECT 1 FROM incarnation_choirs
WHERE incarnation_name = $1 AND choir_name = $2
FOR UPDATE
`

const insertVoiceSQL = `
INSERT INTO incarnation_choir_voices (
    incarnation_name, choir_name, sid, role, position, added_by_aid
) VALUES ($1, $2, $3, $4, $5, $6)
RETURNING added_at
`

// AddVoice добавляет Voice (членство SID в Choir-е) атомарно. Транзакция:
// SELECT … FOR UPDATE на строке Choir-а (сериализует конкурентные AddVoice /
// DeleteChoir) → валидация инварианта членства (SID уже член инкарнации:
// `souls.coven[]` содержит `incarnation_name`, ADR-044 пункт 3) → INSERT voice →
// commit.
//
// Инвариант членства проверяется явным SELECT-ом (FK на souls покрывает только
// существование SID в реестре, но НЕ членство в этой инкарнации). SID, который
// есть в souls, но не несёт incarnation в coven — отклоняется [ErrNotMembers].
//
// Возврат:
//   - [ErrChoirNotFound]  — Choir не существует (нет строки под FOR UPDATE).
//   - [ErrNotMembers]     — SID не член инкарнации (нет в souls ИЛИ coven не
//     содержит incarnation_name).
//   - [ErrVoiceExists]    — Voice для этого SID в этом Choir-е уже есть.
func AddVoice(ctx context.Context, pool TxBeginner, v *Voice) error {
	if v == nil {
		return fmt.Errorf("choir: nil voice")
	}
	if v.IncarnationName == "" || v.ChoirName == "" || v.SID == "" {
		return fmt.Errorf("choir: empty incarnation_name, choir_name or sid")
	}
	if v.Position != nil && *v.Position < 0 {
		return fmt.Errorf("choir: position must be >= 0, got %d", *v.Position)
	}

	tx, err := pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return fmt.Errorf("choir: begin add-voice tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// Lock строки Choir-а: гарантирует, что Choir существует на момент INSERT-а и
	// не будет удалён конкурентным DeleteChoir-ом между проверкой и записью.
	var dummy int
	if err := tx.QueryRow(ctx, selectChoirForUpdateSQL, v.IncarnationName, v.ChoirName).Scan(&dummy); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrChoirNotFound
		}
		return fmt.Errorf("choir: lock choir: %w", err)
	}

	if err := validateMembership(ctx, tx, v.IncarnationName, []string{v.SID}); err != nil {
		return err
	}

	if err := tx.QueryRow(ctx, insertVoiceSQL,
		v.IncarnationName,
		v.ChoirName,
		v.SID,
		nullStr(v.Role),
		nullInt(v.Position),
		nullStr(v.AddedByAID),
	).Scan(&v.AddedAt); err != nil {
		return mapVoiceInsertError(err)
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("choir: commit add-voice tx: %w", err)
	}
	return nil
}

func mapVoiceInsertError(err error) error {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		switch pgErr.Code {
		case pgErrCodeUniqueViolation:
			return ErrVoiceExists
		case pgErrCodeForeignKeyViolation:
			// sid_fk и choir_fk проверены раньше (membership + FOR UPDATE); сюда
			// FK-violation попадёт разве что от added_by_aid с несуществующим AID.
			return fmt.Errorf("choir: FK violation on %s: %w", pgErr.ConstraintName, err)
		case pgErrCodeCheckViolation:
			return fmt.Errorf("choir: CHECK violation on %s: %w", pgErr.ConstraintName, err)
		}
	}
	return fmt.Errorf("choir: insert voice: %w", err)
}

const deleteVoiceSQL = `
DELETE FROM incarnation_choir_voices
WHERE incarnation_name = $1 AND choir_name = $2 AND sid = $3
`

// RemoveVoice удаляет Voice по тройке PK. [ErrVoiceNotFound] при RowsAffected==0
// (защита от тихого no-op на опечатке). Не требует транзакции — единичный DELETE.
func RemoveVoice(ctx context.Context, db ExecQueryRower, incarnation, choirName, sid string) error {
	if incarnation == "" || choirName == "" || sid == "" {
		return fmt.Errorf("choir: empty incarnation_name, choir_name or sid")
	}
	tag, err := db.Exec(ctx, deleteVoiceSQL, incarnation, choirName, sid)
	if err != nil {
		return fmt.Errorf("choir: remove voice: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrVoiceNotFound
	}
	return nil
}

const listVoicesSQL = `
SELECT incarnation_name, choir_name, sid, role, position, added_at, added_by_aid
FROM incarnation_choir_voices
WHERE incarnation_name = $1 AND choir_name = $2
ORDER BY position NULLS LAST, sid
`

// ListVoices возвращает Voice-ы Choir-а, упорядоченные по position (NULL — в
// конец), затем по sid. Пустой список — не ошибка (Choir без Voice-ов).
func ListVoices(ctx context.Context, db ExecQueryRower, incarnation, choirName string) ([]*Voice, error) {
	if incarnation == "" || choirName == "" {
		return nil, fmt.Errorf("choir: empty incarnation_name or choir_name")
	}
	rows, err := db.Query(ctx, listVoicesSQL, incarnation, choirName)
	if err != nil {
		return nil, fmt.Errorf("choir: list voices: %w", err)
	}
	defer rows.Close()

	var out []*Voice
	for rows.Next() {
		v, scanErr := scanVoice(rows)
		if scanErr != nil {
			return nil, fmt.Errorf("choir: list voices scan: %w", scanErr)
		}
		out = append(out, v)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("choir: list voices iter: %w", err)
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// Инвариант членства + helpers
// ---------------------------------------------------------------------------

const membershipSQL = `
SELECT sid FROM souls
WHERE sid = ANY($1) AND $2 = ANY(coven)
`

// validateMembership проверяет инвариант ADR-044 пункт 3: каждый SID уже член
// инкарнации (его `souls.coven[]` содержит incarnation). Строже, чем
// incarnation.validateSoulsExist (та проверяет лишь существование SID в souls):
// тут требуется именно членство в ЭТОЙ инкарнации. Один batch-SELECT с
// предикатом `$incarnation = ANY(coven)` — не per-SID round-trip.
//
// Missing (в порядке первого вхождения, стабильно для тестов) включает SID-ы,
// которых нет в souls вовсе, И SID-ы, которые есть, но не члены инкарнации.
func validateMembership(ctx context.Context, db ExecQueryRower, incarnation string, sids []string) error {
	if len(sids) == 0 {
		return nil
	}
	// Dedup + сохранение порядка первого вхождения для Missing.
	seen := make(map[string]struct{}, len(sids))
	uniq := make([]string, 0, len(sids))
	for _, sid := range sids {
		if _, ok := seen[sid]; ok {
			continue
		}
		seen[sid] = struct{}{}
		uniq = append(uniq, sid)
	}

	rows, err := db.Query(ctx, membershipSQL, uniq, incarnation)
	if err != nil {
		return fmt.Errorf("choir: membership query: %w", err)
	}
	defer rows.Close()

	members := make(map[string]struct{}, len(uniq))
	for rows.Next() {
		var sid string
		if err := rows.Scan(&sid); err != nil {
			return fmt.Errorf("choir: membership scan: %w", err)
		}
		members[sid] = struct{}{}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("choir: membership iter: %w", err)
	}

	var missing []string
	for _, sid := range uniq {
		if _, ok := members[sid]; !ok {
			missing = append(missing, sid)
		}
	}
	if len(missing) > 0 {
		return &ErrNotMembers{Incarnation: incarnation, Missing: missing}
	}
	return nil
}

// validateSizeBounds — sane-bounds на min/max при создании Choir-а (parity
// CHECK-ов миграции 060_create_choirs.up.sql; даём типизированную ошибку до похода в БД).
func validateSizeBounds(minSize, maxSize *int) error {
	if minSize != nil && *minSize <= 0 {
		return fmt.Errorf("%w: min_size must be > 0, got %d", ErrInvalidSizeBounds, *minSize)
	}
	if maxSize != nil && *maxSize <= 0 {
		return fmt.Errorf("%w: max_size must be > 0, got %d", ErrInvalidSizeBounds, *maxSize)
	}
	if minSize != nil && maxSize != nil && *minSize > *maxSize {
		return fmt.Errorf("%w: min_size %d > max_size %d", ErrInvalidSizeBounds, *minSize, *maxSize)
	}
	return nil
}

func scanChoir(row pgx.Row) (*Choir, error) {
	var c Choir
	if err := row.Scan(
		&c.IncarnationName,
		&c.ChoirName,
		&c.Description,
		&c.MinSize,
		&c.MaxSize,
		&c.CreatedAt,
		&c.CreatedByAID,
	); err != nil {
		return nil, err
	}
	return &c, nil
}

func scanVoice(row pgx.Row) (*Voice, error) {
	var v Voice
	if err := row.Scan(
		&v.IncarnationName,
		&v.ChoirName,
		&v.SID,
		&v.Role,
		&v.Position,
		&v.AddedAt,
		&v.AddedByAID,
	); err != nil {
		return nil, err
	}
	return &v, nil
}

// nullStr / nullInt — *T → any для pgx-биндинга (nil → SQL NULL).
func nullStr(s *string) any {
	if s == nil {
		return nil
	}
	return *s
}

func nullInt(i *int) any {
	if i == nil {
		return nil
	}
	return *i
}
