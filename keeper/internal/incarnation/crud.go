package incarnation

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/souls-guild/soul-stack/keeper/internal/statemigrate"
	"github.com/souls-guild/soul-stack/shared/audit"
)

// Sentinel-ошибки CRUD-слоя. Handler-сторона маппит:
//   - ErrIncarnationAlreadyExists → 409 incarnation-already-exists.
//   - ErrIncarnationNotFound      → 404 not-found.
//   - ErrIncarnationNotLocked     → 409 incarnation-locked (unlock возможен
//     из error_locked / migration_failed / destroy_failed; applying/ready/
//     destroying → отказ).
//   - ErrIncarnationBusy          → 409 (upgrade отклонён: идёт прогон,
//     status=applying).
//   - ErrIncarnationLocked        → 409 (upgrade отклонён: статус
//     error_locked / migration_failed — требуется unlock).
//   - ErrDowngradeUnsupported     → 409 (target schema-версия ниже текущей;
//     ADR-019 forward-only).
//   - ErrSchemaVersionMismatch    → 409 (текущая schema-версия не совпала с
//     цепочкой: кто-то проапгрейдил между resolve и FOR UPDATE).
var (
	ErrIncarnationAlreadyExists = errors.New("incarnation: name already exists")
	ErrIncarnationNotFound      = errors.New("incarnation: name not found")
	ErrIncarnationNotLocked     = errors.New("incarnation: not in unlockable status")
	// ErrIncarnationNotErrorLocked — статус НЕ error_locked: rerun-create
	// допустим строго из error_locked (architecture.md → «Атомарность и
	// error_locked»: scope ЖЁСТКО ограничен rerun scenario `create`; для
	// migration_failed / destroy_failed / прочих — обычный unlock + ручной run).
	// Отдельный sentinel от [ErrIncarnationNotLocked] (тот шире — покрывает три
	// блокирующих статуса): rerun сужает допуск до одного.
	ErrIncarnationNotErrorLocked = errors.New("incarnation: not in error_locked status (rerun-create requires error_locked)")
	// ErrRerunScenarioNotCreate — последний прогон, упавший в error_locked, был
	// НЕ create-сценарием (например add_user). rerun-create по контракту
	// (architecture.md → «Атомарность и error_locked») перезапускает строго
	// bootstrap `create`; для произвольного упавшего сценария — обычный unlock +
	// ручной run нужного сценария, иначе rerun исполнил бы create вместо
	// фактически провалившейся операции. Handler маппит в 409 incarnation-locked.
	ErrRerunScenarioNotCreate = errors.New("incarnation: last failed scenario is not `create` — rerun-create restarts `create` only")
	ErrIncarnationBusy        = errors.New("incarnation: run in progress (applying)")
	ErrIncarnationLocked      = errors.New("incarnation: locked — unlock required before upgrade")
	ErrDowngradeUnsupported   = errors.New("incarnation: schema downgrade unsupported (forward-only, ADR-019)")
	ErrSchemaVersionMismatch  = errors.New("incarnation: current schema version does not match migration chain")
	// ErrAlreadyFinalized — single-winner state-commit (ADR-027(j), W1): строка
	// incarnation существует, но уже НЕ в рабочем статусе прогона
	// (applying / destroying) — другой обработчик выиграл финализацию (RunResult
	// vs recovery-перехват). НЕ ошибка: caller (commitSuccess / lockIncarnation)
	// трактует как no-op (логирует «уже финализировано другим»), как
	// [DeleteAfterTeardown] трактует RowsAffected==0 для DELETE.
	ErrAlreadyFinalized = errors.New("incarnation: already finalized by another committer")
)

const (
	pgErrCodeUniqueViolation     = "23505"
	pgErrCodeForeignKeyViolation = "23503"
	pgErrCodeCheckViolation      = "23514"
)

// ExecQueryRower — узкое подмножество интерфейса pgxpool.Pool, нужное
// CRUD-у. Симметрично [operator.ExecQueryRower]: unit-тесты ходят через
// fake без подъёма PG, production даёт реальный pool / Conn / Tx.
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

// insertSQL — INSERT с RETURNING для получения server-side created_at /
// updated_at (DEFAULT NOW()) одной round-trip-ой.
const insertSQL = `
INSERT INTO incarnation (
    name, service, service_version, state_schema_version,
    spec, state, status, status_details, created_by_aid, covens
) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
RETURNING created_at, updated_at
`

// selectByNameSQL — SELECT всех колонок по PK.
const selectByNameSQL = `
SELECT name, service, service_version, state_schema_version,
       spec, state, status, status_details, created_by_aid,
       created_at, updated_at, covens,
       last_drift_check_at, last_drift_summary
FROM incarnation
WHERE name = $1
`

// StateOp — оператор сравнения state-предиката ([StateEq]). Закрытый набор;
// любой иной → [ErrInvalidStateOp].
type StateOp string

const (
	StateOpEq  StateOp = "eq" // равенство (текстовое, jsonb ->> = $n)
	StateOpNe  StateOp = "ne" // неравенство
	StateOpGt  StateOp = "gt" // больше (числовой cast ::numeric)
	StateOpGte StateOp = "gte"
	StateOpLt  StateOp = "lt"
	StateOpLte StateOp = "lte"
)

// numericStateOps — операторы, требующие числового сравнения (cast обеих
// сторон в ::numeric). Остальные (eq/ne) — текстовое сравнение jsonb ->>.
var numericStateOps = map[StateOp]string{
	StateOpGt:  ">",
	StateOpGte: ">=",
	StateOpLt:  "<",
	StateOpLte: "<=",
}

// textStateOps — операторы текстового сравнения над jsonb ->>.
var textStateOps = map[StateOp]string{
	StateOpEq: "=",
	StateOpNe: "<>",
}

// StateEq — предикат по полю jsonb-колонки `state` (фаза 1, jsonb-pushdown).
// Path — ключ верхнего уровня state-объекта (например `redis_version`);
// валидируется форматным whitelist-ом [statePathPattern] — НИКОГДА не
// конкатенируется как SQL-идентификатор без проверки (защита от инъекции).
// Value всегда уходит bind-параметром ($n), не в текст SQL.
//
// MVP: только top-level ключи (без вложенного пути a.b.c). Existence поля
// против service-specific state_schema НЕ проверяется (разные сервисы — разные
// поля): несуществующий ключ даёт `state->>'x' = $n` → NULL → пустой результат,
// что является валидным «ничего не найдено».
type StateEq struct {
	Path  string
	Op    StateOp
	Value string
}

// SortDir — направление сортировки [ListFilter.SortBy].
type SortDir string

const (
	SortAsc  SortDir = "asc"
	SortDesc SortDir = "desc"
)

// sortableColumns — базовые колонки, допустимые в [ListFilter.SortBy]
// (closed whitelist; state-поля идут отдельным префиксом `state.`). Маппинг
// логического имени → SQL-выражение (на случай расхождения, сейчас 1:1).
var sortableColumns = map[string]string{
	"created_at": "created_at",
	"name":       "name",
	"status":     "status",
	"service":    "service",
}

// statePathPrefix — префикс sort-поля, означающий сортировку по jsonb
// state-полю (`sort=state.redis_version`).
const statePathPrefix = "state."

// statePathPattern — форматный whitelist jsonb-path-ключа. Только нижний
// регистр, цифры, подчёркивание; первый символ — буква. Закрывает SQL-инъекцию
// через идентификатор: всё, что не матчит, → [ErrInvalidStatePath]. Existence
// поля против state_schema НЕ проверяется (см. [StateEq]).
var statePathPattern = regexp.MustCompile(`^[a-z][a-z0-9_]*$`)

// Sentinel-ошибки валидации фильтра/сортировки. Handler маппит все в 422.
var (
	ErrInvalidStatePath  = errors.New("incarnation: invalid state path (must match [a-z][a-z0-9_]*)")
	ErrInvalidStateOp    = errors.New("incarnation: invalid state predicate operator")
	ErrInvalidStateValue = errors.New("incarnation: invalid state predicate value (numeric operator requires a number)")
	ErrInvalidSortField  = errors.New("incarnation: invalid sort field")
	ErrInvalidSortDir    = errors.New("incarnation: invalid sort direction")
)

// ListFilter — фильтры для [SelectAll]. Пустые поля означают «не фильтровать».
//
// Coven — exact-match по any-of в `incarnation.covens[]` (declared env-теги,
// ADR-008 amendment a). На SQL — тот же предикат `$n = ANY(covens)`, что
// используется в `soul.ListFilter.Coven` (souls.coven[]); семантически Coven
// тут — env-тег incarnation (например `prod` / `staging`), не Coven-метка
// хоста.
//
// StatePredicates — фильтры по полям jsonb-колонки `state` (jsonb-pushdown,
// фаза 1). AND-комбинируются с базовыми фильтрами и между собой. Path каждого
// предиката валидируется форматным whitelist-ом ([statePathPattern]).
//
// SortBy / SortDir — сортировка. SortBy: базовая колонка из [sortableColumns]
// либо `state.<field>` (jsonb ->>). Пустой SortBy → legacy-порядок
// `created_at DESC, name ASC`. SortDir по умолчанию (пустой) — asc. Tie-break
// по `name ASC` добавляется всегда (стабильная пагинация).
type ListFilter struct {
	Service         string
	Status          Status
	Coven           string
	StatePredicates []StateEq
	SortBy          string
	SortDir         SortDir
}

// ListScope — RBAC scope-граница видимости (`GET /v1/incarnations`, ADR-047
// S3b-3), ОТДЕЛЬНАЯ от пользовательских [ListFilter]: filter — что оператор
// попросил показать (query-params), scope — что ему вообще ПОЛОЖЕНО видеть (из
// JWT, резолвится handler-ом из [rbac.Purview]). Оба пересекаются AND-ом в WHERE
// (фильтр сужает ВНУТРИ scope, не наоборот).
//
// Измерения scope (Covens + StateNames) объединяются OR-ом («всё мне
// доступное»): incarnation видна, если она в одном из scope-ковенов ЛИБО её
// state удовлетворяет одному из state-предикатов scope. Это OR между
// измерениями, в отличие от AND filter∩scope.
//
//   - Covens — coven∪{name} матчер (ADR-008 amendment a): scope-coven матчит
//     incarnation и по `covens[] && ARRAY[Covens]`, и по `name = ANY(Covens)`
//     (имя incarnation = корневая Coven-метка). Это шире, чем
//     [ListFilter.Coven] (тот матчит только covens[]).
//   - StateNames — имена incarnation-ов, чей state УЖЕ удовлетворил state-CEL-
//     предикатам scope (StateExprs, ADR-047 S2c). Резолвятся ДО SQL через
//     keeper/internal/statepredicate (CEL-движок не дублируется), затем
//     приходят сюда множеством имён → `name = ANY(StateNames)` чистым
//     SQL-pushdown-ом (total/offset когерентны, без Go-постфильтр-дрейфа).
//
// Семантика — fail-closed (ADR-047): пустой scope (Covens и StateNames пусты)
// при !Unrestricted даёт заведомо-ложный предикат (ни одной incarnation), а НЕ
// весь список. Unrestricted=true снимает scope-фильтр (весь список). Пустой
// scope handler НЕ передаёт сюда — он отдаёт пустой ответ до запроса в БД (как
// souls-пилот), но defensive-ветка ниже сохраняет fail-closed и здесь.
type ListScope struct {
	Covens       []string
	StateNames   []string
	Unrestricted bool
}

// Create вставляет новый incarnation. status проставляется caller-ом
// (handler-stub передаёт [StatusReady]); scenario-runner в M0.6c-2 будет
// проставлять APPLYING до завершения прогона.
//
// Pre-conditions:
//   - inc.Name соответствует [NamePattern];
//   - inc.Service / inc.ServiceVersion непустые;
//   - inc.Status — одно из четырёх MVP-значений.
//
// Возврат:
//   - [ErrIncarnationAlreadyExists] на UNIQUE по PK.
//   - wrapped fmt.Errorf на FK-violation (`created_by_aid` ссылается на
//     несуществующий AID) и CHECK-violation (status / name format).
func Create(ctx context.Context, db ExecQueryRower, inc *Incarnation) error {
	if inc == nil {
		return fmt.Errorf("incarnation: nil incarnation")
	}
	if !ValidName(inc.Name) {
		return fmt.Errorf("incarnation: invalid name %q (must match %s)", inc.Name, NamePattern)
	}
	if inc.Service == "" {
		return fmt.Errorf("incarnation: service is empty")
	}
	if inc.ServiceVersion == "" {
		return fmt.Errorf("incarnation: service_version is empty")
	}
	if !ValidStatus(inc.Status) {
		return fmt.Errorf("incarnation: invalid status %q", inc.Status)
	}
	if inc.StateSchemaVersion <= 0 {
		// 1 — каноническая стартовая версия (ADR-019); 0 / отрицательное —
		// программная ошибка caller-а.
		return fmt.Errorf("incarnation: state_schema_version must be > 0, got %d", inc.StateSchemaVersion)
	}

	specBytes, err := marshalJSONB(inc.Spec)
	if err != nil {
		return fmt.Errorf("incarnation: marshal spec: %w", err)
	}
	stateBytes, err := marshalJSONB(inc.State)
	if err != nil {
		return fmt.Errorf("incarnation: marshal state: %w", err)
	}
	var statusDetailsArg any
	if inc.StatusDetails != nil {
		b, err := json.Marshal(inc.StatusDetails)
		if err != nil {
			return fmt.Errorf("incarnation: marshal status_details: %w", err)
		}
		statusDetailsArg = b
	}
	var createdByAID any
	if inc.CreatedByAID != nil {
		createdByAID = *inc.CreatedByAID
	}

	// covens — NOT NULL DEFAULT '{}': nil-slice кодируем как пустой массив
	// (pgx иначе передал бы NULL → violation NOT NULL).
	covens := inc.Covens
	if covens == nil {
		covens = []string{}
	}

	row := db.QueryRow(ctx, insertSQL,
		inc.Name,
		inc.Service,
		inc.ServiceVersion,
		inc.StateSchemaVersion,
		specBytes,
		stateBytes,
		string(inc.Status),
		statusDetailsArg,
		createdByAID,
		covens,
	)
	if err := row.Scan(&inc.CreatedAt, &inc.UpdatedAt); err != nil {
		return mapInsertError(err)
	}
	return nil
}

func mapInsertError(err error) error {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		switch pgErr.Code {
		case pgErrCodeUniqueViolation:
			return fmt.Errorf("%w (constraint %s): %w",
				ErrIncarnationAlreadyExists, pgErr.ConstraintName, err)
		case pgErrCodeForeignKeyViolation:
			return fmt.Errorf("incarnation: FK violation on %s: %w", pgErr.ConstraintName, err)
		case pgErrCodeCheckViolation:
			return fmt.Errorf("incarnation: CHECK violation on %s: %w", pgErr.ConstraintName, err)
		}
	}
	return fmt.Errorf("incarnation: insert: %w", err)
}

// SelectByName читает incarnation по PK. [ErrIncarnationNotFound] при
// pgx.ErrNoRows.
func SelectByName(ctx context.Context, db ExecQueryRower, name string) (*Incarnation, error) {
	row := db.QueryRow(ctx, selectByNameSQL, name)
	return scanIncarnation(row)
}

func scanIncarnation(row pgx.Row) (*Incarnation, error) {
	var (
		inc                Incarnation
		statusStr          string
		specBytes          []byte
		stateBytes         []byte
		statusDetailsBytes []byte
		createdByAID       *string
		driftSummaryBytes  []byte
	)
	err := row.Scan(
		&inc.Name,
		&inc.Service,
		&inc.ServiceVersion,
		&inc.StateSchemaVersion,
		&specBytes,
		&stateBytes,
		&statusStr,
		&statusDetailsBytes,
		&createdByAID,
		&inc.CreatedAt,
		&inc.UpdatedAt,
		&inc.Covens,
		&inc.LastDriftCheckAt,
		&driftSummaryBytes,
	)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrIncarnationNotFound
		}
		return nil, fmt.Errorf("incarnation: scan: %w", err)
	}
	inc.Status = Status(statusStr)
	inc.CreatedByAID = createdByAID
	if inc.Spec, err = unmarshalJSONB(specBytes); err != nil {
		return nil, fmt.Errorf("incarnation: unmarshal spec: %w", err)
	}
	if inc.State, err = unmarshalJSONB(stateBytes); err != nil {
		return nil, fmt.Errorf("incarnation: unmarshal state: %w", err)
	}
	if len(statusDetailsBytes) > 0 {
		if err := json.Unmarshal(statusDetailsBytes, &inc.StatusDetails); err != nil {
			return nil, fmt.Errorf("incarnation: unmarshal status_details: %w", err)
		}
	}
	if len(driftSummaryBytes) > 0 {
		var summary DriftScanSummary
		if err := json.Unmarshal(driftSummaryBytes, &summary); err != nil {
			return nil, fmt.Errorf("incarnation: unmarshal last_drift_summary: %w", err)
		}
		inc.LastDriftSummary = &summary
	}
	return &inc, nil
}

// SelectAll возвращает страницу incarnation-ов с применённым фильтром и
// общее количество элементов, удовлетворяющих фильтру (без offset/limit).
//
// Сортировка управляется [ListFilter.SortBy]/[SortDir]; по умолчанию (пустой
// SortBy) — legacy-порядок `created_at DESC, name ASC` (поздние выше; tie-break
// по имени, иначе пагинация неустойчива при одинаковом таймстемпе). Tie-break
// `name ASC` добавляется всегда.
//
// State-фильтры ([ListFilter.StatePredicates]) и sort по `state.<field>`
// валидируются ДО любого запроса в БД ([ErrInvalidStatePath]/[ErrInvalidStateOp]/
// [ErrInvalidSortField]/[ErrInvalidSortDir]): инъекция через jsonb-path не
// доходит до PG, значения уходят bind-параметрами.
//
// Total и items получаются двумя отдельными запросами вне общей
// транзакции — total в этом эндпоинте **eventually consistent**: новый
// incarnation, появившийся между COUNT и SELECT, даст total на единицу
// больше, чем фактических items на текущей странице. Это сознательный
// выбор: явная транзакция (REPEATABLE READ) ради ориентира-числа дороже,
// чем допустимое расхождение в UI-пагинации.
func SelectAll(ctx context.Context, db ExecQueryRower, filter ListFilter, scope ListScope, offset, limit int) ([]*Incarnation, int, error) {
	if offset < 0 {
		return nil, 0, fmt.Errorf("incarnation: offset must be >= 0, got %d", offset)
	}
	if limit < 1 {
		return nil, 0, fmt.Errorf("incarnation: limit must be >= 1, got %d", limit)
	}

	whereSQL, args, err := buildListWhere(filter, scope)
	if err != nil {
		return nil, 0, err
	}
	orderSQL, err := buildListOrderBy(filter)
	if err != nil {
		return nil, 0, err
	}

	// Total — без offset/limit.
	countSQL := "SELECT COUNT(*) FROM incarnation" + whereSQL
	var total int
	if err := db.QueryRow(ctx, countSQL, args...).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("incarnation: count: %w", err)
	}

	// Items — с offset/limit, append-ом к тем же args.
	listSQL := `SELECT name, service, service_version, state_schema_version,
       spec, state, status, status_details, created_by_aid,
       created_at, updated_at, covens,
       last_drift_check_at, last_drift_summary
FROM incarnation` + whereSQL + orderSQL +
		fmt.Sprintf(" OFFSET $%d LIMIT $%d", len(args)+1, len(args)+2)
	listArgs := append(append([]any{}, args...), offset, limit)

	rows, err := db.Query(ctx, listSQL, listArgs...)
	if err != nil {
		return nil, 0, fmt.Errorf("incarnation: list query: %w", err)
	}
	defer rows.Close()

	var out []*Incarnation
	for rows.Next() {
		inc, err := scanIncarnation(rows)
		if err != nil {
			return nil, 0, err
		}
		out = append(out, inc)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, fmt.Errorf("incarnation: list iter: %w", err)
	}
	return out, total, nil
}

// buildListWhere собирает WHERE-clause и args по непустым полям фильтра.
// Возвращает пустую строку и nil-args при отсутствии фильтров. State-предикаты
// валидируются (path-whitelist + закрытый набор операторов + числовость
// значения для numeric-операторов); невалидный →
// [ErrInvalidStatePath]/[ErrInvalidStateOp]/[ErrInvalidStateValue] без
// обращения к БД.
func buildListWhere(f ListFilter, scope ListScope) (string, []any, error) {
	var (
		clauses []string
		args    []any
	)
	if f.Service != "" {
		args = append(args, f.Service)
		clauses = append(clauses, fmt.Sprintf("service = $%d", len(args)))
	}
	if f.Status != "" {
		args = append(args, string(f.Status))
		clauses = append(clauses, fmt.Sprintf("status = $%d", len(args)))
	}
	if f.Coven != "" {
		args = append(args, f.Coven)
		clauses = append(clauses, fmt.Sprintf("$%d = ANY(covens)", len(args)))
	}
	for _, p := range f.StatePredicates {
		if !statePathPattern.MatchString(p.Path) {
			return "", nil, fmt.Errorf("%w: %q", ErrInvalidStatePath, p.Path)
		}
		// Для числовых операторов значение уходит в SQL как `$n::numeric`.
		// Нечисловое значение (например, опечатка `gt:abc`) на PG-стороне
		// дало бы cast-ошибку 22P02 → 500; отбиваем здесь до запроса в БД
		// (handler маппит ErrInvalidStateValue в 422). eq/ne — текстовые,
		// числовая валидация к ним не применяется.
		if _, numeric := numericStateOps[p.Op]; numeric {
			if _, err := strconv.ParseFloat(p.Value, 64); err != nil {
				return "", nil, fmt.Errorf("%w: %q for operator %q", ErrInvalidStateValue, p.Value, p.Op)
			}
		}
		args = append(args, p.Value)
		placeholder := fmt.Sprintf("$%d", len(args))
		clause, err := stateClause(p.Path, p.Op, placeholder)
		if err != nil {
			return "", nil, err
		}
		clauses = append(clauses, clause)
	}

	// RBAC scope-предикат (ADR-047 S3b-3): AND с пользовательским фильтром.
	clauses, args = appendScopeClause(clauses, args, scope)

	if len(clauses) == 0 {
		return "", nil, nil
	}
	where := " WHERE " + clauses[0]
	for _, c := range clauses[1:] {
		where += " AND " + c
	}
	return where, args, nil
}

// appendScopeClause добавляет RBAC scope-предикат (`GET /v1/incarnations`,
// ADR-047 S3b-3) одним AND-clause к пользовательскому фильтру. Внутри scope
// измерения (coven∪{name} ∪ state-names) объединяются OR-ом — единый блок в
// скобках, чтобы не «протечь» через соседние AND-clause фильтра:
//
//		((covens && ARRAY[$c] OR name = ANY($c)) OR name = ANY($s))
//
//	  - coven∪{name}: scope-coven матчит incarnation и по covens[]-пересечению,
//	    и по равенству имени (ADR-008: имя incarnation — корневая Coven-метка).
//	  - state-names: предрезолвнутые имена incarnation-ов, чей state удовлетворил
//	    state-CEL scope (StateExprs) — приходят множеством, матчатся `name = ANY`.
//
// Unrestricted — без ограничения (scope снят). fail-closed: пустой scope (нет
// ни coven, ни state-names) при !Unrestricted даёт `FALSE` — ни одной
// incarnation (а НЕ весь список). Это симметрично [soul.appendScopeClause].
func appendScopeClause(clauses []string, args []any, scope ListScope) ([]string, []any) {
	if scope.Unrestricted {
		return clauses, args
	}

	var dims []string
	if len(scope.Covens) > 0 {
		args = append(args, scope.Covens)
		pos := len(args)
		// coven∪{name}: пересечение covens[] ИЛИ имя ∈ scope-ковенов.
		dims = append(dims, fmt.Sprintf("(covens && $%d OR name = ANY($%d))", pos, pos))
	}
	if len(scope.StateNames) > 0 {
		args = append(args, scope.StateNames)
		dims = append(dims, fmt.Sprintf("name = ANY($%d)", len(args)))
	}

	if len(dims) == 0 {
		// fail-closed: scope введён (не Unrestricted), но пуст по измерениям —
		// ни одной видимой incarnation. Детерминированный FALSE, не «весь список».
		return append(clauses, "FALSE"), args
	}

	scopeClause := dims[0]
	for _, d := range dims[1:] {
		scopeClause += " OR " + d
	}
	return append(clauses, "("+scopeClause+")"), args
}

// stateClause строит один jsonb-pushdown-предикат. Path уже прошёл
// [statePathPattern] (безопасен как идентификатор), placeholder — bind ($n).
// Текстовые операторы — `state->>'path' OP $n`; числовые — оба операнда
// в ::numeric (`(state->>'path')::numeric OP $n::numeric`).
func stateClause(path string, op StateOp, placeholder string) (string, error) {
	if sqlOp, ok := textStateOps[op]; ok {
		return fmt.Sprintf("state->>'%s' %s %s", path, sqlOp, placeholder), nil
	}
	if sqlOp, ok := numericStateOps[op]; ok {
		return fmt.Sprintf("(state->>'%s')::numeric %s %s::numeric", path, sqlOp, placeholder), nil
	}
	return "", fmt.Errorf("%w: %q", ErrInvalidStateOp, op)
}

// buildListOrderBy формирует ORDER BY-clause из [ListFilter.SortBy]/[SortDir].
// Пустой SortBy → legacy `created_at DESC, name ASC`. Иначе: базовая колонка
// из [sortableColumns] либо `state.<field>` (jsonb ->>, path-whitelist), с
// направлением из SortDir (default asc) и обязательным tie-break `name ASC`.
func buildListOrderBy(f ListFilter) (string, error) {
	if f.SortBy == "" {
		return " ORDER BY created_at DESC, name ASC", nil
	}

	dir, err := sortDirSQL(f.SortDir)
	if err != nil {
		return "", err
	}

	var expr string
	if strings.HasPrefix(f.SortBy, statePathPrefix) {
		path := strings.TrimPrefix(f.SortBy, statePathPrefix)
		if !statePathPattern.MatchString(path) {
			return "", fmt.Errorf("%w: %q", ErrInvalidStatePath, path)
		}
		expr = fmt.Sprintf("state->>'%s'", path)
	} else {
		col, ok := sortableColumns[f.SortBy]
		if !ok {
			return "", fmt.Errorf("%w: %q", ErrInvalidSortField, f.SortBy)
		}
		expr = col
	}
	return fmt.Sprintf(" ORDER BY %s %s, name ASC", expr, dir), nil
}

// sortDirSQL валидирует направление сортировки. Пустое → ASC (default).
func sortDirSQL(d SortDir) (string, error) {
	switch d {
	case "", SortAsc:
		return "ASC", nil
	case SortDesc:
		return "DESC", nil
	}
	return "", fmt.Errorf("%w: %q", ErrInvalidSortDir, d)
}

// finalizableStatuses — рабочие (не-терминальные) статусы прогона, из которых
// разрешён single-winner финальный коммит ([UpdateStateFromRun], ADR-027(j) W1):
//   - applying   — обычный прогон (lockRun перевёл сюда на старте);
//   - destroying — teardown scenario `destroy` (incarnation остаётся в
//     destroying на всё время прогона, S-D1/S-D2b; финальный write — только
//     провал-перевод в destroy_failed через lockIncarnation).
//
// Любой иной (терминальный либо уже-перехваченный) статус → строку выиграл
// другой коммиттер: финальный UPDATE даст RowsAffected==0 → [ErrAlreadyFinalized].
var finalizableStatuses = map[Status]struct{}{
	StatusApplying:   {},
	StatusDestroying: {},
}

// UpdateStateFromRun — атомарная фиксация результата прогона `RunResult`
// (ADR-009, M2.4) c single-winner state-commit (ADR-027(j), W1). Внутри одной
// транзакции:
//
//  1. INSERT в `state_history` snapshot перехода (state_before/_after,
//     scenario, apply_id).
//  2. Single-winner UPDATE incarnation.state + status + status_details +
//     updated_at = NOW() с guard `WHERE name=$1 AND status IN
//     ('applying','destroying')` + RETURNING. Только один обработчик «выигрывает»
//     строку: при гонке recovery-перехвата vs оригинального RunResult двойного
//     коммита терминала не происходит (симметрично single-winner DELETE в
//     [DeleteAfterTeardown]). Guard заменяет прежний SELECT … FOR UPDATE —
//     атомарный CAS-UPDATE сериализует конкурентные коммиты сам.
//
// При status = `error_locked` стейт не меняется (caller обычно передаёт
// stateAfter == stateBefore = текущий state); state_history всё равно
// пишется (фиксируем сам факт неудачного прогона).
//
// `apply_id` — ULID прогона (RunResult.apply_id); идёт и в state_history,
// и в audit через caller. Сам audit-write делает event handler уровнем
// выше (после commit-а, чтобы DB-консистентность не зависела от audit).
//
// `changedByAID` = nil — Soul инициирует прогон без identity Архонта
// (`source: soul_grpc`, см. ADR-022). Допускается non-nil для будущего
// случая, когда прогон триггерится Operator API и AID известен.
//
// Возврат:
//   - [ErrIncarnationNotFound]  — строки с таким name нет совсем.
//   - [ErrAlreadyFinalized]     — строка есть, но статус уже не applying/
//     destroying: финализацию выиграл другой коммиттер (no-op, НЕ паника —
//     caller логирует и продолжает). Транзакция при этом откатывается (caller
//     через pgx.BeginFunc), осиротевший state_history-snapshot не остаётся.
func UpdateStateFromRun(
	ctx context.Context,
	tx ExecQueryRower,
	name, scenario, applyID string,
	stateBefore, stateAfter map[string]any,
	status Status,
	statusDetails map[string]any,
	changedByAID *string,
	historyID string,
) error {
	if !ValidName(name) {
		return fmt.Errorf("incarnation: invalid name %q", name)
	}
	if !ValidStatus(status) {
		return fmt.Errorf("incarnation: invalid status %q", status)
	}
	if applyID == "" {
		return fmt.Errorf("incarnation: empty apply_id")
	}
	if historyID == "" {
		return fmt.Errorf("incarnation: empty history_id")
	}

	stateBeforeBytes, err := marshalJSONB(stateBefore)
	if err != nil {
		return fmt.Errorf("incarnation: marshal state_before: %w", err)
	}
	stateAfterBytes, err := marshalJSONB(stateAfter)
	if err != nil {
		return fmt.Errorf("incarnation: marshal state_after: %w", err)
	}
	var statusDetailsArg any
	if statusDetails != nil {
		b, err := json.Marshal(statusDetails)
		if err != nil {
			return fmt.Errorf("incarnation: marshal status_details: %w", err)
		}
		statusDetailsArg = b
	}
	var changedByArg any
	if changedByAID != nil {
		changedByArg = *changedByAID
	}

	const historyInsertSQL = `
INSERT INTO state_history (
    history_id, incarnation_name, scenario, state_before, state_after,
    changed_by_aid, apply_id
) VALUES ($1, $2, $3, $4, $5, $6, $7)
`
	if _, err := tx.Exec(ctx, historyInsertSQL,
		historyID, name, scenario, stateBeforeBytes, stateAfterBytes, changedByArg, applyID,
	); err != nil {
		return fmt.Errorf("incarnation: insert state_history: %w", err)
	}

	// Single-winner guard: коммит проходит ТОЛЬКО если строка ещё в рабочем
	// статусе прогона (applying / destroying). RETURNING name отдаёт строку
	// победителю; пустой результат (pgx.ErrNoRows) = строки нет ЛИБО статус уже
	// сменился — различаем добором статуса ниже (паттерн MarkDispatched).
	const updateSQL = `
UPDATE incarnation
SET state          = $2,
    status         = $3,
    status_details = $4,
    updated_at     = NOW()
WHERE name = $1 AND status IN ('applying', 'destroying')
RETURNING name
`
	var returnedName string
	err = tx.QueryRow(ctx, updateSQL, name, stateAfterBytes, string(status), statusDetailsArg).Scan(&returnedName)
	if err == nil {
		return nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("incarnation: update state: %w", err)
	}

	// 0 строк: либо строки нет вовсе, либо статус уже терминальный/перехваченный.
	// Добор статуса различает not-found (контракт callers-ов сохранён) и
	// already-finalized (single-winner no-op, кто-то выиграл строку первым).
	const probeStatusSQL = `SELECT status FROM incarnation WHERE name = $1`
	var statusStr string
	if perr := tx.QueryRow(ctx, probeStatusSQL, name).Scan(&statusStr); perr != nil {
		if errors.Is(perr, pgx.ErrNoRows) {
			return ErrIncarnationNotFound
		}
		return fmt.Errorf("incarnation: update state probe: %w", perr)
	}
	if _, ok := finalizableStatuses[Status(statusStr)]; ok {
		// Статус всё ещё рабочий, но UPDATE не затронул строку — единственная
		// причина: гонка чтения внутри одной tx (теоретически недостижима под
		// snapshot tx). Возвращаем not-found-семантику как защитный дефолт, а не
		// тихий no-op.
		return ErrIncarnationNotFound
	}
	return fmt.Errorf("%w (status=%s)", ErrAlreadyFinalized, statusStr)
}

// TxBeginner — узкое подмножество [pgxpool.Pool], нужное транзакционным
// операциям (FOR UPDATE → проверка → mutate → commit одним atomic-блоком).
// Реальный `*pgxpool.Pool` удовлетворяет автоматически; unit-тесты дают
// fake, возвращающий fake-tx.
type TxBeginner interface {
	BeginTx(ctx context.Context, opts pgx.TxOptions) (pgx.Tx, error)
}

var _ TxBeginner = (*pgxpool.Pool)(nil)

// UnlockResult — итог [Unlock]: статус до снятия блока (для reply / audit) и
// идентификатор записанного state_history-snapshot-а.
type UnlockResult struct {
	PreviousStatus Status
	HistoryID      string
}

// unlockScenarioLabel — значение `state_history.scenario` для unlock-перехода.
// Unlock — не прогон scenario, но state_history требует non-null scenario;
// фиксируем сам факт ручного снятия блока под этой меткой.
const unlockScenarioLabel = "unlock"

// Unlock снимает блокирующий статус (ADR-009 / ADR-019): переводит
// incarnation error_locked → ready, migration_failed → ready ИЛИ
// destroy_failed → ready и пишет snapshot-row в state_history (state НЕ
// меняется — last known-good сохраняется; unlock не откатывает и не доделывает
// хосты, оператор берёт ответственность за консистентность, architecture.md →
// «Атомарность и error_locked»).
//
// migration_failed снимается так же безопасно, как error_locked: миграция
// атомарна в одной tx, при фейле rollback оставляет дореформенный
// (консистентный) state, поэтому unlock возвращает incarnation в рабочее
// состояние без риска полу-применённой миграции (ADR-019, атомарность).
//
// destroy_failed (S-D2a) снимается тем же путём: teardown работает с хостами,
// а не с jsonb-state, поэтому при упавшем teardown-е state остаётся
// last known-good — unlock возвращает incarnation в ready без риска
// рассинхрона state-графа. Оператор тем самым отказывается от destroy и берёт
// инстанс обратно в работу (альтернативы — повторить destroy / force-снести —
// появятся в S-D2b/S-D3).
//
// Атомарность: одна транзакция SELECT … FOR UPDATE → проверка статуса →
// INSERT state_history → UPDATE status. FOR UPDATE сериализует unlock
// относительно конкурентного scenario-runner-а (его lockRun лочит ту же
// строку).
//
// Возврат:
//   - [ErrIncarnationNotFound] — name не существует (404).
//   - [ErrIncarnationNotLocked] — статус не error_locked, не migration_failed
//     и не destroy_failed (409): нельзя анлочить ready/applying/destroying.
//
// reason пишется в audit-payload caller-ом (state_history-схема MVP не
// несёт metadata-колонки); previous_status возвращается в [UnlockResult].
func Unlock(ctx context.Context, pool TxBeginner, name, reason, unlockedByAID, historyID string) (*UnlockResult, error) {
	if !ValidName(name) {
		return nil, fmt.Errorf("incarnation: invalid name %q", name)
	}
	if reason == "" {
		return nil, fmt.Errorf("incarnation: unlock reason is empty")
	}
	if historyID == "" {
		return nil, fmt.Errorf("incarnation: empty history_id")
	}

	tx, err := pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return nil, fmt.Errorf("incarnation: begin unlock tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	const selectForUpdateSQL = `
SELECT state, status
FROM incarnation
WHERE name = $1
FOR UPDATE
`
	var (
		stateBytes []byte
		statusStr  string
	)
	if err := tx.QueryRow(ctx, selectForUpdateSQL, name).Scan(&stateBytes, &statusStr); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrIncarnationNotFound
		}
		return nil, fmt.Errorf("incarnation: unlock select: %w", err)
	}
	previous := Status(statusStr)
	if previous != StatusErrorLocked && previous != StatusMigrationFailed && previous != StatusDestroyFailed {
		return nil, ErrIncarnationNotLocked
	}

	var changedByArg any
	if unlockedByAID != "" {
		changedByArg = unlockedByAID
	}

	// state_before == state_after: unlock не меняет state (ADR-009).
	// apply_id = history_id ($1): unlock не привязан к apply-прогону (схема
	// требует NOT NULL, FK на apply_runs нет) — подставляем history_id как
	// уникальный non-null маркер.
	const historyInsertSQL = `
INSERT INTO state_history (
    history_id, incarnation_name, scenario, state_before, state_after,
    changed_by_aid, apply_id
) VALUES ($1, $2, $3, $4, $4, $5, $1)
`
	if _, err := tx.Exec(ctx, historyInsertSQL,
		historyID, name, unlockScenarioLabel, stateBytes, changedByArg,
	); err != nil {
		return nil, fmt.Errorf("incarnation: insert unlock state_history: %w", err)
	}

	// status → ready, status_details сбрасываются (блок снят).
	const updateSQL = `
UPDATE incarnation
SET status = $2, status_details = NULL, updated_at = NOW()
WHERE name = $1
`
	if _, err := tx.Exec(ctx, updateSQL, name, string(StatusReady)); err != nil {
		return nil, fmt.Errorf("incarnation: unlock update: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("incarnation: commit unlock tx: %w", err)
	}
	return &UnlockResult{PreviousStatus: previous, HistoryID: historyID}, nil
}

// rerunCreateScenarioLabel — значение `state_history.scenario` для unlock-части
// rerun-create. По конвенции unlock-перехода (state_history требует non-null
// scenario): фиксируем сам факт снятия error_locked под этой меткой, отдельной
// от unlockScenarioLabel — rerun снимает блок И перезапускает create, его след
// в истории отличается от обычного ручного unlock.
const rerunCreateScenarioLabel = "rerun-create"

// createScenarioLabel — имя bootstrap-сценария `create`. Должна совпадать с
// scenario.CreateScenarioName (пакет incarnation НЕ импортирует scenario — он
// нижний слой; держим локальную константу). [UnlockForRerun] сужает scope
// rerun-create строго до повторного `create`: последний прогон, упавший в
// error_locked, обязан быть именно create-сценарием — run.go::abort пишет в
// state_history.scenario имя упавшего сценария, по нему и проверяем.
const createScenarioLabel = "create"

// UnlockForRerun — unlock-часть rerun-create (architecture.md → «Атомарность и
// error_locked»). Атомарно снимает error_locked и переводит incarnation
// error_locked → applying МИНУЯ ready: окна, в котором конкурентный прогон
// проскочил бы в освободившийся ready, не возникает (переход под одним
// FOR UPDATE). State НЕ трогается — last known-good сохраняется (snapshot в
// state_history, state_before == state_after, симметрично [Unlock]).
//
// Допуск ЖЁСТКО из error_locked: migration_failed / destroy_failed / ready /
// applying / destroying → [ErrIncarnationNotErrorLocked] (для них — обычный
// unlock + ручной run; rerun = специализированный rerun bootstrap-а `create`).
//
// Scope=create: дополнительно сужает допуск — последний прогон, упавший в
// error_locked (последний snapshot state_history), обязан быть `create`. Иной
// сценарий (например add_user) → [ErrRerunScenarioNotCreate]: rerun-create
// перезапускает строго bootstrap, не произвольную упавшую операцию.
//
// Caller (handler / MCP-tool) ПОСЛЕ успешного коммита запускает scenario
// `create` через runner.Start с тем же applyID, что передан сюда: статус уже
// applying, lockRun стартующего прогона лочит ту же строку и видит applying как
// валидный стартовый статус (как авто-create в create-handler-е). Передача
// applyID сюда нужна для записи его в state_history.apply_id — снимок unlock-
// перехода коррелирует с запускаемым прогоном.
//
// Атомарность: одна транзакция SELECT … FOR UPDATE → gate error_locked →
// INSERT state_history → UPDATE status=applying → commit. FOR UPDATE
// сериализует rerun относительно конкурентного scenario-runner-а (его lockRun
// лочит ту же строку).
//
// Возврат:
//   - [ErrIncarnationNotFound]       — name не существует (404).
//   - [ErrIncarnationNotErrorLocked] — статус не error_locked (409).
//   - [ErrRerunScenarioNotCreate]    — последний упавший сценарий не `create` (409).
//
// reason пишется в audit-payload caller-ом (state_history-схема MVP не несёт
// metadata-колонки); previous_status возвращается в [UnlockResult].
func UnlockForRerun(ctx context.Context, pool TxBeginner, name, reason, rerunByAID, historyID, applyID string) (*UnlockResult, error) {
	if !ValidName(name) {
		return nil, fmt.Errorf("incarnation: invalid name %q", name)
	}
	if reason == "" {
		return nil, fmt.Errorf("incarnation: rerun-create reason is empty")
	}
	if historyID == "" {
		return nil, fmt.Errorf("incarnation: empty history_id")
	}
	if applyID == "" {
		return nil, fmt.Errorf("incarnation: empty apply_id")
	}

	tx, err := pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return nil, fmt.Errorf("incarnation: begin rerun-create tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	const selectForUpdateSQL = `
SELECT state, status
FROM incarnation
WHERE name = $1
FOR UPDATE
`
	var (
		stateBytes []byte
		statusStr  string
	)
	if err := tx.QueryRow(ctx, selectForUpdateSQL, name).Scan(&stateBytes, &statusStr); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrIncarnationNotFound
		}
		return nil, fmt.Errorf("incarnation: rerun-create select: %w", err)
	}
	previous := Status(statusStr)
	if previous != StatusErrorLocked {
		return nil, ErrIncarnationNotErrorLocked
	}

	// Scope=create: rerun-create перезапускает строго bootstrap `create`. Сценарий,
	// упавший в error_locked, читаем из последнего snapshot-а state_history (run.go::
	// abort→lockIncarnation→UpdateStateFromRun пишет туда scenario имени упавшего
	// сценария). Если это НЕ create (например add_user) — отказ: rerun исполнил бы
	// create вместо фактически провалившейся операции. Та же FOR UPDATE-tx: чтение
	// сериализовано относительно конкурентного scenario-runner-а.
	const lastScenarioSQL = `
SELECT scenario
FROM state_history
WHERE incarnation_name = $1
ORDER BY history_id DESC
LIMIT 1
`
	var lastScenario string
	if err := tx.QueryRow(ctx, lastScenarioSQL, name).Scan(&lastScenario); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			// error_locked без единого snapshot-а — недостижимо штатно (lockIncarnation
			// всегда пишет state_history при провале). Fail-closed: не считаем
			// отсутствие следа за create.
			return nil, ErrRerunScenarioNotCreate
		}
		return nil, fmt.Errorf("incarnation: rerun-create last scenario probe: %w", err)
	}
	if lastScenario != createScenarioLabel {
		return nil, ErrRerunScenarioNotCreate
	}

	var changedByArg any
	if rerunByAID != "" {
		changedByArg = rerunByAID
	}

	// state_before == state_after: rerun не меняет state (last known-good).
	// apply_id = $6: снимок unlock-перехода коррелирует с запускаемым прогоном
	// (тот же applyID использует runner.Start у caller-а).
	const historyInsertSQL = `
INSERT INTO state_history (
    history_id, incarnation_name, scenario, state_before, state_after,
    changed_by_aid, apply_id
) VALUES ($1, $2, $3, $4, $4, $5, $6)
`
	if _, err := tx.Exec(ctx, historyInsertSQL,
		historyID, name, rerunCreateScenarioLabel, stateBytes, changedByArg, applyID,
	); err != nil {
		return nil, fmt.Errorf("incarnation: insert rerun-create state_history: %w", err)
	}

	// status → applying (МИНУЯ ready), status_details сбрасываются (блок снят).
	// Конкурентный прогон не проскочит: ready никогда не материализуется, а
	// FOR UPDATE держит строку до commit-а.
	const updateSQL = `
UPDATE incarnation
SET status = $2, status_details = NULL, updated_at = NOW()
WHERE name = $1
`
	if _, err := tx.Exec(ctx, updateSQL, name, string(StatusApplying)); err != nil {
		return nil, fmt.Errorf("incarnation: rerun-create update: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("incarnation: commit rerun-create tx: %w", err)
	}
	return &UnlockResult{PreviousStatus: previous, HistoryID: historyID}, nil
}

// migrationScenarioLabel — значение `state_history.scenario` для шага
// state_schema-миграции (docs/migrations.md §Атомарность: scenario="migration").
// Миграция — не прогон scenario через Soul, но state_history требует non-null
// scenario; фиксируем под этой меткой каждый шаг цепочки.
const migrationScenarioLabel = "migration"

// UpgradeInput — вход [UpgradeStateSchema]. Caller (Slice 2) резолвит
// TargetSchemaVer из service.yml целевого снапшота, собирает Chain через
// [artifact.ServiceLoader.LoadMigrationChain] и генерит ApplyID (ULID одной
// upgrade-операции, общий для всех шагов цепочки).
type UpgradeInput struct {
	Name             string
	TargetServiceVer string                 // git-ref целевой версии сервиса (ADR-007)
	TargetSchemaVer  int                    // state_schema_version из service.yml снапшота
	Chain            statemigrate.Chain     // цепочка current→target (пустая = no-op ref-bump)
	Evaluator        statemigrate.Evaluator // migration-CEL ([statemigrate.NewEvaluator])
	ApplyID          string                 // ULID upgrade-операции (общий на цепочку)
	ChangedByAID     *string                // Архонт-инициатор (nil — без identity)
}

// UpgradeResult — итог [UpgradeStateSchema]: схема до/после и число
// применённых шагов миграции (0 для no-op ref-bump).
type UpgradeResult struct {
	FromSchemaVer int
	ToSchemaVer   int
	Steps         int
}

// UpgradeStateSchema атомарно применяет state_schema-миграцию incarnation
// (ADR-019, docs/migrations.md §Атомарность). Тот же транзакционный паттерн,
// что [Unlock] / scenario.lockRun: одна tx FOR UPDATE → gate статуса → sanity
// против chain → in-memory [statemigrate.Apply] → per-step INSERT state_history
// → UPDATE incarnation → commit.
//
// Апгрейд разрешён ТОЛЬКО из ready:
//   - applying        → [ErrIncarnationBusy] (идёт прогон);
//   - error_locked    → [ErrIncarnationLocked] (нужен unlock);
//   - migration_failed → [ErrIncarnationLocked] (нужен unlock).
//
// Sanity против chain (защита от гонки resolve↔FOR UPDATE):
//   - текущая schema-версия должна равняться chain[0].FromVersion;
//   - TargetSchemaVer должен равняться chain[last].ToVersion;
//     иначе [ErrSchemaVersionMismatch] (кто-то проапгрейдил между resolve и
//     блокировкой строки).
//   - TargetSchemaVer < текущей → [ErrDowngradeUnsupported] (forward-only).
//
// No-op (пустой Chain — сменился ref, но schema_version тот же): [Apply]
// вернёт FinalState = копию state и пустые Steps; всё равно пишется одна
// zero-diff state_history-запись (симметрично unlock) и UPDATE service_version.
//
// При ошибке [Apply] / любого write tx откатывается; затем ОТДЕЛЬНОЙ
// background-tx incarnation помечается status=migration_failed с masked
// status_details (паттерн scenario.lockIncarnation; фейл миграции →
// migration_failed, НЕ error_locked, ADR-019). Ошибка такой пометки
// оборачивается в возврат, но первичная причина сохраняется через %w.
//
// Возврат [ErrIncarnationNotFound], если строки нет.
func UpgradeStateSchema(ctx context.Context, pool TxBeginner, in UpgradeInput) (*UpgradeResult, error) {
	if !ValidName(in.Name) {
		return nil, fmt.Errorf("incarnation: invalid name %q", in.Name)
	}
	if in.TargetServiceVer == "" {
		return nil, fmt.Errorf("incarnation: empty target service_version")
	}
	if in.TargetSchemaVer <= 0 {
		return nil, fmt.Errorf("incarnation: target state_schema_version must be > 0, got %d", in.TargetSchemaVer)
	}
	if in.Evaluator == nil {
		return nil, fmt.Errorf("incarnation: nil evaluator")
	}
	if in.ApplyID == "" {
		return nil, fmt.Errorf("incarnation: empty apply_id")
	}

	res, err := upgradeTx(ctx, pool, in)
	if err == nil {
		return res, nil
	}
	// Sentinel-отказы (gate / sanity / not-found) — НЕ migration_failed:
	// state не тронут, incarnation остаётся в исходном статусе.
	if isUpgradeRejection(err) {
		return nil, err
	}
	// Фейл Apply / write внутри tx (rollback уже сделан) → пометить
	// migration_failed отдельной background-tx.
	markErr := markMigrationFailed(pool, in, err)
	if markErr != nil {
		return nil, fmt.Errorf("incarnation: upgrade failed (%w); пометка migration_failed провалена: %v", err, markErr)
	}
	return nil, err
}

// isUpgradeRejection — true для sentinel-отказов, которые НЕ переводят
// incarnation в migration_failed (state не изменён, статус сохранён).
func isUpgradeRejection(err error) bool {
	return errors.Is(err, ErrIncarnationNotFound) ||
		errors.Is(err, ErrIncarnationBusy) ||
		errors.Is(err, ErrIncarnationLocked) ||
		errors.Is(err, ErrDowngradeUnsupported) ||
		errors.Is(err, ErrSchemaVersionMismatch)
}

// upgradeTx выполняет всю upgrade-логику в одной PG-транзакции. Выделено из
// [UpgradeStateSchema], чтобы failure-handling (migration_failed) жил снаружи,
// после гарантированного rollback (defer Rollback в этой функции).
func upgradeTx(ctx context.Context, pool TxBeginner, in UpgradeInput) (*UpgradeResult, error) {
	tx, err := pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return nil, fmt.Errorf("incarnation: begin upgrade tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	const selectForUpdateSQL = `
SELECT state, state_schema_version, status
FROM incarnation
WHERE name = $1
FOR UPDATE
`
	var (
		stateBytes []byte
		currentVer int
		statusStr  string
	)
	if err := tx.QueryRow(ctx, selectForUpdateSQL, in.Name).Scan(&stateBytes, &currentVer, &statusStr); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrIncarnationNotFound
		}
		return nil, fmt.Errorf("incarnation: upgrade select: %w", err)
	}

	switch Status(statusStr) {
	case StatusReady, StatusDrift:
		// ready — штатный путь; drift (ADR-031, Scry) — информационный статус,
		// upgrade его НЕ блокирует (как и обычный apply): новая state_schema
		// может сама являться путём remediation расхождения. По окончании
		// upgrade-tx статус уйдёт в ready через тот же UPDATE.
	case StatusApplying:
		return nil, ErrIncarnationBusy
	case StatusErrorLocked, StatusMigrationFailed:
		return nil, ErrIncarnationLocked
	default:
		return nil, fmt.Errorf("incarnation: upgrade from unknown status %q", statusStr)
	}

	if in.TargetSchemaVer < currentVer {
		return nil, ErrDowngradeUnsupported
	}

	// Sanity против chain (только для непустой цепочки): защита от гонки
	// resolve(service.yml снапшота) ↔ FOR UPDATE — кто-то мог проапгрейдить
	// строку между этими точками.
	if len(in.Chain) > 0 {
		if in.Chain[0].FromVersion != currentVer || in.Chain[len(in.Chain)-1].ToVersion != in.TargetSchemaVer {
			return nil, ErrSchemaVersionMismatch
		}
	} else if in.TargetSchemaVer != currentVer {
		// Пустая цепочка обязана быть ref-bump-ом без смены schema-версии.
		return nil, ErrSchemaVersionMismatch
	}

	currentState, err := unmarshalJSONB(stateBytes)
	if err != nil {
		return nil, fmt.Errorf("incarnation: upgrade unmarshal state: %w", err)
	}

	applyRes, err := statemigrate.Apply(ctx, currentState, in.Chain, in.Evaluator)
	if err != nil {
		return nil, fmt.Errorf("incarnation: миграция %q: %w", in.Name, err)
	}

	if err := writeMigrationHistory(ctx, tx, in, currentState, applyRes); err != nil {
		return nil, err
	}

	finalBytes, err := marshalJSONB(applyRes.FinalState)
	if err != nil {
		return nil, fmt.Errorf("incarnation: marshal migrated state: %w", err)
	}

	const updateSQL = `
UPDATE incarnation
SET state                = $2,
    state_schema_version = $3,
    service_version      = $4,
    status               = 'ready',
    status_details       = NULL,
    updated_at           = NOW()
WHERE name = $1
`
	if _, err := tx.Exec(ctx, updateSQL, in.Name, finalBytes, in.TargetSchemaVer, in.TargetServiceVer); err != nil {
		return nil, fmt.Errorf("incarnation: upgrade update: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("incarnation: commit upgrade tx: %w", err)
	}

	return &UpgradeResult{
		FromSchemaVer: currentVer,
		ToSchemaVer:   in.TargetSchemaVer,
		Steps:         len(applyRes.Steps),
	}, nil
}

// writeMigrationHistory пишет per-step snapshot цепочки в state_history. Один
// общий ApplyID на upgrade, разные history_id (ULID) на шаг. Для no-op
// (пустая цепочка) пишется одна zero-diff запись (state_before == state_after),
// симметрично unlock — фиксируем сам факт ref-bump-а.
func writeMigrationHistory(ctx context.Context, tx ExecQueryRower, in UpgradeInput, before map[string]any, res statemigrate.Result) error {
	const historyInsertSQL = `
INSERT INTO state_history (
    history_id, incarnation_name, scenario, state_before, state_after,
    changed_by_aid, apply_id
) VALUES ($1, $2, $3, $4, $5, $6, $7)
`
	var changedByArg any
	if in.ChangedByAID != nil {
		changedByArg = *in.ChangedByAID
	}

	if len(res.Steps) == 0 {
		// No-op ref-bump: zero-diff snapshot (state неизменно).
		stateBytes, err := marshalJSONB(before)
		if err != nil {
			return fmt.Errorf("incarnation: marshal state (no-op history): %w", err)
		}
		if _, err := tx.Exec(ctx, historyInsertSQL,
			audit.NewULID(), in.Name, migrationScenarioLabel, stateBytes, stateBytes, changedByArg, in.ApplyID,
		); err != nil {
			return fmt.Errorf("incarnation: insert migration state_history (no-op): %w", err)
		}
		return nil
	}

	for i := range res.Steps {
		beforeBytes, err := marshalJSONB(res.Steps[i].StateBefore)
		if err != nil {
			return fmt.Errorf("incarnation: marshal step state_before: %w", err)
		}
		afterBytes, err := marshalJSONB(res.Steps[i].StateAfter)
		if err != nil {
			return fmt.Errorf("incarnation: marshal step state_after: %w", err)
		}
		if _, err := tx.Exec(ctx, historyInsertSQL,
			audit.NewULID(), in.Name, migrationScenarioLabel, beforeBytes, afterBytes, changedByArg, in.ApplyID,
		); err != nil {
			return fmt.Errorf("incarnation: insert migration state_history (step %d): %w", i, err)
		}
	}
	return nil
}

// markMigrationFailed помечает incarnation status=migration_failed после
// провала миграции. Отдельная background-tx (паттерн scenario.lockIncarnation):
// исходный ctx мог быть отменён, но зафиксировать провал надо в любом случае.
// status_details маскируется через [audit.MaskSecrets] (migration-CEL без
// vault, но cause может транзитом нести vault-ref из старого state — defense
// in depth, status_details читается наружу без маскинга).
func markMigrationFailed(pool TxBeginner, in UpgradeInput, cause error) error {
	wctx := context.Background()
	details := audit.MaskSecrets(map[string]any{
		"reason":   "migration_failed",
		"apply_id": in.ApplyID,
		"error":    cause.Error(),
	})
	detailsBytes, err := json.Marshal(details)
	if err != nil {
		return fmt.Errorf("incarnation: marshal migration_failed details: %w", err)
	}

	tx, err := pool.BeginTx(wctx, pgx.TxOptions{})
	if err != nil {
		return fmt.Errorf("incarnation: begin migration_failed tx: %w", err)
	}
	defer func() { _ = tx.Rollback(wctx) }()

	const updateSQL = `
UPDATE incarnation
SET status = 'migration_failed', status_details = $2, updated_at = NOW()
WHERE name = $1
`
	if _, err := tx.Exec(wctx, updateSQL, in.Name, detailsBytes); err != nil {
		return fmt.Errorf("incarnation: migration_failed update: %w", err)
	}
	if err := tx.Commit(wctx); err != nil {
		return fmt.Errorf("incarnation: commit migration_failed tx: %w", err)
	}
	return nil
}

// HistoryFilter — фильтры для [HistorySelectByName]. Пустые поля — «не
// фильтровать». ApplyID — ULID конкретного прогона; обычно матчит
// 0 или 1 row (один state_history-snapshot на apply), но контракт не
// запрещает несколько при будущем расширении.
//
// IncludeArchived — флаг включения soft-deleted-снимков (ADR-Q19 retention,
// колонка `state_history.archived_at`). По умолчанию false: реестр истории
// возвращает только активные (`archived_at IS NULL`) снимки — типовой
// сценарий Operator API / MCP. При true возвращаются все записи (включая
// помеченные правилом Reaper `archive_state_history`) — нужно для разбора
// «куда делся snapshot N дней назад».
type HistoryFilter struct {
	ApplyID         string
	IncludeArchived bool
}

// HistorySelectByName возвращает страницу записей `state_history`
// конкретной incarnation в обратном хронологическом порядке + общее
// количество (без offset/limit). При непустом `filter.ApplyID`
// результат дополнительно фильтруется по `apply_id`.
//
// По умолчанию (filter.IncludeArchived = false) возвращаются только
// активные снимки (`archived_at IS NULL`, ADR-Q19 retention). Total
// тоже считается по активному множеству — пагинация Operator API не
// «прыгает» через soft-deleted-дыры.
//
// Возврат ([], 0, nil) для несуществующей incarnation — отдельным запросом
// проверять existence НЕ обязательно: caller (handler) пусть сначала
// делает [SelectByName], чтобы вернуть 404, либо признаёт пустую историю
// валидной для существующей incarnation.
func HistorySelectByName(ctx context.Context, db ExecQueryRower, name string, filter HistoryFilter, offset, limit int) ([]*HistoryEntry, int, error) {
	if offset < 0 {
		return nil, 0, fmt.Errorf("incarnation: history offset must be >= 0, got %d", offset)
	}
	if limit < 1 {
		return nil, 0, fmt.Errorf("incarnation: history limit must be >= 1, got %d", limit)
	}

	args := []any{name}
	where := "WHERE incarnation_name = $1"
	if !filter.IncludeArchived {
		where += " AND archived_at IS NULL"
	}
	if filter.ApplyID != "" {
		args = append(args, filter.ApplyID)
		where += fmt.Sprintf(" AND apply_id = $%d", len(args))
	}

	countSQL := "SELECT COUNT(*) FROM state_history " + where
	var total int
	if err := db.QueryRow(ctx, countSQL, args...).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("incarnation: history count: %w", err)
	}

	listSQL := `SELECT history_id, scenario, state_before, state_after,
       changed_by_aid, apply_id, at
FROM state_history
` + where +
		fmt.Sprintf(`
ORDER BY at DESC, history_id ASC
OFFSET $%d LIMIT $%d`, len(args)+1, len(args)+2)
	listArgs := append(append([]any{}, args...), offset, limit)

	rows, err := db.Query(ctx, listSQL, listArgs...)
	if err != nil {
		return nil, 0, fmt.Errorf("incarnation: history list query: %w", err)
	}
	defer rows.Close()

	var out []*HistoryEntry
	for rows.Next() {
		entry, err := scanHistoryEntry(rows)
		if err != nil {
			return nil, 0, err
		}
		out = append(out, entry)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, fmt.Errorf("incarnation: history list iter: %w", err)
	}
	return out, total, nil
}

func scanHistoryEntry(row pgx.Row) (*HistoryEntry, error) {
	var (
		entry            HistoryEntry
		stateBeforeBytes []byte
		stateAfterBytes  []byte
		changedByAID     *string
	)
	err := row.Scan(
		&entry.HistoryID,
		&entry.Scenario,
		&stateBeforeBytes,
		&stateAfterBytes,
		&changedByAID,
		&entry.ApplyID,
		&entry.At,
	)
	if err != nil {
		return nil, fmt.Errorf("incarnation: history scan: %w", err)
	}
	entry.ChangedByAID = changedByAID
	if entry.StateBefore, err = unmarshalJSONB(stateBeforeBytes); err != nil {
		return nil, fmt.Errorf("incarnation: history unmarshal state_before: %w", err)
	}
	if entry.StateAfter, err = unmarshalJSONB(stateAfterBytes); err != nil {
		return nil, fmt.Errorf("incarnation: history unmarshal state_after: %w", err)
	}
	return &entry, nil
}

// ValidStatus — closed enum проверка status. Дублирует CHECK
// incarnation_status_valid из миграции (005 + 031 + 036 + 047), чтобы отказывать
// в Go ещё до round-trip-а. Экспортирована для list-filter в handler-слое.
func ValidStatus(s Status) bool {
	switch s {
	case StatusReady, StatusApplying, StatusErrorLocked, StatusMigrationFailed,
		StatusDestroying, StatusDestroyFailed, StatusDrift:
		return true
	}
	return false
}

// marshalJSONB сериализует map в bytes для JSONB-колонки. nil → `{}`,
// симметрично shared/audit и operator.marshalMetadata.
func marshalJSONB(m map[string]any) ([]byte, error) {
	if m == nil {
		return []byte("{}"), nil
	}
	return json.Marshal(m)
}

// unmarshalJSONB парсит JSONB-bytes в map. Пустые байты / `null` → nil-map.
func unmarshalJSONB(b []byte) (map[string]any, error) {
	if len(b) == 0 {
		return nil, nil
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, err
	}
	return m, nil
}
