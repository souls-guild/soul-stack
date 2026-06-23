package operator

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// ErrOperatorAlreadyExists — UNIQUE-violation (`23505`) при Insert: AID
// уже занят либо повторно вставляется bootstrap (partial unique index
// `operators_first_archon_idx` на `created_by_aid IS NULL`).
var ErrOperatorAlreadyExists = errors.New("operator: AID already exists")

// ErrOperatorNotFound — SELECT не нашёл строку по AID. Возвращается
// SelectByAID — отдельный sentinel, чтобы вызывающий мог отличить
// «не существует» от транспортной ошибки.
var ErrOperatorNotFound = errors.New("operator: AID not found")

// ErrOperatorAlreadyRevoked — Revoke вызван для уже ревокнутого AID
// (revoked_at != NULL). Sentinel выделен, чтобы handler возвращал
// 409, а не 404.
var ErrOperatorAlreadyRevoked = errors.New("operator: AID already revoked")

// pgErrCodeUniqueViolation — SQLSTATE для UNIQUE-нарушения, в т.ч. PK и
// partial unique index. Документировано в pgerrcode, но в keeper/go.sum
// есть только indirect-зависимость; держим constant локально, чтобы не
// тянуть пакет в API.
const pgErrCodeUniqueViolation = "23505"

// pgErrCodeForeignKeyViolation — SQLSTATE для FK-нарушения. Для
// operators возникает на `created_by_aid` (insert ссылается на
// несуществующий AID).
const pgErrCodeForeignKeyViolation = "23503"

// ExecQueryRower — узкое подмножество интерфейса pgxpool.Pool, нужное
// CRUD-у. Сужение позволяет unit-тестировать функции через fake-pool без
// поднятия Postgres-а; реальный pool из keeper/internal/pg удовлетворяет
// интерфейсу автоматически.
//
// Query — часть подмножества для функций, читающих несколько строк;
// pgxpool.Pool/Conn/Tx удовлетворяют автоматически.
//
// Тип экспортирован, чтобы handlers/operator.go мог типизировать pool
// в OperatorHandler без зависимости от pgxpool в API-слое.
type ExecQueryRower interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}

// execQueryRower — backwards-совместимый alias (package-internal usage в
// crud.go и crud_test.go).
type execQueryRower = ExecQueryRower

// Compile-time check: pgxpool.Pool / pgx.Conn / pgx.Tx удовлетворяют
// execQueryRower. Все три фактически используются (Pool в `keeper run`,
// Tx в bootstrap.Init, Conn — теоретически для пользовательских snippet-ов).
// pgx.Tx — интерфейс, потому `(pgx.Tx)(nil)`-форма без указателя.
var (
	_ execQueryRower = (*pgx.Conn)(nil)
	_ execQueryRower = (*pgxpool.Pool)(nil)
	_ execQueryRower = (pgx.Tx)(nil)
)

// insertOperatorSQL — INSERT с явным маппингом всех колонок таблицы
// `operators` (003_create_operators.up.sql). `created_at` берётся из
// DEFAULT NOW(), если caller не задал значение — поведение симметрично
// audit_log в `keeper/internal/auditpg`.
const insertOperatorSQL = `
INSERT INTO operators (aid, display_name, auth_method, created_at, created_by_aid, created_via, revoked_at, metadata)
VALUES ($1, $2, $3, COALESCE($4, NOW()), $5, $6, $7, $8)
`

// selectOperatorByAIDSQL — SELECT всех колонок operators по PK.
const selectOperatorByAIDSQL = `
SELECT aid, display_name, auth_method, created_at, created_by_aid, created_via, revoked_at, metadata
FROM operators
WHERE aid = $1
`

// countOperatorsSQL — COUNT(*) без фильтра по revoked_at. Используется
// bootstrap-инвариантом из ADR-013: «реестр пуст» означает 0 строк,
// независимо от revoked-флага.
const countOperatorsSQL = `SELECT COUNT(*) FROM operators`

// ListFilter — параметры `GET /v1/operators`. Пустые поля = «без фильтра».
// IncludeRevoked=false → SQL добавляет `revoked_at IS NULL` (отдаём только
// активных; default-поведение UI). IncludeRevoked=true → без фильтра по
// revoked_at (admin-view).
type ListFilter struct {
	AuthMethod     AuthMethod
	IncludeRevoked bool
}

// Insert вставляет нового Архонта в реестр.
//
// Pre-conditions:
//   - op.AID соответствует [AIDPattern] (валидируется до round-trip).
//   - op.AuthMethod — один из enum-ов (jwt / mtls / combined / ldap / oidc;
//     ldap/oidc — федеративная аутентификация, ADR-058).
//   - op.DisplayName — непустой (NOT NULL без DEFAULT в схеме).
//
// Возврат:
//   - [ErrOperatorAlreadyExists] на UNIQUE-violation (PK или
//     partial unique index `operators_first_archon_idx`).
//   - wrapped fmt.Errorf на FK-violation (`created_by_aid` ссылается на
//     несуществующий AID) с упоминанием SQLSTATE — caller может
//     различить случай через сообщение.
//   - прочие ошибки pgx — без обёртки, пробрасываются как есть.
func Insert(ctx context.Context, db execQueryRower, op *Operator) error {
	if op == nil {
		return fmt.Errorf("operator: nil operator")
	}
	if !ValidAID(op.AID) {
		return fmt.Errorf("operator: invalid AID %q (must match %s)", op.AID, AIDPattern)
	}
	if op.DisplayName == "" {
		return fmt.Errorf("operator: display_name is empty")
	}
	switch op.AuthMethod {
	case AuthMethodJWT, AuthMethodMTLS, AuthMethodCombined, AuthMethodLDAP, AuthMethodOIDC:
	default:
		return fmt.Errorf("operator: invalid auth_method %q", op.AuthMethod)
	}

	// created_via по умолчанию 'user' (ADR-058(d)): Operator API
	// (Service.Create) и legacy-вызовы не задают поле — оператор, заведённый
	// через POST /v1/operators, по определению user. Bootstrap/system/ldap/oidc
	// проставляют значение явно. Дефолт ставится здесь (а не COALESCE в SQL),
	// чтобы прикладная валидация ниже всегда видела канонизированное значение.
	createdVia := op.CreatedVia
	if createdVia == "" {
		createdVia = CreatedViaUser
	}
	switch createdVia {
	case CreatedViaBootstrap, CreatedViaUser, CreatedViaLDAP, CreatedViaOIDC, CreatedViaSystem:
	default:
		return fmt.Errorf("operator: invalid created_via %q", createdVia)
	}

	metadataBytes, err := marshalMetadata(op.Metadata)
	if err != nil {
		return fmt.Errorf("operator: marshal metadata: %w", err)
	}

	var createdAt any
	if !op.CreatedAt.IsZero() {
		createdAt = op.CreatedAt.UTC()
	}

	var createdByAID any
	if op.CreatedByAID != nil {
		createdByAID = *op.CreatedByAID
	}

	var revokedAt any
	if op.RevokedAt != nil {
		revokedAt = op.RevokedAt.UTC()
	}

	_, err = db.Exec(ctx, insertOperatorSQL,
		op.AID,
		op.DisplayName,
		string(op.AuthMethod),
		createdAt,
		createdByAID,
		createdVia,
		revokedAt,
		metadataBytes,
	)
	if err != nil {
		return mapInsertError(err)
	}
	return nil
}

// mapInsertError маппит pgx-ошибки в sentinel-ы пакета. UNIQUE — общий
// ErrOperatorAlreadyExists (вызывающий по контексту знает, был ли это
// PK-конфликт по AID или partial unique по bootstrap-инварианту).
func mapInsertError(err error) error {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		switch pgErr.Code {
		case pgErrCodeUniqueViolation:
			// Multi-wrap (Go 1.20+): и sentinel, и оригинал доступны
			// через errors.Is. Имя constraint-а — в сообщении для логов.
			return fmt.Errorf("%w (constraint %s): %w",
				ErrOperatorAlreadyExists, pgErr.ConstraintName, err)
		case pgErrCodeForeignKeyViolation:
			return fmt.Errorf("operator: FK violation on %s: %w", pgErr.ConstraintName, err)
		}
	}
	return fmt.Errorf("operator: insert: %w", err)
}

// SelectByAID читает Operator по PK. Возвращает [ErrOperatorNotFound]
// при pgx.ErrNoRows.
func SelectByAID(ctx context.Context, db execQueryRower, aid string) (*Operator, error) {
	row := db.QueryRow(ctx, selectOperatorByAIDSQL, aid)
	return scanOperator(row)
}

// scanOperator — общий Scan для одной строки operators. Вынесен, чтобы
// SelectByAID и будущие List-функции читали колонки одинаково.
func scanOperator(row pgx.Row) (*Operator, error) {
	var (
		op            Operator
		authMethodStr string
		createdByAID  *string
		metadataBytes []byte
	)
	err := row.Scan(
		&op.AID,
		&op.DisplayName,
		&authMethodStr,
		&op.CreatedAt,
		&createdByAID,
		&op.CreatedVia,
		&op.RevokedAt,
		&metadataBytes,
	)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrOperatorNotFound
		}
		return nil, fmt.Errorf("operator: scan: %w", err)
	}
	op.AuthMethod = AuthMethod(authMethodStr)
	op.CreatedByAID = createdByAID
	if len(metadataBytes) > 0 {
		if err := json.Unmarshal(metadataBytes, &op.Metadata); err != nil {
			return nil, fmt.Errorf("operator: unmarshal metadata: %w", err)
		}
	}
	return &op, nil
}

// Count возвращает суммарное число записей в operators (включая
// revoked). Используется bootstrap-инвариантом «реестр пуст» из ADR-013
// под PG advisory lock — см. `keeper/internal/bootstrap`.
func Count(ctx context.Context, db execQueryRower) (int64, error) {
	var n int64
	if err := db.QueryRow(ctx, countOperatorsSQL).Scan(&n); err != nil {
		return 0, fmt.Errorf("operator: count: %w", err)
	}
	return n, nil
}

// List возвращает страницу строк operators под фильтром, отсортированную по
// created_at DESC (свежие сверху, паритет push_runs/incarnations/errands).
// total — COUNT(*) под тем же фильтром без LIMIT/OFFSET (для UI-пагинации).
//
// Запросы простые без подготовленных выражений — фильтр маленький, prepared
// statement переиспользуется драйвером.
func List(ctx context.Context, db execQueryRower, f ListFilter, offset, limit int) ([]*Operator, int, error) {
	whereSQL, args := buildOperatorWhere(f)

	var total int
	if err := db.QueryRow(ctx, "SELECT COUNT(*) FROM operators"+whereSQL, args...).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("operator: list count: %w", err)
	}

	selectSQL := `SELECT aid, display_name, auth_method, created_at, created_by_aid, created_via, revoked_at, metadata
FROM operators` + whereSQL + `
ORDER BY created_at DESC
LIMIT $` + intToString(len(args)+1) + ` OFFSET $` + intToString(len(args)+2)
	args = append(args, limit, offset)

	rows, err := db.Query(ctx, selectSQL, args...)
	if err != nil {
		return nil, 0, fmt.Errorf("operator: list select: %w", err)
	}
	defer rows.Close()

	out := make([]*Operator, 0, limit)
	for rows.Next() {
		op, err := scanOperator(rows)
		if err != nil {
			return nil, 0, fmt.Errorf("operator: list scan: %w", err)
		}
		out = append(out, op)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, fmt.Errorf("operator: list iter: %w", err)
	}
	return out, total, nil
}

// buildOperatorWhere собирает WHERE-предикат под ListFilter. Возвращает
// строку с ведущим " WHERE …" либо "" если фильтр пуст.
func buildOperatorWhere(f ListFilter) (string, []any) {
	var (
		conds []string
		args  []any
	)
	if f.AuthMethod != "" {
		args = append(args, string(f.AuthMethod))
		conds = append(conds, "auth_method = $"+intToString(len(args)))
	}
	if !f.IncludeRevoked {
		conds = append(conds, "revoked_at IS NULL")
	}
	if len(conds) == 0 {
		return "", args
	}
	out := " WHERE "
	for i, c := range conds {
		if i > 0 {
			out += " AND "
		}
		out += c
	}
	return out, args
}

// intToString — мелкий helper для построения $N плейсхолдеров. Симметрично
// errand.Store::itoa (там inline strconv-free для одного места); operator-
// crud SCAN-функцию использует трижды, держим inline-impl ради нулевой
// зависимости от strconv в hot-path-е list-а.
func intToString(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [10]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}

// marshalMetadata сериализует metadata в JSON-bytes для прямой подстановки
// в JSONB-колонку (pgx умеет). nil → `{}`, симметрично audit-payload.
func marshalMetadata(metadata map[string]any) ([]byte, error) {
	if metadata == nil {
		return []byte("{}"), nil
	}
	return json.Marshal(metadata)
}

// revokeOperatorSQL — UPDATE active-operator-а: ставим revoked_at=NOW() +
// дописываем `revoke_reason` в metadata. WHERE revoked_at IS NULL —
// атомарная защита от повторного revoke (rows-affected = 0 → уже revoked
// или не существует, дифференцируем через SELECT в Revoke).
const revokeOperatorSQL = `
UPDATE operators
SET revoked_at = NOW(),
    metadata = jsonb_set(COALESCE(metadata, '{}'::jsonb), '{revoke_reason}', to_jsonb($2::text))
WHERE aid = $1 AND revoked_at IS NULL
`

// revokeOperatorNoReasonSQL — то же без записи в metadata. Используется,
// когда caller не передал reason — иначе jsonb_set добавил бы
// `"revoke_reason":""`, что засоряет metadata пустыми ключами.
const revokeOperatorNoReasonSQL = `
UPDATE operators
SET revoked_at = NOW()
WHERE aid = $1 AND revoked_at IS NULL
`

// Revoke ставит revoked_at для активного оператора и сохраняет reason в
// metadata.revoke_reason (только при непустом reason — см.
// revokeOperatorNoReasonSQL).
//
// Семантика:
//   - aid не существует в реестре → [ErrOperatorNotFound].
//   - aid уже ревокнут (revoked_at != NULL) → [ErrOperatorAlreadyRevoked].
//   - reason пуст — допустимо (поле опциональное), metadata не меняется.
//
// Активные JWT ревокнутого Архонта продолжают работать до `exp`
// (ADR-014(d), PM-decision M0.6b #3 — JWT verify revoked_at не читает).
func Revoke(ctx context.Context, db execQueryRower, aid string, reason string) error {
	if !ValidAID(aid) {
		return fmt.Errorf("operator: invalid AID %q (must match %s)", aid, AIDPattern)
	}
	var (
		tag pgconn.CommandTag
		err error
	)
	if reason == "" {
		tag, err = db.Exec(ctx, revokeOperatorNoReasonSQL, aid)
	} else {
		tag, err = db.Exec(ctx, revokeOperatorSQL, aid, reason)
	}
	if err != nil {
		return fmt.Errorf("operator: revoke: %w", err)
	}
	if tag.RowsAffected() == 1 {
		return nil
	}
	// 0 rows — либо AID нет вовсе, либо уже revoked. Дифференцируем через
	// SelectByAID: для UX-handler-а важно различить 404 vs 409.
	op, selErr := SelectByAID(ctx, db, aid)
	if errors.Is(selErr, ErrOperatorNotFound) {
		return ErrOperatorNotFound
	}
	if selErr != nil {
		return fmt.Errorf("operator: revoke probe: %w", selErr)
	}
	if op.RevokedAt != nil {
		return ErrOperatorAlreadyRevoked
	}
	// Не должно случиться: WHERE-clause или rows-affected дали 0, но
	// строка активна. Возвращаем generic-error как симптом потенциального
	// race-condition (например, параллельный revoke).
	return fmt.Errorf("operator: revoke: 0 rows affected, but %q is active (concurrent revoke?)", aid)
}
