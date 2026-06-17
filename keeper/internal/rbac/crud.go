package rbac

import (
	"context"
	"errors"
	"fmt"
	"regexp"

	"github.com/jackc/pgx/v5/pgconn"
)

// ErrRoleNotFound — роль с указанным name отсутствует в rbac_roles.
// Возвращается DeleteRole / UpdateRolePermissions при 0 affected rows и
// service-ом при SELECT … FOR UPDATE, не нашедшем строку. Sentinel выделен,
// чтобы transport маппил «нет роли» в 404, а не 500.
var ErrRoleNotFound = errors.New("rbac: role not found")

// ErrRoleAlreadyExists — UNIQUE-violation (`23505`) на PK rbac_roles.name
// при CreateRole. Transport маппит в 409 (`role-already-exists`).
var ErrRoleAlreadyExists = errors.New("rbac: role already exists")

// ErrRoleBuiltin — попытка role.delete / role.update над ролью с
// builtin=true (cluster-admin). Builtin-роль не редактируется и не
// удаляется (ADR-028(b)). Transport маппит в 409 / 422.
var ErrRoleBuiltin = errors.New("rbac: role is builtin (delete/update forbidden)")

// ErrRoleOperatorNotFound — membership-строка (role_name, aid) отсутствует
// в rbac_role_operators при RevokeOperator. Transport маппит в 404.
var ErrRoleOperatorNotFound = errors.New("rbac: role-operator membership not found")

// ErrOperatorNotFound — grant-operator над несуществующим AID: FK-violation
// (23503) на rbac_role_operators_aid_fk. Sentinel выделен, чтобы transport
// маппил «нет такого Архонта» в 404, а не 500. Существование роли service
// проверяет lock-ом ДО insert-а ([ErrRoleNotFound]), поэтому FK-violation в
// grant-пути остаётся только на aid (и теоретически на granted_by_aid —
// middleware гарантирует валидного caller-а, но FK защищает; оба ведут к
// этому sentinel-у как «referenced operator not found»).
var ErrOperatorNotFound = errors.New("rbac: operator (AID) not found")

// ErrWouldLockOutCluster — мутация (role.delete / role.update /
// role.revoke-operator) оставила бы кластер без активного Архонта с
// эффективным `*`-permission (self-lockout-инвариант ADR-028(f),
// rbac.md → § Встроенные роли).
//
// ОТДЕЛЬНЫЙ sentinel от [operator.ErrWouldLockOutCluster]: пакеты rbac и
// operator не должны зависеть друг от друга (избегаем import-цикла —
// operator.Service уже тянет RBACSource-поверхность rbac.Holder). Transport-
// слой маппит ОБА sentinel-а в один problem-type `would-lock-out-cluster`
// (409), shared string-а между пакетами нет.
var ErrWouldLockOutCluster = errors.New("rbac: would lock out cluster (no active operator with effective '*' would remain)")

// reRoleName — формат имени роли (rbac.md → § Storage, SQL CHECK
// rbac_roles_name_format). Дублируется в Go для прикладной валидации до
// round-trip-а (better error, нет лишнего обращения к БД на битом имени).
var reRoleName = regexp.MustCompile(`^[a-z][a-z0-9-]*$`)

// RoleNamePattern — публичная строковая форма [reRoleName] для caller-ов
// другого пакета (например, operator.Service на Create-with-roles, чтобы
// валидировать имена до открытия tx). Совпадает с SQL CHECK
// rbac_roles_name_format.
const RoleNamePattern = `^[a-z][a-z0-9-]*$`

// ValidRoleName проверяет имя роли по [reRoleName]. Экспортирована для
// caller-ов вне пакета, делающих пред-валидацию до round-trip-а
// (operator.Service.Create при наличии Roles[]).
func ValidRoleName(name string) bool {
	return reRoleName.MatchString(name)
}

// pgErrCodeUniqueViolation / pgErrCodeForeignKeyViolation — SQLSTATE для
// UNIQUE / FK нарушений. Держим локально (как в operator/crud.go), чтобы не
// тянуть pgerrcode в keeper.
const (
	pgErrCodeUniqueViolation     = "23505"
	pgErrCodeForeignKeyViolation = "23503"
)

const (
	// insertRoleSQL — INSERT строки rbac_roles. builtin всегда false для
	// ролей, созданных через API: builtin=true ставит только seed-миграция
	// 027 (cluster-admin). created_at берётся из DEFAULT NOW(). default_scope
	// nullable (ADR-047 S1): NULL = роль без scope-ограничения (backcompat).
	insertRoleSQL = `
INSERT INTO rbac_roles (name, description, builtin, created_by_aid, default_scope)
VALUES ($1, $2, false, $3, $4)
`

	// updateRoleDefaultScopeSQL — установка/снятие default_scope роли
	// (ADR-047 S1). NULL снимает scope (роль → backcompat-unrestricted для
	// bare-perms). Используется replace-семантикой UpdateRole.
	updateRoleDefaultScopeSQL = `UPDATE rbac_roles SET default_scope = $2 WHERE name = $1`

	// insertRolePermissionSQL — INSERT одной permission-строки роли.
	// ON CONFLICT DO NOTHING делает batch-insert идемпотентным при
	// дублях в наборе (дубль внутри одного role.create — не ошибка БД).
	insertRolePermissionSQL = `
INSERT INTO rbac_role_permissions (role_name, permission)
VALUES ($1, $2)
ON CONFLICT (role_name, permission) DO NOTHING
`

	// deleteRoleSQL — DELETE строки rbac_roles. CASCADE на
	// rbac_role_permissions / rbac_role_operators сносит permissions и
	// membership одной операцией.
	deleteRoleSQL = `DELETE FROM rbac_roles WHERE name = $1`

	// deleteRolePermissionsSQL — DELETE всех permission-строк роли.
	// Первая половина replace-семантики UpdateRolePermissions.
	deleteRolePermissionsSQL = `DELETE FROM rbac_role_permissions WHERE role_name = $1`

	// deleteRoleOperatorSQL — DELETE одной membership-строки (role, aid).
	deleteRoleOperatorSQL = `DELETE FROM rbac_role_operators WHERE role_name = $1 AND aid = $2`

	// lockRoleForUpdateSQL — SELECT builtin строки роли под row-lock
	// (FOR UPDATE) до конца транзакции. Используется service-ом первым
	// шагом всех мутаций (delete/update): сериализует конкурентные операции
	// над одной ролью и читает builtin для builtin-границы.
	lockRoleForUpdateSQL = `SELECT builtin FROM rbac_roles WHERE name = $1 FOR UPDATE`

	// lockRoleOperatorForUpdateSQL — row-lock membership-строки (role, aid)
	// до конца транзакции. Используется RevokeOperator-путём service-а перед
	// self-lockout-проверкой.
	lockRoleOperatorForUpdateSQL = `SELECT 1 FROM rbac_role_operators WHERE role_name = $1 AND aid = $2 FOR UPDATE`

	// directClusterAdminsForUpdateSQL — ПРЯМАЯ ветка self-lockout-ядра.
	//
	// Активные операторы (operators.revoked_at IS NULL) с эффективным `*`
	// через ЛЮБУЮ ПРЯМУЮ роль (rbac_role_operators), под row-lock на три
	// таблицы в одной tx.
	//
	// ПОЧЕМУ источник — БД (FOR UPDATE), а НЕ enforcer.ClusterAdmins():
	//   1. Snapshot enforcer-а (in-memory, ADR-028(d)) обновляется с TTL/pub-sub
	//      задержкой — между чтением снимка и мутацией он устаревает. Решение по
	//      устаревшему снимку = staleness-дыра: можно снять последнего админа,
	//      если снимок ещё «помнит» уже-revoked-нутого второго.
	//   2. FOR UPDATE на ro/rp/o сериализует конкурентные lockout-операции
	//      (R2/R5): две параллельные tx, снимающие `*` разными путями, не могут
	//      обе пройти проверку «останется ≥1 админ» — первая держит lock-и до
	//      COMMIT, вторая ждёт и видит уже-применённое состояние.
	// НЕ унифицировать обратно на enforcer-снимок — это вернёт дыру (1).
	//
	// БЕЗ DISTINCT: PostgreSQL запрещает `FOR UPDATE` с `DISTINCT`
	// (SQLSTATE 0A000). Дедуп AID-ов, держащих `*` через несколько ролей,
	// делается в Go ([dedupAIDs]); это не меняет ни row-lock, ни множество.
	//
	// ИНВАРИАНТ lock-порядка: self-lockout-ядро берёт lock-и ДВУМЯ запросами в
	// фиксированном порядке — СНАЧАЛА эта прямая ветка (ro,rp,o), ПОТОМ Synod-
	// ветка ([synodClusterAdminsForUpdateSQL], so,sr,rp,o). Порядок ОДИНАКОВ во
	// всех call-site-ах (operator.Revoke + role-мутации DeleteRole/
	// UpdateRolePermissions/RevokeOperator + их exclude-варианты в service.go) —
	// иначе разный порядок lock-ов = deadlock между конкурентными lockout-
	// операциями. Расщепление на два SELECT-а (вместо одного UNION) ВЫНУЖДЕННОЕ:
	// PostgreSQL запрещает `FOR UPDATE` с UNION (SQLSTATE 0A000) — Synod-разворот
	// (ADR-049(f)) не сложить в один locking-запрос с прямым.
	directClusterAdminsForUpdateSQL = `
SELECT ro.aid
FROM rbac_role_operators ro
JOIN rbac_role_permissions rp ON rp.role_name = ro.role_name
JOIN operators o ON o.aid = ro.aid
WHERE rp.permission = '*' AND o.revoked_at IS NULL
FOR UPDATE OF ro, rp, o
`

	// synodClusterAdminsForUpdateSQL — SYNOD-ветка self-lockout-ядра (ADR-049(f)).
	//
	// Активные операторы с эффективным `*`, пришедшим через ЛЮБОЙ Synod
	// (synod_operators ⋈ synod_roles → роль с `*`), под row-lock на четыре
	// таблицы. Без этой ветки `*` через группу невидим self-lockout-у: снятие
	// прямого `*` залочило бы кластер, даже если последний админ держит `*`
	// через Synod (и наоборот — нельзя осиротить админа, чей `*` только в группе).
	//
	// Берётся ВТОРЫМ запросом после [directClusterAdminsForUpdateSQL] (см. там
	// инвариант lock-порядка). Дедуп объединённого множества — в Go ([dedupAIDs]):
	// AID, держащий `*` и напрямую, и через Synod, должен учитываться один раз.
	synodClusterAdminsForUpdateSQL = `
SELECT so.aid
FROM synod_operators so
JOIN synod_roles sr ON sr.synod_name = so.synod_name
JOIN rbac_role_permissions rp ON rp.role_name = sr.role_name
JOIN operators o ON o.aid = so.aid
WHERE rp.permission = '*' AND o.revoked_at IS NULL
FOR UPDATE OF so, sr, rp, o
`
)

// CreateRole создаёт роль и её permissions в одной транзакции:
// INSERT rbac_roles + batch INSERT rbac_role_permissions.
//
// Пред-валидация (defense-in-depth; основная — в [Service.CreateRole]):
//   - name по [reRoleName] (формат рор-CHECK rbac_roles_name_format);
//   - каждый permission через [ParsePermission] (БД хранит RAW-строку, не
//     валидирует — парсер ловит мусор до записи).
//
// db должен быть транзакцией (`pgx.Tx`): при ошибке batch-insert-а откат
// убирает уже вставленную rbac_roles-строку. Вызов на pool-е оставит
// частично созданную роль при сбое — caller обязан передать tx.
//
// Ошибки:
//   - [ErrRoleAlreadyExists] на UNIQUE-violation rbac_roles.name (23505).
//   - wrapped FK-violation на created_by_aid (несуществующий AID).
//   - fmt.Errorf на битом name / permission (до round-trip-а).
func CreateRole(ctx context.Context, db ExecQueryRower, name, description string, permissions []string, createdByAID *string, defaultScope *string) error {
	if !reRoleName.MatchString(name) {
		return fmt.Errorf("rbac: invalid role name %q (must match %s)", name, reRoleName.String())
	}
	for _, raw := range permissions {
		if _, err := ParsePermission(raw); err != nil {
			return fmt.Errorf("rbac: invalid permission %q: %w", raw, err)
		}
	}
	if defaultScope != nil {
		if _, err := ParseDefaultScope(*defaultScope); err != nil {
			return fmt.Errorf("rbac: invalid default_scope %q: %w", *defaultScope, err)
		}
	}

	var createdBy any
	if createdByAID != nil {
		createdBy = *createdByAID
	}
	if _, err := db.Exec(ctx, insertRoleSQL, name, description, createdBy, defaultScopeArg(defaultScope)); err != nil {
		return mapRoleError(err)
	}
	for _, perm := range permissions {
		if _, err := db.Exec(ctx, insertRolePermissionSQL, name, perm); err != nil {
			return mapRoleError(err)
		}
	}
	return nil
}

// DeleteRole удаляет роль; CASCADE сносит её permissions и membership.
// builtin-граница и self-lockout-проверка — на стороне [Service.DeleteRole]
// (здесь только DELETE).
//
// Ошибки:
//   - [ErrRoleNotFound] при 0 affected rows.
//   - wrapped pgx-ошибка на транспортном сбое.
func DeleteRole(ctx context.Context, db ExecQueryRower, name string) error {
	tag, err := db.Exec(ctx, deleteRoleSQL, name)
	if err != nil {
		return fmt.Errorf("rbac: delete role %q: %w", name, wrapPgErr(err))
	}
	if tag.RowsAffected() == 0 {
		return ErrRoleNotFound
	}
	return nil
}

// UpdateRolePermissions заменяет набор permissions роли (replace-семантика):
// DELETE всех permission-строк роли + batch INSERT нового набора. Должен
// вызываться в транзакции (`pgx.Tx`) — иначе при сбое insert-а роль останется
// с пустым набором permissions.
//
// Роль обязана существовать: проверяется по rows-affected DELETE-а — 0 строк
// при пустом старом наборе неоднозначен, поэтому existence-граница (lock роли)
// делается в [Service.UpdateRolePermissions] до вызова этой функции. Здесь
// возврат [ErrRoleNotFound] недостижим напрямую (роль уже залочена service-ом);
// функция остаётся транспортной.
//
// Пред-валидация permissions — defense-in-depth (как в CreateRole).
func UpdateRolePermissions(ctx context.Context, db ExecQueryRower, name string, permissions []string) error {
	for _, raw := range permissions {
		if _, err := ParsePermission(raw); err != nil {
			return fmt.Errorf("rbac: invalid permission %q: %w", raw, err)
		}
	}
	if _, err := db.Exec(ctx, deleteRolePermissionsSQL, name); err != nil {
		return fmt.Errorf("rbac: clear permissions of role %q: %w", name, wrapPgErr(err))
	}
	for _, perm := range permissions {
		if _, err := db.Exec(ctx, insertRolePermissionSQL, name, perm); err != nil {
			return mapRoleError(err)
		}
	}
	return nil
}

// RevokeOperator снимает membership-строку (roleName, aid) из
// rbac_role_operators. self-lockout-проверка — на стороне
// [Service.RevokeOperator] (здесь только DELETE).
//
// Ошибки:
//   - [ErrRoleOperatorNotFound] при 0 affected rows (пары нет).
//   - wrapped pgx-ошибка на транспортном сбое.
func RevokeOperator(ctx context.Context, db ExecQueryRower, roleName, aid string) error {
	tag, err := db.Exec(ctx, deleteRoleOperatorSQL, roleName, aid)
	if err != nil {
		return fmt.Errorf("rbac: revoke operator (%s -> %s): %w", roleName, aid, wrapPgErr(err))
	}
	if tag.RowsAffected() == 0 {
		return ErrRoleOperatorNotFound
	}
	return nil
}

// LockEffectiveClusterAdmins возвращает AID-ы активных операторов
// (operators.revoked_at IS NULL) с эффективным `*`-permission через любую роль —
// ПРЯМУЮ (rbac_role_operators) ИЛИ через Synod (synod_operators ⋈ synod_roles),
// взяв row-lock (FOR UPDATE) на соответствующие таблицы. ЯДРО self-lockout-
// инварианта (ADR-028(f) + ADR-049(f) Synod-разворот).
//
// Два locking-запроса в фиксированном порядке (прямой → Synod, см.
// [directClusterAdminsForUpdateSQL]): UNION в одном locking-запросе PostgreSQL
// запрещает (SQLSTATE 0A000), поэтому ветки берутся отдельными SELECT-ами и
// объединяются в Go ([dedupAIDs]) — AID, держащий `*` обоими путями, учитывается
// один раз.
//
// tx ОБЯЗАН быть транзакцией (`pgx.Tx`): FOR UPDATE вне tx даёт ошибку PG
// «cannot use FOR UPDATE outside transaction». Lock держится до COMMIT/ROLLBACK
// и сериализует конкурентные lockout-операции.
//
// Источник — БД, а НЕ [Enforcer.ClusterAdmins] — см. комментарий
// [directClusterAdminsForUpdateSQL]: snapshot enforcer-а устаревает до TTL
// (staleness-дыра), FOR UPDATE сериализует гонку.
func LockEffectiveClusterAdmins(ctx context.Context, tx ExecQueryRower) ([]string, error) {
	direct, err := scanAIDs(ctx, tx, directClusterAdminsForUpdateSQL)
	if err != nil {
		return nil, fmt.Errorf("rbac: lock direct cluster-admins: %w", err)
	}
	synod, err := scanAIDs(ctx, tx, synodClusterAdminsForUpdateSQL)
	if err != nil {
		return nil, fmt.Errorf("rbac: lock synod cluster-admins: %w", err)
	}
	return dedupAIDs(append(direct, synod...)), nil
}

// dedupAIDs убирает повторы AID-ов (один AID держит `*` через несколько
// ролей → несколько строк в JOIN-е). Дедуп в Go заменяет SQL DISTINCT,
// запрещённый с FOR UPDATE (см. [directClusterAdminsForUpdateSQL]).
func dedupAIDs(in []string) []string {
	if len(in) <= 1 {
		return in
	}
	seen := make(map[string]struct{}, len(in))
	// Новый слайс, а НЕ in-place in[:0]: явная безопасность на auth-пути —
	// дедуп не должен мутировать входной слайс caller-а (перф здесь неважен).
	out := make([]string, 0, len(in))
	for _, a := range in {
		if _, ok := seen[a]; ok {
			continue
		}
		seen[a] = struct{}{}
		out = append(out, a)
	}
	return out
}

// lockRole берёт row-lock на строку роли (FOR UPDATE) и возвращает её
// builtin-флаг. Первый шаг service-мутаций над ролью: сериализует
// конкурентные delete/update и читает builtin для builtin-границы.
//
// tx ОБЯЗАН быть транзакцией. Возврат [ErrRoleNotFound], если строки нет.
func lockRole(ctx context.Context, tx ExecQueryRower, name string) (builtin bool, err error) {
	rows, err := tx.Query(ctx, lockRoleForUpdateSQL, name)
	if err != nil {
		return false, fmt.Errorf("rbac: lock role %q: %w", name, wrapPgErr(err))
	}
	defer rows.Close()
	if !rows.Next() {
		if err := rows.Err(); err != nil {
			return false, fmt.Errorf("rbac: lock role %q iter: %w", name, err)
		}
		return false, ErrRoleNotFound
	}
	if err := rows.Scan(&builtin); err != nil {
		return false, fmt.Errorf("rbac: scan role %q builtin: %w", name, err)
	}
	return builtin, nil
}

// lockRoleOperator берёт row-lock на membership-строку (role, aid).
// Возврат [ErrRoleOperatorNotFound], если пары нет. tx ОБЯЗАН быть tx.
func lockRoleOperator(ctx context.Context, tx ExecQueryRower, roleName, aid string) error {
	rows, err := tx.Query(ctx, lockRoleOperatorForUpdateSQL, roleName, aid)
	if err != nil {
		return fmt.Errorf("rbac: lock membership (%s -> %s): %w", roleName, aid, wrapPgErr(err))
	}
	defer rows.Close()
	if !rows.Next() {
		if err := rows.Err(); err != nil {
			return fmt.Errorf("rbac: lock membership iter: %w", err)
		}
		return ErrRoleOperatorNotFound
	}
	return nil
}

// UpdateRoleDefaultScope заменяет default_scope роли (ADR-047 S1; replace-
// семантика, NULL снимает scope). Роль обязана существовать — existence-граница
// (lock роли) делается в [Service.UpdateRolePermissions] до вызова. Здесь
// валидируется грамматика scope и пишется UPDATE.
func UpdateRoleDefaultScope(ctx context.Context, db ExecQueryRower, name string, defaultScope *string) error {
	if defaultScope != nil {
		if _, err := ParseDefaultScope(*defaultScope); err != nil {
			return fmt.Errorf("rbac: invalid default_scope %q: %w", *defaultScope, err)
		}
	}
	if _, err := db.Exec(ctx, updateRoleDefaultScopeSQL, name, defaultScopeArg(defaultScope)); err != nil {
		return fmt.Errorf("rbac: update default_scope of role %q: %w", name, wrapPgErr(err))
	}
	return nil
}

// defaultScopeArg переводит *string default_scope в args-значение: nil → PG
// NULL (роль без scope-ограничения), иначе разыменованная RAW-строка. Пустую
// строку приравниваем к NULL — «введённое пустое» в coven-MVP смысла не имеет
// (см. ResolvePurview/отчёт S1).
func defaultScopeArg(scope *string) any {
	if scope == nil || *scope == "" {
		return nil
	}
	return *scope
}

// roleGivesWildcard — true, если набор permission-строк роли содержит `*`.
// Используется service-ом для решения, нужна ли self-lockout-проверка
// (мутация роли без `*` кластер не залочит).
func roleGivesWildcard(permissions []string) bool {
	for _, p := range permissions {
		if p == "*" {
			return true
		}
	}
	return false
}

// rolePermissions читает permission-строки роли (без lock-а) — нужен
// service-у, чтобы узнать, давала ли роль `*` ДО update/delete. Отдельный
// SELECT (роль уже залочена lockRole-ом в той же tx).
func rolePermissions(ctx context.Context, tx ExecQueryRower, name string) ([]string, error) {
	rows, err := tx.Query(ctx, `SELECT permission FROM rbac_role_permissions WHERE role_name = $1`, name)
	if err != nil {
		return nil, fmt.Errorf("rbac: read permissions of role %q: %w", name, wrapPgErr(err))
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var p string
		if err := rows.Scan(&p); err != nil {
			return nil, fmt.Errorf("rbac: scan permission of role %q: %w", name, err)
		}
		out = append(out, p)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("rbac: iter permissions of role %q: %w", name, err)
	}
	return out, nil
}

// roleDefaultScope читает RAW default_scope роли (ADR-047 S1) — нужен subset-у
// при UpdateRolePermissions без SetDefaultScope: добавляемые bare-perms
// наследуют СУЩЕСТВУЮЩИЙ scope роли (PATCH-семантика). nil = NULL (роль без
// scope). Роль уже залочена lockRole-ом в той же tx.
func roleDefaultScope(ctx context.Context, tx ExecQueryRower, name string) (*string, error) {
	rows, err := tx.Query(ctx, `SELECT default_scope FROM rbac_roles WHERE name = $1`, name)
	if err != nil {
		return nil, fmt.Errorf("rbac: read default_scope of role %q: %w", name, wrapPgErr(err))
	}
	defer rows.Close()
	var scope *string
	if rows.Next() {
		if err := rows.Scan(&scope); err != nil {
			return nil, fmt.Errorf("rbac: scan default_scope of role %q: %w", name, err)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("rbac: iter default_scope of role %q: %w", name, err)
	}
	return scope, nil
}

// mapRoleError маппит pgx-ошибки INSERT-ов в sentinel-ы пакета по образцу
// [operator.mapInsertError]:
//   - 23505 (UNIQUE) → [ErrRoleAlreadyExists] (multi-wrap: sentinel + оригинал).
//   - 23503 (FK) → wrapped с именем constraint-а (created_by_aid /
//     role_name / aid ссылаются на несуществующую строку).
//   - прочее → wrapped с SQLSTATE.
func mapRoleError(err error) error {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		switch pgErr.Code {
		case pgErrCodeUniqueViolation:
			return fmt.Errorf("%w (constraint %s): %w", ErrRoleAlreadyExists, pgErr.ConstraintName, err)
		case pgErrCodeForeignKeyViolation:
			return fmt.Errorf("rbac: FK violation on %s: %w", pgErr.ConstraintName, err)
		}
	}
	return fmt.Errorf("rbac: %w", wrapPgErr(err))
}

// mapGrantError маппит pgx-ошибку INSERT-membership-а (grant-operator path).
// Роль service проверяет lock-ом до insert-а, поэтому FK-violation тут — это
// несуществующий operator (aid или granted_by_aid):
//   - 23503 (FK) → [ErrOperatorNotFound] (multi-wrap: sentinel + оригинал).
//   - прочее → wrapped с SQLSTATE.
func mapGrantError(err error) error {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == pgErrCodeForeignKeyViolation {
		return fmt.Errorf("%w (constraint %s): %w", ErrOperatorNotFound, pgErr.ConstraintName, err)
	}
	return fmt.Errorf("rbac: %w", wrapPgErr(err))
}
