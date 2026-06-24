package rbac

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// ExecQueryRower — узкое подмножество pgxpool.Pool, нужное repository-у для
// чтения RBAC-снимка и записи membership-а. Симметрично
// [operator.ExecQueryRower]; объявлено локально, чтобы пакет rbac не тянул
// operator. `*pgxpool.Pool` / `pgx.Tx` удовлетворяют автоматически.
type ExecQueryRower interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}

// Compile-time check: pgx.Tx удовлетворяет ExecQueryRower (bootstrap пишет
// membership внутри своей advisory-lock-транзакции; pool — в `keeper run`).
var _ ExecQueryRower = (pgx.Tx)(nil)

// Snapshot — сырой снимок RBAC из трёх таблиц до парсинга permission-строк.
// Разделение «сырой снимок ↔ распарсенный Enforcer» позволяет тестировать
// загрузку из БД отдельно от парсинга/матчинга permissions.
//
// Revoked — четвёртая проекция (ADR-014 Amendment 2026-05-27, JWT immediate
// revoke): AID → `operators.revoked_at` ревокнутых Архонтов. Кладётся в тот
// же снимок RBAC, чтобы revoke-проверка работала тем же путём, что и обычная
// permission-проверка (TTL-poll + `rbac:invalidate` pub/sub), без отдельной
// JWT-blocklist-инфраструктуры.
type Snapshot struct {
	// Roles — имя роли → её permission-строки (RAW, как в БД).
	// Роль без permissions присутствует с пустым/nil-слайсом — она валидна,
	// просто ничего не разрешает.
	Roles map[string][]string

	// RoleScopes — имя роли → RAW default_scope-строка (ADR-047 S1).
	// Только роли с НЕ-NULL default_scope попадают сюда; отсутствие ключа =
	// NULL = измерение НЕ введено (bare-perms роли unrestricted, backcompat).
	RoleScopes map[string]string

	// Membership — AID → имена ролей, привязанных к нему (rbac_role_operators).
	Membership map[string][]string

	// Revoked — AID → `operators.revoked_at` для всех Архонтов с
	// `revoked_at IS NOT NULL`. Активные операторы здесь отсутствуют.
	// Используется [Enforcer.Check] первым шагом — revoked AID получает
	// deny независимо от ролей (ADR-014 Amendment 2026-05-27).
	Revoked map[string]time.Time
}

const (
	// selectRolesSQL — все роли каталога с default_scope (ADR-047 S1). Роль без
	// permissions тоже попадает в снимок (LEFT JOIN ниже её бы дал, но отдельный
	// SELECT проще и без дедупликации). default_scope NULL → scan в *string=nil.
	selectRolesSQL = `SELECT name, default_scope FROM rbac_roles`

	// selectRolePermissionsSQL — все (role_name, permission)-пары.
	selectRolePermissionsSQL = `SELECT role_name, permission FROM rbac_role_permissions`

	// selectRoleOperatorsSQL — все membership-строки (role_name, aid).
	selectRoleOperatorsSQL = `SELECT role_name, aid FROM rbac_role_operators`

	// selectRevokedOperatorsSQL — AID-ы и `revoked_at` всех ревокнутых
	// Архонтов (ADR-014 Amendment 2026-05-27). Используется [LoadSnapshot]
	// для наполнения Snapshot.Revoked. Активные не выбираются — снимок
	// держит только revoked-проекцию.
	selectRevokedOperatorsSQL = `SELECT aid, revoked_at FROM operators WHERE revoked_at IS NOT NULL`

	// selectSynodOperatorsSQL — membership «Synod ↔ архон» (ADR-049). Соединяется
	// с selectSynodRolesSQL в Go при сборке снимка: эффективные роли архона =
	// прямые ∪ роли через все его Synod-ы.
	selectSynodOperatorsSQL = `SELECT synod_name, aid FROM synod_operators`

	// selectSynodRolesSQL — bundle «Synod ↔ роль» (ADR-049). Группа → её роли;
	// разворачивается в Membership через synod_operators.
	selectSynodRolesSQL = `SELECT synod_name, role_name FROM synod_roles`
)

// LoadSnapshot читает RBAC-таблицы и собирает [Snapshot]. Отдельные SELECT-ы
// (без JOIN) — данных мало (роли/membership редки), денормализация JOIN-ом
// усложнила бы дедупликацию permissions роли с множеством membership-ов.
//
// Synod (ADR-049): к прямому membership-у (rbac_role_operators) добавляются
// роли через все Synod-ы архона — `synod_operators` ⋈ `synod_roles`. Эффективные
// роли архона в [Snapshot.Membership] = прямые ∪ через Synod (union множества,
// дедуп). Это собирается ДО возврата снимка; `NewEnforcerFromSnapshot` и матчинг
// ниже Synod не видят — источник роли им безразличен.
//
// Permission-строки НЕ парсятся здесь — это делает [NewEnforcerFromSnapshot]
// существующим [ParsePermission]; repository остаётся транспортным слоем.
//
// Membership-строки (прямые и через Synod), ссылающиеся на роль вне Roles, в
// enforcer не попадают (FK гарантирует существование роли, но защищаемся от
// рассинхрона: роль без записи в rbac_roles игнорируется при сборке Enforcer).
func LoadSnapshot(ctx context.Context, db ExecQueryRower) (*Snapshot, error) {
	snap := &Snapshot{
		Roles:      make(map[string][]string),
		RoleScopes: make(map[string]string),
		Membership: make(map[string][]string),
		Revoked:    make(map[string]time.Time),
	}

	if err := loadRoles(ctx, db, snap); err != nil {
		return nil, err
	}
	if err := loadPermissions(ctx, db, snap); err != nil {
		return nil, err
	}
	if err := loadMembership(ctx, db, snap); err != nil {
		return nil, err
	}
	if err := loadRevoked(ctx, db, snap); err != nil {
		return nil, err
	}
	if err := loadSynodMembership(ctx, db, snap); err != nil {
		return nil, err
	}
	return snap, nil
}

func loadRoles(ctx context.Context, db ExecQueryRower, snap *Snapshot) error {
	rows, err := db.Query(ctx, selectRolesSQL)
	if err != nil {
		return fmt.Errorf("rbac: query roles: %w", wrapPgErr(err))
	}
	defer rows.Close()
	for rows.Next() {
		var (
			name         string
			defaultScope *string
		)
		if err := rows.Scan(&name, &defaultScope); err != nil {
			return fmt.Errorf("rbac: scan role: %w", err)
		}
		if _, ok := snap.Roles[name]; !ok {
			snap.Roles[name] = nil
		}
		// NULL default_scope → ключ не кладём (отсутствие = измерение не
		// введено, см. RoleScopes-комментарий). Пустую строку трактуем как
		// NULL (никаких «введённое пустое измерение» в coven-MVP — см. отчёт
		// S1 про Deny).
		if defaultScope != nil && *defaultScope != "" {
			snap.RoleScopes[name] = *defaultScope
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("rbac: iter roles: %w", err)
	}
	return nil
}

func loadPermissions(ctx context.Context, db ExecQueryRower, snap *Snapshot) error {
	rows, err := db.Query(ctx, selectRolePermissionsSQL)
	if err != nil {
		return fmt.Errorf("rbac: query permissions: %w", wrapPgErr(err))
	}
	defer rows.Close()
	for rows.Next() {
		var roleName, permission string
		if err := rows.Scan(&roleName, &permission); err != nil {
			return fmt.Errorf("rbac: scan permission: %w", err)
		}
		snap.Roles[roleName] = append(snap.Roles[roleName], permission)
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("rbac: iter permissions: %w", err)
	}
	return nil
}

func loadMembership(ctx context.Context, db ExecQueryRower, snap *Snapshot) error {
	rows, err := db.Query(ctx, selectRoleOperatorsSQL)
	if err != nil {
		return fmt.Errorf("rbac: query membership: %w", wrapPgErr(err))
	}
	defer rows.Close()
	for rows.Next() {
		var roleName, aid string
		if err := rows.Scan(&roleName, &aid); err != nil {
			return fmt.Errorf("rbac: scan membership: %w", err)
		}
		snap.Membership[aid] = append(snap.Membership[aid], roleName)
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("rbac: iter membership: %w", err)
	}
	return nil
}

// loadRevoked заполняет Snapshot.Revoked AID-ами ревокнутых Архонтов (ADR-014
// Amendment 2026-05-27). Активные операторы пропускаются на стороне SQL
// (WHERE revoked_at IS NOT NULL) — снимок остаётся компактным.
func loadRevoked(ctx context.Context, db ExecQueryRower, snap *Snapshot) error {
	rows, err := db.Query(ctx, selectRevokedOperatorsSQL)
	if err != nil {
		return fmt.Errorf("rbac: query revoked: %w", wrapPgErr(err))
	}
	defer rows.Close()
	for rows.Next() {
		var (
			aid       string
			revokedAt time.Time
		)
		if err := rows.Scan(&aid, &revokedAt); err != nil {
			return fmt.Errorf("rbac: scan revoked: %w", err)
		}
		snap.Revoked[aid] = revokedAt
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("rbac: iter revoked: %w", err)
	}
	return nil
}

// loadSynodMembership разворачивает роли через Synod-ы и ДОПОЛНЯЕТ ими
// snap.Membership (ADR-049(c)/(e)): эффективные роли архона = прямые ∪ через
// все его Synod-ы. Дубль роли (через прямой грант И Synod, либо через два
// Synod-а) идемпотентен — union множества, не мультимножество.
//
// Два SELECT-а (synod_operators / synod_roles) собираются в Go: bundle «Synod →
// роли» строится map-ой, затем для каждой membership-строки synod_operators
// роли группы добавляются архону с дедупом против уже-известных (прямых +
// добавленных другими Synod-ами того же архона).
//
// Вызывается ПОСЛЕ loadMembership — прямые роли уже в snap.Membership и учтены
// при дедупе. Synod-роли, ссылающиеся на роль вне каталога, попадают в
// Membership, но отбрасываются NewEnforcerFromSnapshot (как dangling прямой
// membership) — repository остаётся транспортным слоем.
func loadSynodMembership(ctx context.Context, db ExecQueryRower, snap *Snapshot) error {
	bundle, err := loadSynodRoles(ctx, db)
	if err != nil {
		return err
	}

	rows, err := db.Query(ctx, selectSynodOperatorsSQL)
	if err != nil {
		return fmt.Errorf("rbac: query synod operators: %w", wrapPgErr(err))
	}
	defer rows.Close()

	// known[aid] — множество ролей, уже закреплённых за архоном (прямые +
	// добавленные предыдущими Synod-ами в этом проходе). Lazy-инициализация из
	// snap.Membership, чтобы прямые роли участвовали в дедупе.
	known := make(map[string]map[string]struct{})
	for rows.Next() {
		var synodName, aid string
		if err := rows.Scan(&synodName, &aid); err != nil {
			return fmt.Errorf("rbac: scan synod operator: %w", err)
		}
		seen, ok := known[aid]
		if !ok {
			seen = make(map[string]struct{}, len(snap.Membership[aid]))
			for _, r := range snap.Membership[aid] {
				seen[r] = struct{}{}
			}
			known[aid] = seen
		}
		for _, roleName := range bundle[synodName] {
			if _, dup := seen[roleName]; dup {
				continue
			}
			seen[roleName] = struct{}{}
			snap.Membership[aid] = append(snap.Membership[aid], roleName)
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("rbac: iter synod operators: %w", err)
	}
	return nil
}

// loadSynodRoles читает bundle «Synod → роли» (synod_roles) в map. Дубль
// (synod_name, role_name) исключён PK, но дедуп на чтении дешёв и безопасен.
func loadSynodRoles(ctx context.Context, db ExecQueryRower) (map[string][]string, error) {
	rows, err := db.Query(ctx, selectSynodRolesSQL)
	if err != nil {
		return nil, fmt.Errorf("rbac: query synod roles: %w", wrapPgErr(err))
	}
	defer rows.Close()
	bundle := make(map[string][]string)
	for rows.Next() {
		var synodName, roleName string
		if err := rows.Scan(&synodName, &roleName); err != nil {
			return nil, fmt.Errorf("rbac: scan synod role: %w", err)
		}
		bundle[synodName] = append(bundle[synodName], roleName)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("rbac: iter synod roles: %w", err)
	}
	return bundle, nil
}

// RoleView — API-проекция роли для read-эндпоинтов role.list (Slice 2a/2b):
// каталожные поля (Description / Builtin) + развёрнутые Permissions и
// Operators (AID-ы). Отличается от [Snapshot]: тот — enforcer-снимок без
// description/builtin (matching-only); этот — человеко-/API-ориентированный
// view. Operators отсортирован детерминированно (стабильный вывод list-а).
type RoleView struct {
	Name        string
	Description string
	Builtin     bool
	Permissions []string
	Operators   []string
	// DefaultScope — RAW default_scope роли (ADR-047 S1); пустая строка = NULL
	// (роль без scope-ограничения). Для list-эндпоинта.
	DefaultScope string
}

const (
	// selectRoleViewsSQL — каталог ролей с description/builtin/default_scope (в
	// отличие от [selectRolesSQL], читающего name+default_scope для enforcer-
	// снимка). ORDER BY name — детерминированный порядок list-а.
	selectRoleViewsSQL = `SELECT name, description, builtin, default_scope FROM rbac_roles ORDER BY name`
)

// LoadRoleViews собирает API-каталог ролей тремя SELECT-ами (роли /
// permissions / membership) — без N+1, симметрично [LoadSnapshot]. Сборка
// «role → его permissions/operators» делается в Go по name-ключу.
//
// Permissions/Operators роли без записей присутствуют пустым слайсом (роль
// валидна, просто ничего не разрешает / никому не назначена). Permission-
// строки и membership-строки, ссылающиеся на роль вне каталога, отбрасываются
// (FK гарантирует консистентность, но защищаемся от рассинхрона — как
// [LoadSnapshot]).
func LoadRoleViews(ctx context.Context, db ExecQueryRower) ([]RoleView, error) {
	views, index, err := loadRoleViewRows(ctx, db)
	if err != nil {
		return nil, err
	}
	if err := loadRoleViewPermissions(ctx, db, index); err != nil {
		return nil, err
	}
	if err := loadRoleViewOperators(ctx, db, index); err != nil {
		return nil, err
	}
	return views, nil
}

// loadRoleViewRows читает каталог ролей в slice (детерминированный порядок —
// ORDER BY name) и строит index «name → *RoleView» на тот же backing-массив,
// чтобы permissions/operators наполнялись по месту без повторного прохода.
func loadRoleViewRows(ctx context.Context, db ExecQueryRower) ([]RoleView, map[string]*RoleView, error) {
	rows, err := db.Query(ctx, selectRoleViewsSQL)
	if err != nil {
		return nil, nil, fmt.Errorf("rbac: query role views: %w", wrapPgErr(err))
	}
	defer rows.Close()
	var views []RoleView
	for rows.Next() {
		var (
			v            RoleView
			defaultScope *string
		)
		if err := rows.Scan(&v.Name, &v.Description, &v.Builtin, &defaultScope); err != nil {
			return nil, nil, fmt.Errorf("rbac: scan role view: %w", err)
		}
		if defaultScope != nil {
			v.DefaultScope = *defaultScope
		}
		views = append(views, v)
	}
	if err := rows.Err(); err != nil {
		return nil, nil, fmt.Errorf("rbac: iter role views: %w", err)
	}
	index := make(map[string]*RoleView, len(views))
	for i := range views {
		index[views[i].Name] = &views[i]
	}
	return views, index, nil
}

func loadRoleViewPermissions(ctx context.Context, db ExecQueryRower, index map[string]*RoleView) error {
	rows, err := db.Query(ctx, selectRolePermissionsSQL)
	if err != nil {
		return fmt.Errorf("rbac: query role-view permissions: %w", wrapPgErr(err))
	}
	defer rows.Close()
	for rows.Next() {
		var roleName, permission string
		if err := rows.Scan(&roleName, &permission); err != nil {
			return fmt.Errorf("rbac: scan role-view permission: %w", err)
		}
		if v, ok := index[roleName]; ok {
			v.Permissions = append(v.Permissions, permission)
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("rbac: iter role-view permissions: %w", err)
	}
	return nil
}

func loadRoleViewOperators(ctx context.Context, db ExecQueryRower, index map[string]*RoleView) error {
	rows, err := db.Query(ctx, selectRoleOperatorsSQL)
	if err != nil {
		return fmt.Errorf("rbac: query role-view operators: %w", wrapPgErr(err))
	}
	defer rows.Close()
	for rows.Next() {
		var roleName, aid string
		if err := rows.Scan(&roleName, &aid); err != nil {
			return fmt.Errorf("rbac: scan role-view operator: %w", err)
		}
		if v, ok := index[roleName]; ok {
			v.Operators = append(v.Operators, aid)
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("rbac: iter role-view operators: %w", err)
	}
	return nil
}

// insertRoleOperatorSQL — INSERT membership-строки (role_name, aid).
// granted_at берётся из DEFAULT NOW(), если caller не задал; granted_by_aid
// опционален (NULL у seed-/bootstrap-membership-а). ON CONFLICT DO NOTHING
// делает вставку идемпотентной — повторный grant той же пары не ошибка
// (симметрично seed-миграции 027).
const insertRoleOperatorSQL = `
INSERT INTO rbac_role_operators (role_name, aid, granted_by_aid)
VALUES ($1, $2, $3)
ON CONFLICT (role_name, aid) DO NOTHING
`

// GrantOperator привязывает AID к роли — добавляет membership-строку в
// rbac_role_operators (ADR-028(c)). Используется keeper init для привязки
// первого Архонта к cluster-admin (фикс BUG-1) внутри его advisory-lock-
// транзакции; в Фазе 2 — role.grant-operator через API.
//
// grantedByAID == nil → granted_by_aid IS NULL (bootstrap-membership без
// инициатора-Архонта). Идемпотентно: повторный grant той же пары — no-op.
//
// Ошибки:
//   - FK-violation на role_name → роль не существует (seed-миграция 027 не
//     применена); на aid → оператор не существует.
//   - прочие — wrapped с SQLSTATE.
func GrantOperator(ctx context.Context, db ExecQueryRower, roleName, aid string, grantedByAID *string) error {
	var grantedBy any
	if grantedByAID != nil {
		grantedBy = *grantedByAID
	}
	if _, err := db.Exec(ctx, insertRoleOperatorSQL, roleName, aid, grantedBy); err != nil {
		return fmt.Errorf("rbac: grant operator (%s -> %s): %w", roleName, aid, wrapPgErr(err))
	}
	return nil
}

// selectDirectRolesOfSQL — имена ПРЯМЫХ membership-ролей одного AID
// (rbac_role_operators). НЕ включает роли через Synod (ADR-049) — federated-
// реконсиляция (HIGH-1, ADR-058(d)) реконсилирует только прямой membership,
// управляемый group_role_map, и не должна трогать Synod-выданные роли.
const selectDirectRolesOfSQL = `SELECT role_name FROM rbac_role_operators WHERE aid = $1`

// DirectRolesOf возвращает имена ролей, привязанных к AID ПРЯМОЙ membership-
// строкой rbac_role_operators (без Synod-разворота). Используется federated-
// реконсиляцией (auth/mapper.go, HIGH-1) для вычисления роли, которые надо
// снять, когда внешние группы пользователя изменились.
//
// db может быть pool ИЛИ tx (pgx.Tx удовлетворяет ExecQueryRower) — caller
// читает текущий membership и пишет grant/revoke в ОДНОЙ транзакции.
func DirectRolesOf(ctx context.Context, db ExecQueryRower, aid string) ([]string, error) {
	rows, err := db.Query(ctx, selectDirectRolesOfSQL, aid)
	if err != nil {
		return nil, fmt.Errorf("rbac: select direct roles of %q: %w", aid, wrapPgErr(err))
	}
	defer rows.Close()

	var out []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, fmt.Errorf("rbac: scan direct role of %q: %w", aid, err)
		}
		out = append(out, name)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("rbac: iterate direct roles of %q: %w", aid, err)
	}
	return out, nil
}

// wrapPgErr добавляет SQLSTATE в сообщение, если ошибка — pgconn.PgError.
// Это упрощает диагностику «таблица rbac_* не существует» (миграция не
// применена) от транспортных сбоев.
func wrapPgErr(err error) error {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return fmt.Errorf("pg %s: %w", pgErr.Code, err)
	}
	return err
}
