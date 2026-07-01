package herald

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Sentinel-ошибки CRUD-слоя. Handler-сторона (OpenAPI/MCP, слайс S4) маппит:
//   - ErrHeraldExists    → 409.
//   - ErrHeraldNotFound  → 404.
//   - ErrTidingExists    → 409.
//   - ErrTidingNotFound  → 404.
//
// ErrHeraldInUse НЕ вводится: tidings.herald — ON DELETE CASCADE (ADR-052(a),
// naming-rules.md), удаление Herald-а уносит его Tiding-подписки, а не
// блокируется (отличие от RESTRICT-409).
var (
	ErrHeraldExists   = errors.New("herald: name already exists")
	ErrHeraldNotFound = errors.New("herald: name not found")
	ErrTidingExists   = errors.New("herald: tiding name already exists")
	ErrTidingNotFound = errors.New("herald: tiding name not found")

	// ErrValidation — обёртка над любой service-валидацией CRUD-входа (битый
	// name/type/config/secret_ref/event_types/projection). Handler-сторона
	// (OpenAPI/MCP) маппит её в 422 validation-failed; public-detail безопасен
	// (формируется валидаторами без internal SQL/stack — см. [PublicMessage]).
	ErrValidation = errors.New("herald: validation failed")

	// ErrEphemeralRequiresVoyage — нарушен инвариант ephemeral⟺voyage_id
	// (ADR-052(g)): разовое правило обязано нести voyage_id, постоянное — не
	// должно. Дублирует CHECK tidings_ephemeral_voyage_consistent (defence in
	// depth + дружелюбная ошибка до похода в БД). Заворачивается в [ErrValidation].
	ErrEphemeralRequiresVoyage = errors.New("herald: ephemeral tiding requires voyage_id (and non-ephemeral must not set it)")
)

// IsValidationError — true, если err — service-валидация CRUD-входа
// ([ErrValidation]-обёртка). Используется handler-ами для маппинга в 422.
func IsValidationError(err error) bool {
	return errors.Is(err, ErrValidation)
}

// PublicMessage возвращает безопасный для клиента текст валидационной ошибки:
// trim обёртки `herald: validation failed: ` и внутреннего pkg-префикса
// `herald: `. На не-валидационных ошибках caller их сюда не передаёт (проверяет
// IsValidationError первым).
func PublicMessage(err error) string {
	if err == nil {
		return ""
	}
	// ErrValidation обёрнут как fmt.Errorf("%w: <validator-msg>", ErrValidation),
	// поэтому err.Error() = "herald: validation failed: <validator-msg>". Берём
	// исходный validator-msg (он уже public), убирая обёртку и pkg-префикс.
	msg := strings.TrimPrefix(err.Error(), "herald: validation failed: ")
	return strings.TrimPrefix(msg, "herald: ")
}

// wrapValidation оборачивает валидационную ошибку в [ErrValidation], сохраняя
// исходное сообщение для public-detail И цепочку errors.Is до вложенного
// sentinel-а (например [ErrEphemeralRequiresVoyage]). nil → nil. Двойной %w
// (Go 1.20+) делает результат сопоставимым и с ErrValidation, и с обёрнутым err.
func wrapValidation(err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("%w: %w", ErrValidation, err)
}

const (
	pgErrCodeUniqueViolation     = "23505"
	pgErrCodeForeignKeyViolation = "23503"
	pgErrCodeCheckViolation      = "23514"
)

// ExecQueryRower — узкое подмножество pgxpool.Pool, нужное CRUD-у. Симметрично
// augur/pushprovider: unit-тесты ходят через fake без подъёма PG, production
// даёт реальный pool / Conn / Tx.
type ExecQueryRower interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}

// Compile-time check.
var (
	_ ExecQueryRower = (*pgx.Conn)(nil)
	_ ExecQueryRower = (*pgxpool.Pool)(nil)
	_ ExecQueryRower = (pgx.Tx)(nil)
)

// --- Herald -----------------------------------------------------------

const heraldColumns = `name, type, config, secret_ref, enabled, created_at, updated_at, created_by_aid`

const heraldInsertSQL = `
INSERT INTO heralds (name, type, config, secret_ref, enabled, created_by_aid)
VALUES ($1, $2, $3, $4, $5, $6)
RETURNING created_at, updated_at
`

const heraldSelectByNameSQL = `
SELECT ` + heraldColumns + `
FROM heralds
WHERE name = $1
`

const heraldUpdateSQL = `
UPDATE heralds
SET type = $2,
    config = $3,
    secret_ref = $4,
    enabled = $5,
    updated_at = NOW()
WHERE name = $1
`

// InsertHerald вставляет новый Herald-канал.
//
// Pre-conditions (service-валидация):
//   - h.Name матчит [NamePattern];
//   - h.Type ∈ closed enum ([ValidHeraldType]);
//   - h.Config валиден для типа ([ValidateConfig] — webhook url + SSRF-контур);
//   - h.SecretRef (если задан) — vault-ref ([ValidateSecretRef]).
//
// Возврат [ErrHeraldExists] на UNIQUE по PK; wrapped fmt.Errorf на FK-/CHECK-
// violation.
func InsertHerald(ctx context.Context, db ExecQueryRower, h *Herald) error {
	if h == nil {
		return fmt.Errorf("herald: nil herald")
	}
	if err := validateHerald(h); err != nil {
		return err
	}

	configBytes, err := marshalConfig(h.Config)
	if err != nil {
		return fmt.Errorf("herald: marshal config: %w", err)
	}

	row := db.QueryRow(ctx, heraldInsertSQL,
		h.Name, string(h.Type), configBytes, secretRefArg(h.SecretRef), h.Enabled, aidArg(h.CreatedByAID),
	)
	if err := row.Scan(&h.CreatedAt, &h.UpdatedAt); err != nil {
		return mapHeraldInsertError(err)
	}
	return nil
}

func mapHeraldInsertError(err error) error {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		switch pgErr.Code {
		case pgErrCodeUniqueViolation:
			return fmt.Errorf("%w (constraint %s): %w", ErrHeraldExists, pgErr.ConstraintName, err)
		case pgErrCodeForeignKeyViolation:
			return fmt.Errorf("herald: FK violation on %s: %w", pgErr.ConstraintName, err)
		case pgErrCodeCheckViolation:
			return fmt.Errorf("herald: CHECK violation on %s: %w", pgErr.ConstraintName, err)
		}
	}
	return fmt.Errorf("herald: insert herald: %w", err)
}

// SelectHeraldByName читает Herald по PK. [ErrHeraldNotFound] при pgx.ErrNoRows.
func SelectHeraldByName(ctx context.Context, db ExecQueryRower, name string) (*Herald, error) {
	return scanHerald(db.QueryRow(ctx, heraldSelectByNameSQL, name))
}

func scanHerald(row pgx.Row) (*Herald, error) {
	var (
		h            Herald
		typeStr      string
		configBytes  []byte
		secretRef    *string
		createdByAID *string
	)
	err := row.Scan(
		&h.Name, &typeStr, &configBytes, &secretRef, &h.Enabled,
		&h.CreatedAt, &h.UpdatedAt, &createdByAID,
	)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrHeraldNotFound
		}
		return nil, fmt.Errorf("herald: scan herald: %w", err)
	}
	h.Type = HeraldType(typeStr)
	h.SecretRef = secretRef
	h.CreatedByAID = createdByAID
	if len(configBytes) > 0 {
		if err := json.Unmarshal(configBytes, &h.Config); err != nil {
			return nil, fmt.Errorf("herald: unmarshal config: %w", err)
		}
	}
	return &h, nil
}

// SelectAllHeralds возвращает страницу Herald-ов и общее количество.
//
// Сортировка — `updated_at DESC, name ASC` (свежие выше; tie-break по имени для
// устойчивой пагинации). Total и items — двумя запросами вне общей транзакции
// (eventually consistent, как augur/pushprovider).
func SelectAllHeralds(ctx context.Context, db ExecQueryRower, offset, limit int) ([]*Herald, int, error) {
	if offset < 0 {
		return nil, 0, fmt.Errorf("herald: offset must be >= 0, got %d", offset)
	}
	if limit < 1 {
		return nil, 0, fmt.Errorf("herald: limit must be >= 1, got %d", limit)
	}

	var total int
	if err := db.QueryRow(ctx, "SELECT COUNT(*) FROM heralds").Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("herald: count heralds: %w", err)
	}

	const listSQL = `SELECT ` + heraldColumns + `
FROM heralds
ORDER BY updated_at DESC, name ASC
OFFSET $1 LIMIT $2`
	rows, err := db.Query(ctx, listSQL, offset, limit)
	if err != nil {
		return nil, 0, fmt.Errorf("herald: list heralds query: %w", err)
	}
	defer rows.Close()

	out := make([]*Herald, 0, limit)
	for rows.Next() {
		h, err := scanHerald(rows)
		if err != nil {
			return nil, 0, err
		}
		out = append(out, h)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, fmt.Errorf("herald: list heralds iter: %w", err)
	}
	return out, total, nil
}

// UpdateHerald заменяет mutable-поля Herald-а (type/config/secret_ref/enabled,
// replace-семантика). name (PK) immutable. [ErrHeraldNotFound] если PK не найден.
func UpdateHerald(ctx context.Context, db ExecQueryRower, h *Herald) error {
	if h == nil {
		return fmt.Errorf("herald: nil herald")
	}
	if err := validateHerald(h); err != nil {
		return err
	}

	configBytes, err := marshalConfig(h.Config)
	if err != nil {
		return fmt.Errorf("herald: marshal config: %w", err)
	}

	tag, err := db.Exec(ctx, heraldUpdateSQL,
		h.Name, string(h.Type), configBytes, secretRefArg(h.SecretRef), h.Enabled,
	)
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == pgErrCodeCheckViolation {
			return fmt.Errorf("herald: CHECK violation on %s: %w", pgErr.ConstraintName, err)
		}
		return fmt.Errorf("herald: update herald: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrHeraldNotFound
	}
	return nil
}

// DeleteHerald удаляет Herald по PK. Все его Tiding-ы уходят каскадом (ON DELETE
// CASCADE, ADR-052(a)). [ErrHeraldNotFound] если строки не было.
func DeleteHerald(ctx context.Context, db ExecQueryRower, name string) error {
	tag, err := db.Exec(ctx, "DELETE FROM heralds WHERE name = $1", name)
	if err != nil {
		return fmt.Errorf("herald: delete herald: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrHeraldNotFound
	}
	return nil
}

// --- Tiding -----------------------------------------------------------

const tidingColumns = `name, herald, event_types, only_failures, only_changes, incarnation, cadence, task, ephemeral, voyage_id, created_from_cadence_id, annotations, projection, enabled, created_at, updated_at, created_by_aid`

const tidingInsertSQL = `
INSERT INTO tidings (name, herald, event_types, only_failures, only_changes, incarnation, cadence, task, ephemeral, voyage_id, created_from_cadence_id, annotations, projection, enabled, created_by_aid)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15)
RETURNING created_at, updated_at
`

const tidingSelectByNameSQL = `
SELECT ` + tidingColumns + `
FROM tidings
WHERE name = $1
`

const tidingUpdateSQL = `
UPDATE tidings
SET herald = $2,
    event_types = $3,
    only_failures = $4,
    only_changes = $5,
    incarnation = $6,
    cadence = $7,
    task = $8,
    ephemeral = $9,
    voyage_id = $10,
    created_from_cadence_id = $11,
    annotations = $12,
    projection = $13,
    enabled = $14,
    updated_at = NOW()
WHERE name = $1
`

// InsertTiding вставляет новое Tiding-правило.
//
// Pre-conditions (service-валидация):
//   - t.Name матчит [NamePattern];
//   - t.Herald непустой (FK на heralds — на existence проверяет БД);
//   - t.EventTypes валиден ([ValidateEventTypes] — непустой + run-scope).
//
// Возврат [ErrTidingExists] на UNIQUE по PK; [ErrHeraldNotFound] если Herald
// по FK не существует (FK-violation на tidings_herald_fk); wrapped fmt.Errorf
// на прочие FK-/CHECK-violation.
func InsertTiding(ctx context.Context, db ExecQueryRower, t *Tiding) error {
	if t == nil {
		return fmt.Errorf("herald: nil tiding")
	}
	if err := validateTiding(t); err != nil {
		return err
	}

	annotationsBytes, err := marshalAnnotations(t.Annotations)
	if err != nil {
		return fmt.Errorf("herald: marshal annotations: %w", err)
	}

	row := db.QueryRow(ctx, tidingInsertSQL,
		t.Name, t.Herald, t.EventTypes, t.OnlyFailures, t.OnlyChanges,
		optStrArg(t.Incarnation), optStrArg(t.Cadence), optStrArg(t.Task),
		t.Ephemeral, optStrArg(t.VoyageID), optStrArg(t.CreatedFromCadenceID),
		annotationsBytes, projectionArg(t.Projection),
		t.Enabled, aidArg(t.CreatedByAID),
	)
	if err := row.Scan(&t.CreatedAt, &t.UpdatedAt); err != nil {
		return mapTidingInsertError(err)
	}
	return nil
}

func mapTidingInsertError(err error) error {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		switch pgErr.Code {
		case pgErrCodeUniqueViolation:
			return fmt.Errorf("%w (constraint %s): %w", ErrTidingExists, pgErr.ConstraintName, err)
		case pgErrCodeForeignKeyViolation:
			if pgErr.ConstraintName == "tidings_herald_fk" {
				return fmt.Errorf("%w (tiding references it): %w", ErrHeraldNotFound, err)
			}
			return fmt.Errorf("herald: tiding FK violation on %s: %w", pgErr.ConstraintName, err)
		case pgErrCodeCheckViolation:
			return fmt.Errorf("herald: tiding CHECK violation on %s: %w", pgErr.ConstraintName, err)
		}
	}
	return fmt.Errorf("herald: insert tiding: %w", err)
}

// SelectTidingByName читает Tiding по PK. [ErrTidingNotFound] при pgx.ErrNoRows.
func SelectTidingByName(ctx context.Context, db ExecQueryRower, name string) (*Tiding, error) {
	return scanTiding(db.QueryRow(ctx, tidingSelectByNameSQL, name))
}

func scanTiding(row pgx.Row) (*Tiding, error) {
	var (
		t                    Tiding
		incarnation          *string
		cadence              *string
		task                 *string
		voyageID             *string
		createdFromCadenceID *string
		annotationsBytes     []byte
		createdByAID         *string
	)
	err := row.Scan(
		&t.Name, &t.Herald, &t.EventTypes, &t.OnlyFailures, &t.OnlyChanges,
		&incarnation, &cadence, &task, &t.Ephemeral, &voyageID, &createdFromCadenceID,
		&annotationsBytes, &t.Projection,
		&t.Enabled, &t.CreatedAt, &t.UpdatedAt, &createdByAID,
	)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrTidingNotFound
		}
		return nil, fmt.Errorf("herald: scan tiding: %w", err)
	}
	t.Incarnation = incarnation
	t.Cadence = cadence
	t.Task = task
	t.VoyageID = voyageID
	t.CreatedFromCadenceID = createdFromCadenceID
	t.CreatedByAID = createdByAID
	if len(annotationsBytes) > 0 {
		if err := json.Unmarshal(annotationsBytes, &t.Annotations); err != nil {
			return nil, fmt.Errorf("herald: unmarshal annotations: %w", err)
		}
	}
	return &t, nil
}

func collectTidings(rows pgx.Rows) ([]*Tiding, error) {
	defer rows.Close()
	var out []*Tiding
	for rows.Next() {
		t, err := scanTiding(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("herald: list tidings iter: %w", err)
	}
	return out, nil
}

// SelectAllTidings возвращает страницу Tiding-ов и общее количество. Сортировка
// `updated_at DESC, name ASC`. Total и items — двумя запросами (eventually
// consistent).
//
// includeEphemeral=false (default) скрывает разовые правила (ADR-052(g)):
// listing отражает только постоянные подписки, управляемые оператором, а
// привязанные к прогону ephemeral-правила — деталь реализации (ADR-042 «тупой
// фронт»: фильтрация на бэке, не клиентская). total считается под тем же
// предикатом, чтобы пагинация не «теряла» страницы. includeEphemeral=true
// возвращает все (отладка).
func SelectAllTidings(ctx context.Context, db ExecQueryRower, includeEphemeral bool, offset, limit int) ([]*Tiding, int, error) {
	if offset < 0 {
		return nil, 0, fmt.Errorf("herald: offset must be >= 0, got %d", offset)
	}
	if limit < 1 {
		return nil, 0, fmt.Errorf("herald: limit must be >= 1, got %d", limit)
	}

	// Предикат скрытия ephemeral. Partial-индекс tidings_ephemeral_voyage_idx
	// покрывает только ephemeral-строки; для default-ветки (NOT ephemeral)
	// БД делает seq-scan по небольшой таблице правил — приемлемо.
	where := ""
	if !includeEphemeral {
		where = " WHERE NOT ephemeral"
	}

	var total int
	if err := db.QueryRow(ctx, "SELECT COUNT(*) FROM tidings"+where).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("herald: count tidings: %w", err)
	}

	listSQL := `SELECT ` + tidingColumns + `
FROM tidings` + where + `
ORDER BY updated_at DESC, name ASC
OFFSET $1 LIMIT $2`
	rows, err := db.Query(ctx, listSQL, offset, limit)
	if err != nil {
		return nil, 0, fmt.Errorf("herald: list tidings query: %w", err)
	}
	tidings, err := collectTidings(rows)
	if err != nil {
		return nil, 0, err
	}
	return tidings, total, nil
}

// SelectTidingsByHerald возвращает все Tiding-правила одного Herald-а (CRUD
// list-by-herald). Сортировка `updated_at DESC, name ASC`.
func SelectTidingsByHerald(ctx context.Context, db ExecQueryRower, herald string) ([]*Tiding, error) {
	const sql = `SELECT ` + tidingColumns + `
FROM tidings
WHERE herald = $1
ORDER BY updated_at DESC, name ASC`
	rows, err := db.Query(ctx, sql, herald)
	if err != nil {
		return nil, fmt.Errorf("herald: list tidings by herald query: %w", err)
	}
	return collectTidings(rows)
}

// UpdateTiding заменяет mutable-поля Tiding-а (replace-семантика). name (PK)
// immutable. [ErrTidingNotFound] если PK не найден; [ErrHeraldNotFound] если
// новый herald по FK не существует.
func UpdateTiding(ctx context.Context, db ExecQueryRower, t *Tiding) error {
	if t == nil {
		return fmt.Errorf("herald: nil tiding")
	}
	if err := validateTiding(t); err != nil {
		return err
	}

	annotationsBytes, err := marshalAnnotations(t.Annotations)
	if err != nil {
		return fmt.Errorf("herald: marshal annotations: %w", err)
	}

	tag, err := db.Exec(ctx, tidingUpdateSQL,
		t.Name, t.Herald, t.EventTypes, t.OnlyFailures, t.OnlyChanges,
		optStrArg(t.Incarnation), optStrArg(t.Cadence), optStrArg(t.Task),
		t.Ephemeral, optStrArg(t.VoyageID), optStrArg(t.CreatedFromCadenceID),
		annotationsBytes, projectionArg(t.Projection),
		t.Enabled,
	)
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) {
			switch pgErr.Code {
			case pgErrCodeForeignKeyViolation:
				if pgErr.ConstraintName == "tidings_herald_fk" {
					return fmt.Errorf("%w (tiding references it): %w", ErrHeraldNotFound, err)
				}
				return fmt.Errorf("herald: tiding FK violation on %s: %w", pgErr.ConstraintName, err)
			case pgErrCodeCheckViolation:
				return fmt.Errorf("herald: tiding CHECK violation on %s: %w", pgErr.ConstraintName, err)
			}
		}
		return fmt.Errorf("herald: update tiding: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrTidingNotFound
	}
	return nil
}

// DeleteTiding удаляет Tiding по PK. [ErrTidingNotFound] если строки не было.
func DeleteTiding(ctx context.Context, db ExecQueryRower, name string) error {
	tag, err := db.Exec(ctx, "DELETE FROM tidings WHERE name = $1", name)
	if err != nil {
		return fmt.Errorf("herald: delete tiding: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrTidingNotFound
	}
	return nil
}

// --- helpers ----------------------------------------------------------

// validateHerald — service-валидация полей Herald до записи. Все ошибки
// заворачиваются в [ErrValidation] (handler → 422). Каждая под-проверка несёт
// public-message (без internal SQL/stack).
func validateHerald(h *Herald) error {
	if !ValidName(h.Name) {
		return wrapValidation(fmt.Errorf("invalid name %q (must match %s)", h.Name, NamePattern))
	}
	if !ValidHeraldType(h.Type) {
		return wrapValidation(fmt.Errorf("invalid type %q (must be webhook)", h.Type))
	}
	if err := ValidateConfig(h.Type, h.Config); err != nil {
		return wrapValidation(err)
	}
	if err := ValidateSecretRef(h.Type, h.SecretRef); err != nil {
		return wrapValidation(err)
	}
	return nil
}

// validateTiding — service-валидация полей Tiding до записи. Ошибки в
// [ErrValidation]. existence Herald-а по FK проверяет БД (см. mapTidingInsertError).
func validateTiding(t *Tiding) error {
	// Пустую строку VoyageID нормализуем к nil ДО маппинга, чтобы domain-guard и
	// SQL-arg (optStrArg) согласованно трактовали её как NULL: иначе guard ниже
	// пропускал бы non-ephemeral+&"" (как «не задан»), а optStrArg писал бы ''
	// → CHECK tidings_ephemeral_voyage_consistent падал бы 500-ой (симметрично
	// тому, как aidArg трактует пустой AID как NULL).
	if t.VoyageID != nil && *t.VoyageID == "" {
		t.VoyageID = nil
	}
	// Пустую строку task-селектора нормализуем к nil: nil = «без фильтра», а
	// пустой адрес changed_tasks им матчиться не должен (ADR-052 §l). Без этого
	// optStrArg писал бы `''` в колонку — мёртвый селектор, не матчащий ничего.
	if t.Task != nil && *t.Task == "" {
		t.Task = nil
	}
	// Пустую строку origin-маркера нормализуем к nil: nil = «заведено не формой
	// Cadence», пустой '' в TEXT-колонке нарушил бы FK на cadences(id) (parity
	// VoyageID/Task). Непустое значение проверит БД (FK existence на cadences).
	if t.CreatedFromCadenceID != nil && *t.CreatedFromCadenceID == "" {
		t.CreatedFromCadenceID = nil
	}
	if !ValidName(t.Name) {
		return wrapValidation(fmt.Errorf("invalid tiding name %q (must match %s)", t.Name, NamePattern))
	}
	if t.Herald == "" {
		return wrapValidation(fmt.Errorf("tiding herald is empty"))
	}
	if err := ValidateEventTypes(t.EventTypes); err != nil {
		return wrapValidation(err)
	}
	// Инвариант ephemeral⟺voyage_id (ADR-052(g), defence in depth поверх CHECK):
	// разовое правило обязано нести voyage_id, постоянное — не должно. VoyageID
	// здесь уже нормализован (nil вместо пустой строки), поэтому `!= nil`
	// достаточно — `*t.VoyageID != ""` тут уже инвариантно истинно.
	if t.Ephemeral != (t.VoyageID != nil) {
		return wrapValidation(ErrEphemeralRequiresVoyage)
	}
	if err := ValidateProjection(t.Projection); err != nil {
		return wrapValidation(err)
	}
	return nil
}

// secretRefArg — nil-string → NULL для nullable secret_ref-колонки.
func secretRefArg(ref *string) any {
	if ref == nil {
		return nil
	}
	return *ref
}

// optStrArg — nil-string → NULL для опц. селекторов incarnation/cadence/voyage_id.
func optStrArg(s *string) any {
	if s == nil {
		return nil
	}
	return *s
}

// projectionArg — nil/пустой slice → пустой TEXT[] (колонка NOT NULL DEFAULT '{}',
// pgx требует не-nil). Непустой передаётся как есть.
func projectionArg(p []string) []string {
	if p == nil {
		return []string{}
	}
	return p
}

// aidArg — nil/пустой AID → NULL для nullable created_by_aid (FK ON DELETE SET
// NULL). Пустую строку трактуем как NULL: пустой AID нарушил бы FK на operators.
func aidArg(aid *string) any {
	if aid == nil || *aid == "" {
		return nil
	}
	return *aid
}
