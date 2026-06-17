package rbac

import (
	"context"
	"errors"
	"fmt"
	"sort"

	"github.com/jackc/pgx/v5/pgconn"
)

// ErrSynodNotFound — группа с указанным name отсутствует в synods.
// Возвращается при SELECT … FOR UPDATE / DELETE, не нашедшем строку.
// Transport маппит «нет группы» в 404 (симметрия [ErrRoleNotFound]).
var ErrSynodNotFound = errors.New("rbac: synod not found")

// ErrSynodAlreadyExists — UNIQUE-violation (`23505`) на PK synods.name при
// CreateSynod. Transport маппит в 409 (симметрия [ErrRoleAlreadyExists]).
var ErrSynodAlreadyExists = errors.New("rbac: synod already exists")

// ErrInvalidSynodName — name группы не проходит [reSynodName]. Validation-
// error sentinel (transport маппит в 422, симметрия [ErrInvalidRoleName]).
var ErrInvalidSynodName = errors.New("rbac: invalid synod name")

// ErrSynodBuiltin — попытка synod.delete над группой с builtin=true. Builtin-
// группа не удаляется (ADR-049(g), симметрия [ErrRoleBuiltin]). Transport
// маппит в 409.
var ErrSynodBuiltin = errors.New("rbac: synod is builtin (delete forbidden)")

// ErrSynodOperatorNotFound — membership-строка (synod_name, aid) отсутствует
// в synod_operators при RemoveOperator. Transport маппит в 404.
var ErrSynodOperatorNotFound = errors.New("rbac: synod-operator membership not found")

// ErrSynodRoleNotFound — bundle-строка (synod_name, role_name) отсутствует
// в synod_roles при RevokeRole. Transport маппит в 404.
var ErrSynodRoleNotFound = errors.New("rbac: synod-role bundle entry not found")

// SynodDescriptionMaxLen — потолок длины description группы (cap против
// раздувания UI/audit-payload). Единый источник правды для ОБОИХ write-path:
// HTTP-handler (PATCH /v1/synods/{name}) и MCP-tool (keeper.synod.update)
// ссылаются на него, чтобы превышение давало одинаковый validation-failed
// (422). OpenAPI-схема несёт тот же maxLength — двойная защита (спека +
// transport). Превышение → [problem.TypeValidationFailed] / mcpCodeValidationFailed.
const SynodDescriptionMaxLen = 1024

// reSynodName совпадает с SQL CHECK synods_name_format (миграция 069) и с
// [reRoleName] — kebab-case. Дублируется в Go для валидации до round-trip-а.
// Используется единый [reRoleName] (формат идентичен), отдельная regexp не
// заводится, чтобы не плодить два источника одного паттерна.

const (
	// insertSynodSQL — INSERT строки synods. builtin всегда false для групп,
	// созданных через API (builtin=true ставит только seed-миграция, если
	// появится). created_at из DEFAULT NOW().
	insertSynodSQL = `
INSERT INTO synods (name, description, builtin, created_by_aid)
VALUES ($1, $2, false, $3)
`

	// updateSynodDescriptionSQL — правка ТОЛЬКО description группы (ADR-049
	// amend). name (PK) immutable, поэтому в WHERE и не меняется. builtin/roles/
	// membership не трогаются — description косметический, прав не выдаёт.
	updateSynodDescriptionSQL = `UPDATE synods SET description = $2 WHERE name = $1`

	// deleteSynodSQL — DELETE строки synods. CASCADE на synod_operators /
	// synod_roles сносит membership и bundle одной операцией.
	deleteSynodSQL = `DELETE FROM synods WHERE name = $1`

	// lockSynodForUpdateSQL — SELECT builtin строки группы под row-lock
	// (FOR UPDATE) до конца tx. Первый шаг мутаций над группой: сериализует
	// конкурентные операции и читает builtin для builtin-границы.
	lockSynodForUpdateSQL = `SELECT builtin FROM synods WHERE name = $1 FOR UPDATE`

	// insertSynodOperatorSQL — INSERT membership-строки (synod_name, aid).
	// ON CONFLICT DO NOTHING делает grant идемпотентным (повторное добавление
	// архона в группу — no-op, симметрия insertRoleOperatorSQL).
	insertSynodOperatorSQL = `
INSERT INTO synod_operators (synod_name, aid, added_by_aid)
VALUES ($1, $2, $3)
ON CONFLICT (synod_name, aid) DO NOTHING
`

	// deleteSynodOperatorSQL — DELETE одной membership-строки (synod, aid).
	deleteSynodOperatorSQL = `DELETE FROM synod_operators WHERE synod_name = $1 AND aid = $2`

	// insertSynodRoleSQL — INSERT bundle-строки (synod_name, role_name).
	// ON CONFLICT DO NOTHING — повторный grant роли в группу no-op.
	insertSynodRoleSQL = `
INSERT INTO synod_roles (synod_name, role_name, granted_by_aid)
VALUES ($1, $2, $3)
ON CONFLICT (synod_name, role_name) DO NOTHING
`

	// deleteSynodRoleSQL — DELETE одной bundle-строки (synod, role).
	deleteSynodRoleSQL = `DELETE FROM synod_roles WHERE synod_name = $1 AND role_name = $2`

	// lockSynodRoleForUpdateSQL — row-lock bundle-строки (synod, role) до
	// конца tx. Используется RevokeRole-путём перед self-lockout-проверкой.
	lockSynodRoleForUpdateSQL = `SELECT 1 FROM synod_roles WHERE synod_name = $1 AND role_name = $2 FOR UPDATE`

	// lockSynodOperatorForUpdateSQL — row-lock membership-строки (synod, aid)
	// до конца tx. Используется RemoveOperator-путём перед self-lockout.
	lockSynodOperatorForUpdateSQL = `SELECT 1 FROM synod_operators WHERE synod_name = $1 AND aid = $2 FOR UPDATE`
)

// CreateSynod создаёт группу. builtin-граница и self-lockout к create неприменимы
// (создание пустой группы прав не выдаёт). db должен быть tx/pool.
//
// Ошибки:
//   - [ErrSynodAlreadyExists] на UNIQUE-violation synods.name (23505).
//   - [ErrInvalidSynodName] на битом name (до round-trip-а).
//   - wrapped FK-violation на created_by_aid (несуществующий AID).
func CreateSynod(ctx context.Context, db ExecQueryRower, name, description string, createdByAID *string) error {
	if !reRoleName.MatchString(name) {
		return fmt.Errorf("%w: %q must match %s", ErrInvalidSynodName, name, reRoleName.String())
	}
	var createdBy any
	if createdByAID != nil {
		createdBy = *createdByAID
	}
	if _, err := db.Exec(ctx, insertSynodSQL, name, description, createdBy); err != nil {
		return mapSynodError(err)
	}
	return nil
}

// DeleteSynod удаляет группу; CASCADE сносит её membership и bundle. builtin-
// граница и self-lockout-проверка — на стороне [Service.DeleteSynod] (здесь
// только DELETE).
func DeleteSynod(ctx context.Context, db ExecQueryRower, name string) error {
	tag, err := db.Exec(ctx, deleteSynodSQL, name)
	if err != nil {
		return fmt.Errorf("rbac: delete synod %q: %w", name, wrapPgErr(err))
	}
	if tag.RowsAffected() == 0 {
		return ErrSynodNotFound
	}
	return nil
}

// lockSynod берёт row-lock на строку группы (FOR UPDATE) и возвращает её
// builtin-флаг. Первый шаг service-мутаций над группой (симметрия [lockRole]).
// tx ОБЯЗАН быть транзакцией. Возврат [ErrSynodNotFound], если строки нет.
func lockSynod(ctx context.Context, tx ExecQueryRower, name string) (builtin bool, err error) {
	rows, err := tx.Query(ctx, lockSynodForUpdateSQL, name)
	if err != nil {
		return false, fmt.Errorf("rbac: lock synod %q: %w", name, wrapPgErr(err))
	}
	defer rows.Close()
	if !rows.Next() {
		if err := rows.Err(); err != nil {
			return false, fmt.Errorf("rbac: lock synod %q iter: %w", name, err)
		}
		return false, ErrSynodNotFound
	}
	if err := rows.Scan(&builtin); err != nil {
		return false, fmt.Errorf("rbac: scan synod %q builtin: %w", name, err)
	}
	return builtin, nil
}

// lockSynodRole берёт row-lock на bundle-строку (synod, role). Возврат
// [ErrSynodRoleNotFound], если пары нет. tx ОБЯЗАН быть tx.
func lockSynodRole(ctx context.Context, tx ExecQueryRower, synodName, roleName string) error {
	rows, err := tx.Query(ctx, lockSynodRoleForUpdateSQL, synodName, roleName)
	if err != nil {
		return fmt.Errorf("rbac: lock synod-role (%s -> %s): %w", synodName, roleName, wrapPgErr(err))
	}
	defer rows.Close()
	if !rows.Next() {
		if err := rows.Err(); err != nil {
			return fmt.Errorf("rbac: lock synod-role iter: %w", err)
		}
		return ErrSynodRoleNotFound
	}
	return nil
}

// lockSynodOperator берёт row-lock на membership-строку (synod, aid). Возврат
// [ErrSynodOperatorNotFound], если пары нет. tx ОБЯЗАН быть tx.
func lockSynodOperator(ctx context.Context, tx ExecQueryRower, synodName, aid string) error {
	rows, err := tx.Query(ctx, lockSynodOperatorForUpdateSQL, synodName, aid)
	if err != nil {
		return fmt.Errorf("rbac: lock synod-operator (%s -> %s): %w", synodName, aid, wrapPgErr(err))
	}
	defer rows.Close()
	if !rows.Next() {
		if err := rows.Err(); err != nil {
			return fmt.Errorf("rbac: lock synod-operator iter: %w", err)
		}
		return ErrSynodOperatorNotFound
	}
	return nil
}

// synodRoles читает роли группы (bundle synod_roles) без lock-а — нужен
// service-у при grant-role/revoke-role, чтобы посчитать `*`/least-privilege.
// Группа уже залочена lockSynod-ом в той же tx.
func synodRoles(ctx context.Context, tx ExecQueryRower, name string) ([]string, error) {
	rows, err := tx.Query(ctx, `SELECT role_name FROM synod_roles WHERE synod_name = $1`, name)
	if err != nil {
		return nil, fmt.Errorf("rbac: read roles of synod %q: %w", name, wrapPgErr(err))
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var r string
		if err := rows.Scan(&r); err != nil {
			return nil, fmt.Errorf("rbac: scan role of synod %q: %w", name, err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("rbac: iter roles of synod %q: %w", name, err)
	}
	return out, nil
}

// mapSynodError маппит pgx-ошибки synod-INSERT-ов в sentinel-ы (паттерн
// [mapRoleError]):
//   - 23505 (UNIQUE) → [ErrSynodAlreadyExists].
//   - 23503 (FK) → wrapped с именем constraint-а (created_by_aid).
//   - прочее → wrapped с SQLSTATE.
func mapSynodError(err error) error {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		switch pgErr.Code {
		case pgErrCodeUniqueViolation:
			return fmt.Errorf("%w (constraint %s): %w", ErrSynodAlreadyExists, pgErr.ConstraintName, err)
		case pgErrCodeForeignKeyViolation:
			return fmt.Errorf("rbac: FK violation on %s: %w", pgErr.ConstraintName, err)
		}
	}
	return fmt.Errorf("rbac: %w", wrapPgErr(err))
}

// mapSynodMemberError маппит pgx-ошибку INSERT-membership-а/bundle-а (add-
// operator / grant-role). Группа service проверяет lock-ом до insert-а, поэтому
// FK-violation тут — несуществующий operator (add-operator) ИЛИ роль
// (grant-role):
//   - 23503 (FK) на aid/added_by_aid → [ErrOperatorNotFound];
//   - 23503 (FK) на role_name → [ErrRoleNotFound] (grant-role над несуществующей ролью);
//   - прочее → wrapped с SQLSTATE.
//
// Различение по имени constraint-а: synod_roles_role_fk → роль, иначе оператор.
func mapSynodMemberError(err error) error {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == pgErrCodeForeignKeyViolation {
		if pgErr.ConstraintName == "synod_roles_role_fk" {
			return fmt.Errorf("%w (constraint %s): %w", ErrRoleNotFound, pgErr.ConstraintName, err)
		}
		return fmt.Errorf("%w (constraint %s): %w", ErrOperatorNotFound, pgErr.ConstraintName, err)
	}
	return fmt.Errorf("rbac: %w", wrapPgErr(err))
}

// SynodView — API-проекция группы для read-эндпоинта synod.list (ADR-049(g)):
// каталожные поля (Description / Builtin) + развёрнутые Roles (имена ролей в
// bundle) и Operators (AID-члены). Симметрия [RoleView]. Roles/Operators
// отсортированы детерминированно (стабильный вывод list-а).
type SynodView struct {
	Name        string
	Description string
	Builtin     bool
	Roles       []string
	Operators   []string
}

const (
	// selectSynodViewsSQL — каталог групп с description/builtin. ORDER BY name —
	// детерминированный порядок list-а (симметрия selectRoleViewsSQL).
	selectSynodViewsSQL = `SELECT name, description, builtin FROM synods ORDER BY name`
)

// LoadSynodViews собирает API-каталог групп тремя SELECT-ами (группы / bundle /
// membership) — без N+1, симметрично [LoadRoleViews]. Сборка «synod → роли/
// операторы» делается в Go по name-ключу. Roles/Operators группы без записей —
// пустой слайс. Висячие строки (роль/AID вне каталога групп) отбрасываются.
func LoadSynodViews(ctx context.Context, db ExecQueryRower) ([]SynodView, error) {
	views, index, err := loadSynodViewRows(ctx, db)
	if err != nil {
		return nil, err
	}
	if err := loadSynodViewRoles(ctx, db, index); err != nil {
		return nil, err
	}
	if err := loadSynodViewOperators(ctx, db, index); err != nil {
		return nil, err
	}
	for i := range views {
		sort.Strings(views[i].Roles)
		sort.Strings(views[i].Operators)
	}
	return views, nil
}

func loadSynodViewRows(ctx context.Context, db ExecQueryRower) ([]SynodView, map[string]*SynodView, error) {
	rows, err := db.Query(ctx, selectSynodViewsSQL)
	if err != nil {
		return nil, nil, fmt.Errorf("rbac: query synod views: %w", wrapPgErr(err))
	}
	defer rows.Close()
	var views []SynodView
	for rows.Next() {
		var v SynodView
		if err := rows.Scan(&v.Name, &v.Description, &v.Builtin); err != nil {
			return nil, nil, fmt.Errorf("rbac: scan synod view: %w", err)
		}
		views = append(views, v)
	}
	if err := rows.Err(); err != nil {
		return nil, nil, fmt.Errorf("rbac: iter synod views: %w", err)
	}
	index := make(map[string]*SynodView, len(views))
	for i := range views {
		index[views[i].Name] = &views[i]
	}
	return views, index, nil
}

func loadSynodViewRoles(ctx context.Context, db ExecQueryRower, index map[string]*SynodView) error {
	rows, err := db.Query(ctx, selectSynodRolesSQL)
	if err != nil {
		return fmt.Errorf("rbac: query synod-view roles: %w", wrapPgErr(err))
	}
	defer rows.Close()
	for rows.Next() {
		var synodName, roleName string
		if err := rows.Scan(&synodName, &roleName); err != nil {
			return fmt.Errorf("rbac: scan synod-view role: %w", err)
		}
		if v, ok := index[synodName]; ok {
			v.Roles = append(v.Roles, roleName)
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("rbac: iter synod-view roles: %w", err)
	}
	return nil
}

func loadSynodViewOperators(ctx context.Context, db ExecQueryRower, index map[string]*SynodView) error {
	rows, err := db.Query(ctx, selectSynodOperatorsSQL)
	if err != nil {
		return fmt.Errorf("rbac: query synod-view operators: %w", wrapPgErr(err))
	}
	defer rows.Close()
	for rows.Next() {
		var synodName, aid string
		if err := rows.Scan(&synodName, &aid); err != nil {
			return fmt.Errorf("rbac: scan synod-view operator: %w", err)
		}
		if v, ok := index[synodName]; ok {
			v.Operators = append(v.Operators, aid)
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("rbac: iter synod-view operators: %w", err)
	}
	return nil
}
