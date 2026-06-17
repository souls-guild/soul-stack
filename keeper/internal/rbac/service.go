package rbac

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync/atomic"

	"github.com/jackc/pgx/v5"
)

// ErrInvalidRoleName — name роли не проходит [reRoleName]. Validation-error
// sentinel (отдельно от ErrRoleAlreadyExists / ErrRoleNotFound): transport
// маппит в 422, а не 409/404. Конкретный битый permission возвращается
// wrapped-ошибкой ParsePermission (тоже 422; sentinel не нужен — текст несёт
// диагностику).
var ErrInvalidRoleName = errors.New("rbac: invalid role name")

// ServicePool — узкое подмножество pgxpool.Pool, нужное [Service]:
// транспортная поверхность [ExecQueryRower] + BeginTx для атомарных мутаций
// под FOR UPDATE. Реальный `*pgxpool.Pool` удовлетворяет автоматически.
//
// Симметрично [operator.ServicePool]; объявлено локально, чтобы rbac не тянул
// operator (и обратно — избегаем import-цикла, см. [ErrWouldLockOutCluster]).
type ServicePool interface {
	ExecQueryRower
	BeginTx(ctx context.Context, opts pgx.TxOptions) (pgx.Tx, error)
}

// Invalidator — поверхность cluster-wide RBAC-инвалидации (ADR-028(d), B2).
// После успешного commit-а role-мутации [Service] вызывает Invalidate, чтобы
// остальные Keeper-ноды near-instant перечитали снимок (вместо ожидания
// TTL-poll-а). Реализуется в `keeper run` адаптером поверх
// [keeperredis.PublishRBACInvalidate]; в single-Keeper/dev-режиме (без Redis)
// инвалидатор не подключён — работает только TTL-poll.
//
// Invalidate — best-effort: ошибку публикации НЕ возвращает (мутация уже
// зафиксирована в БД), реализация логирует и глотает.
type Invalidator interface {
	Invalidate(ctx context.Context)
}

// ServiceDeps — зависимости [Service]. Все поля immutable после конструктора.
type ServiceDeps struct {
	Pool   ServicePool
	Logger *slog.Logger
}

// Service — бизнес-логика RBAC-CRUD (роли / permissions / membership) под
// role.*-permissions (ADR-028(e)). Один источник правды для будущего
// transport-фасада (OpenAPI/MCP — Slice 2); инварианты (builtin-граница,
// self-lockout) живут здесь, transport только декодирует input / кодирует
// output.
//
// Безопасен для конкурентного использования: deps immutable, состояние не
// держится; атомарность мутаций обеспечивается транзакциями + FOR UPDATE.
type Service struct {
	pool   ServicePool
	logger *slog.Logger

	// inv — опциональный cluster-wide invalidator (B2). Late-binding через
	// [Service.SetInvalidator]: Redis-клиент в `keeper run` поднимается ПОСЛЕ
	// NewService, поэтому инъекция отложена (паттерн store.SetAuditWriter /
	// vc.SetMetrics в main.go). atomic.Pointer — конкурентная запись сеттером
	// vs. чтение из мутаций без отдельного mutex-а.
	inv atomic.Pointer[Invalidator]
}

// NewService собирает service. Pool обязателен.
func NewService(d ServiceDeps) (*Service, error) {
	if d.Pool == nil {
		return nil, errors.New("rbac: ServiceDeps.Pool is nil")
	}
	return &Service{pool: d.Pool, logger: d.Logger}, nil
}

// SetInvalidator late-binding-ом подключает cluster-wide invalidator (B2).
// Вызывается из `keeper run` после подъёма Redis-клиента. nil — снять
// invalidator (вернуться к чистому TTL-poll-у). Идемпотентен, потокобезопасен.
func (s *Service) SetInvalidator(inv Invalidator) {
	if inv == nil {
		s.inv.Store(nil)
		return
	}
	s.inv.Store(&inv)
}

// invalidate шлёт cluster-wide invalidate-сигнал после успешного commit-а
// role-мутации (B2). No-op, если invalidator не подключён (single-Keeper/dev).
// Best-effort: реализация Invalidate сама логирует и глотает ошибку publish-а
// — мутация уже зафиксирована, потеря сигнала компенсируется TTL-poll-ом.
func (s *Service) invalidate(ctx context.Context) {
	if p := s.inv.Load(); p != nil {
		(*p).Invalidate(ctx)
	}
}

// CreateRoleInput — параметры CreateRole.
type CreateRoleInput struct {
	Name        string
	Description string
	Permissions []string
	CallerAID   string
	// DefaultScope — role default_scope (ADR-047 S1), наследуемый permission-ами
	// роли без своего селектора. nil = роль без scope-ограничения (backcompat).
	DefaultScope *string
}

// CreateRole создаёт роль с её permissions. Валидация name + КАЖДОГО
// permission через [ParsePermission] идёт ДО открытия tx (битый ввод не
// должен держать транзакцию).
//
// Возврат:
//   - [ErrInvalidRoleName] — name не по формату (422).
//   - wrapped ParsePermission-ошибка — битый permission (422).
//   - [ErrRoleAlreadyExists] — name занят (409).
//   - wrapped FK-violation — CallerAID не существует в operators (вряд ли:
//     middleware гарантирует валидного caller-а, но FK защищает).
func (s *Service) CreateRole(ctx context.Context, in CreateRoleInput) error {
	if !reRoleName.MatchString(in.Name) {
		return fmt.Errorf("%w: %q must match %s", ErrInvalidRoleName, in.Name, reRoleName.String())
	}
	for _, raw := range in.Permissions {
		if _, err := ParsePermission(raw); err != nil {
			return fmt.Errorf("rbac: invalid permission %q: %w", raw, err)
		}
	}
	if in.DefaultScope != nil {
		if _, err := ParseDefaultScope(*in.DefaultScope); err != nil {
			return fmt.Errorf("rbac: invalid default_scope %q: %w", *in.DefaultScope, err)
		}
	}

	var createdBy *string
	if in.CallerAID != "" {
		createdBy = &in.CallerAID
	}

	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return fmt.Errorf("rbac: begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// Least-privilege subset-check (ADR-028, rbac.md → § Инвариант
	// least-privilege): нельзя создать роль с permission, которым caller не
	// обладает сам. Защита от вертикальной эскалации (role.create без `*` →
	// роль с `*` → grant себе → cluster-admin). Гранящиеся bare-perms
	// разворачиваются под создаваемый default_scope роли (ADR-047 S1), иначе
	// caller со scope=prod выдал бы роль scope=staging.
	required, err := requiredPermissions(in.Permissions, in.DefaultScope)
	if err != nil {
		return err
	}
	if err := s.assertCallerMayGrant(ctx, tx, in.CallerAID, required); err != nil {
		return err
	}

	if err := CreateRole(ctx, tx, in.Name, in.Description, in.Permissions, createdBy, in.DefaultScope); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("rbac: commit tx: %w", err)
	}
	s.invalidate(ctx)
	return nil
}

// DeleteRole удаляет роль (каскадом permissions + membership).
//
// Порядок проверок в tx (детерминированный lock-порядок против deadlock — R2:
// роль → permissions → membership/operators):
//  1. lock строки роли (SELECT … FOR UPDATE); нет → [ErrRoleNotFound].
//  2. builtin=true → [ErrRoleBuiltin] (ПЕРВОЙ, до lockout — builtin важнее).
//  3. если роль даёт `*` — self-lockout-проверка: останутся ли активные
//     админы с `*` через РОЛЬ ≠ удаляемая; пусто → [ErrWouldLockOutCluster].
//  4. DELETE.
func (s *Service) DeleteRole(ctx context.Context, name string) error {
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return fmt.Errorf("rbac: begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	builtin, err := lockRole(ctx, tx, name)
	if err != nil {
		return err
	}
	if builtin {
		return ErrRoleBuiltin
	}

	perms, err := rolePermissions(ctx, tx, name)
	if err != nil {
		return err
	}
	if roleGivesWildcard(perms) {
		if err := s.assertNotLastWildcardRole(ctx, tx, name); err != nil {
			return err
		}
	}

	if err := DeleteRole(ctx, tx, name); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("rbac: commit tx: %w", err)
	}
	s.invalidate(ctx)
	return nil
}

// UpdateRolePermissionsInput — параметры UpdateRolePermissions.
type UpdateRolePermissionsInput struct {
	Name        string
	Permissions []string
	CallerAID   string

	// SetDefaultScope — если true, default_scope роли ЗАМЕНЯЕТСЯ значением
	// DefaultScope (nil → снять scope). Если false — default_scope не трогается
	// (PATCH-семантика: caller, не передавший поле, не сбрасывает scope роли).
	SetDefaultScope bool
	// DefaultScope — новое значение default_scope при SetDefaultScope=true.
	DefaultScope *string
}

// UpdateRolePermissions заменяет набор permissions роли (replace-семантика).
//
// Порядок в tx:
//  1. lock роли; нет → [ErrRoleNotFound].
//  2. builtin=true → [ErrRoleBuiltin] (до lockout).
//  3. валидация нового набора через [ParsePermission].
//  4. если старый набор давал `*`, а новый — нет → self-lockout-проверка
//     (останутся ли админы с `*` через РОЛЬ ≠ обновляемая); пусто →
//     [ErrWouldLockOutCluster].
//  5. replace.
func (s *Service) UpdateRolePermissions(ctx context.Context, in UpdateRolePermissionsInput) error {
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return fmt.Errorf("rbac: begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	builtin, err := lockRole(ctx, tx, in.Name)
	if err != nil {
		return err
	}
	if builtin {
		return ErrRoleBuiltin
	}

	for _, raw := range in.Permissions {
		if _, err := ParsePermission(raw); err != nil {
			return fmt.Errorf("rbac: invalid permission %q: %w", raw, err)
		}
	}
	if in.SetDefaultScope && in.DefaultScope != nil {
		if _, err := ParseDefaultScope(*in.DefaultScope); err != nil {
			return fmt.Errorf("rbac: invalid default_scope %q: %w", *in.DefaultScope, err)
		}
	}

	// rolePermissions читает старый набор без отдельного lock-а на
	// rbac_role_permissions: роль уже залочена lockRole-ом (FOR UPDATE на
	// rbac_roles) в этой же tx, а изменить permissions роли можно только
	// через эту роль-строку — конкурентная мутация сериализуется тем же
	// row-lock-ом. Дополнительный lock на permission-строки избыточен.
	oldPerms, err := rolePermissions(ctx, tx, in.Name)
	if err != nil {
		return err
	}

	// Least-privilege subset-check: ограничиваются только ДОБАВЛЯЕМЫЕ
	// permissions (новые, которых не было в старом наборе). Удаление прав не
	// ограничивается (оператор может урезать чужую роль, даже не владея этими
	// правами — это не эскалация). Защита от обхода через update: caller с
	// role.update добавляет `*` в существующую роль.
	added := addedPermissions(oldPerms, in.Permissions)
	// Эффективный scope добавляемых bare-perms (ADR-047 S1): при SetDefaultScope
	// — НОВОЕ значение (replace), иначе — СУЩЕСТВУЮЩИЙ scope роли (PATCH:
	// добавляемые права наследуют тот scope, под которым роль будет жить).
	grantedScope := in.DefaultScope
	if !in.SetDefaultScope {
		grantedScope, err = roleDefaultScope(ctx, tx, in.Name)
		if err != nil {
			return err
		}
	}
	required, err := requiredPermissions(added, grantedScope)
	if err != nil {
		return err
	}
	if err := s.assertCallerMayGrant(ctx, tx, in.CallerAID, required); err != nil {
		return err
	}

	// Self-lockout-проверка нужна только при снятии `*`: старый набор давал
	// `*`, новый — нет. Если новый набор тоже даёт `*` — кластер не залочится.
	if roleGivesWildcard(oldPerms) && !roleGivesWildcard(in.Permissions) {
		if err := s.assertNotLastWildcardRole(ctx, tx, in.Name); err != nil {
			return err
		}
	}

	if err := UpdateRolePermissions(ctx, tx, in.Name, in.Permissions); err != nil {
		return err
	}
	if in.SetDefaultScope {
		if err := UpdateRoleDefaultScope(ctx, tx, in.Name, in.DefaultScope); err != nil {
			return err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("rbac: commit tx: %w", err)
	}
	s.invalidate(ctx)
	return nil
}

// RevokeOperatorInput — параметры RevokeOperator.
type RevokeOperatorInput struct {
	RoleName string
	AID      string
}

// RevokeOperator снимает membership-строку (RoleName, AID).
//
// builtin-граница: revoke-operator над builtin cluster-admin РАЗРЕШЁН (иначе
// нельзя снять ошибочно назначенного админа), но с тем же self-lockout-ом.
//
// Порядок в tx:
//  1. lock membership-строки; нет → [ErrRoleOperatorNotFound].
//  2. если роль даёт `*` И снимаемый AID держит `*` ТОЛЬКО через неё —
//     self-lockout-проверка: останутся ли активные админы с `*` после
//     исключения пары (RoleName, AID); пусто → [ErrWouldLockOutCluster].
//  3. DELETE.
func (s *Service) RevokeOperator(ctx context.Context, in RevokeOperatorInput) error {
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return fmt.Errorf("rbac: begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if err := lockRoleOperator(ctx, tx, in.RoleName, in.AID); err != nil {
		return err
	}

	// Lock роли (детерминированный порядок: роль → permissions → operators —
	// против deadlock R2) и чтение её permissions — нужно знать, даёт ли роль `*`.
	if _, err := lockRole(ctx, tx, in.RoleName); err != nil {
		// Роль не может исчезнуть после успешного lockRoleOperator (FK +
		// row-lock на membership), но защищаемся: ErrRoleNotFound → как есть.
		return err
	}
	perms, err := rolePermissions(ctx, tx, in.RoleName)
	if err != nil {
		return err
	}
	if roleGivesWildcard(perms) {
		// Снимаем последнего админа с `*`? Контрольный запрос ПОД FOR UPDATE
		// исключает целевую пару (RoleName, AID): если AID держит `*` и через
		// другие роли — он останется в выборке и lockout не сработает.
		if err := s.assertNotLastWildcardOperator(ctx, tx, in.RoleName, in.AID); err != nil {
			return err
		}
	}

	if err := RevokeOperator(ctx, tx, in.RoleName, in.AID); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("rbac: commit tx: %w", err)
	}
	s.invalidate(ctx)
	return nil
}

// GrantOperatorInput — параметры GrantOperator. CallerAID опционален
// (nil → granted_by_aid IS NULL для bootstrap-membership-а; transport
// заполняет caller-ом из claims).
type GrantOperatorInput struct {
	RoleName  string
	AID       string
	CallerAID *string
}

// GrantOperator привязывает AID к роли — вставляет membership-строку
// (RoleName, AID) с granted_by_aid = CallerAID. Фасад поверх пакетной
// [GrantOperator] (repository.go), симметричный [Service.RevokeOperator].
//
// Порядок в tx (детерминированный lock-порядок против deadlock — R2:
// роль → operators; тот же, что у RevokeOperator):
//  1. lock строки роли (SELECT … FOR UPDATE); нет → [ErrRoleNotFound].
//  2. INSERT membership-а; FK-violation на несуществующий AID →
//     [ErrOperatorNotFound] (через [mapGrantError]).
//
// self-lockout-проверки НЕТ: grant только добавляет membership (в т.ч.
// admin-а) — расширение admin-set-а кластер запереть не может. Это
// единственная мутация membership-а без lockout-границы (revoke/delete/
// update её требуют).
//
// Идемпотентно: повторный grant той же пары — no-op (ON CONFLICT DO NOTHING
// в insertRoleOperatorSQL).
func (s *Service) GrantOperator(ctx context.Context, in GrantOperatorInput) error {
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return fmt.Errorf("rbac: begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// Lock роли существование-границей: даёт чистый ErrRoleNotFound вместо
	// FK-violation на role_name (тот неотличим от FK на aid в одном маппере).
	if _, err := lockRole(ctx, tx, in.RoleName); err != nil {
		return err
	}

	// Least-privilege subset-check: нельзя грантить роль, содержащую
	// permission вне набора caller-а — иначе обход (cluster-admin создал
	// мощную роль, suboperator с role.grant-operator привязал её себе/другим
	// и поднялся). Проверяются permissions ГРАНЯЩЕЙСЯ роли.
	//
	// CallerAID == nil — системный/bootstrap-грант (keeper init привязывает
	// первого Архонта к cluster-admin внутри advisory-lock-tx): least-privilege
	// к нему не применяется (нет caller-Архонта как субъекта). Subject-грант из
	// transport-а всегда несёт CallerAID (claims.Subject).
	if in.CallerAID != nil {
		grantedPerms, err := rolePermissions(ctx, tx, in.RoleName)
		if err != nil {
			return err
		}
		// bare-perms гранящейся роли наследуют её default_scope (ADR-047 S1):
		// привязка scoped-роли конферит право в её scope, не unrestricted.
		grantedScope, err := roleDefaultScope(ctx, tx, in.RoleName)
		if err != nil {
			return err
		}
		required, err := requiredPermissions(grantedPerms, grantedScope)
		if err != nil {
			return err
		}
		if err := s.assertCallerMayGrant(ctx, tx, *in.CallerAID, required); err != nil {
			return err
		}
	}

	if _, err := tx.Exec(ctx, insertRoleOperatorSQL, in.RoleName, in.AID, grantedByArg(in.CallerAID)); err != nil {
		return mapGrantError(err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("rbac: commit tx: %w", err)
	}
	s.invalidate(ctx)
	return nil
}

// grantedByArg переводит *string CallerAID в args-значение для
// granted_by_aid: nil → nil (PG NULL), иначе разыменованная строка.
func grantedByArg(callerAID *string) any {
	if callerAID == nil {
		return nil
	}
	return *callerAID
}

// ListRoles возвращает API-каталог ролей (имя / description / builtin +
// развёрнутые permissions и operators-AID-ы). Read-only, без tx — собирается
// тремя SELECT-ами без N+1 ([LoadRoleViews], симметрично [LoadSnapshot]).
// Это API-view, не enforcer-snapshot: с description/builtin для role.list.
func (s *Service) ListRoles(ctx context.Context) ([]RoleView, error) {
	return LoadRoleViews(ctx, s.pool)
}

// assertNotLastWildcardRole — self-lockout для delete/update→remove-`*`:
// взять lock на effective-cluster-admins (ядро, БД + FOR UPDATE) и проверить,
// что останется ≥1 активный AID с `*` через РОЛЬ ≠ excludeRole. Пусто →
// [ErrWouldLockOutCluster].
//
// excludeRole мутируется/удаляется в этой же tx — её вклад в эффективный `*`
// исчезнет. Поэтому считаем «выживших» через ДРУГИЕ роли. Для точности
// используем per-role membership (нельзя просто исключить роль из общего
// списка AID-ов: AID мог держать `*` и через excludeRole, и через другую).
func (s *Service) assertNotLastWildcardRole(ctx context.Context, tx ExecQueryRower, excludeRole string) error {
	survivors, err := s.lockWildcardAdminsExcludingRole(ctx, tx, excludeRole)
	if err != nil {
		return err
	}
	if len(survivors) == 0 {
		return ErrWouldLockOutCluster
	}
	return nil
}

// assertNotLastWildcardOperator — self-lockout для revoke-operator: взять lock
// на effective-cluster-admins и проверить, что останется ≥1 активный AID с `*`
// ПОСЛЕ исключения ровно пары (excludeRole, excludeAID). Пусто →
// [ErrWouldLockOutCluster].
//
// Исключается только вклад пары: если excludeAID держит `*` ещё и через другую
// роль — он остаётся; если другой AID держит `*` через excludeRole — он
// остаётся (снимается лишь membership excludeAID, не вся роль).
func (s *Service) assertNotLastWildcardOperator(ctx context.Context, tx ExecQueryRower, excludeRole, excludeAID string) error {
	survivors, err := s.lockWildcardAdminsExcludingPair(ctx, tx, excludeRole, excludeAID)
	if err != nil {
		return err
	}
	if len(survivors) == 0 {
		return ErrWouldLockOutCluster
	}
	return nil
}

// lockWildcardAdminsExcludingRole берёт lock на self-lockout-ядро (FOR UPDATE) и
// возвращает активных AID-ов с эффективным `*` через роль ≠ excludeRole —
// учитывая ОБА пути (прямой ∪ через Synod, ADR-049(f)). excludeRole
// удаляется/теряет `*` в этой же tx, поэтому её вклад исключается из обеих
// веток: прямой `ro.role_name <> $1` и Synod `sr.role_name <> $1` (роль,
// бандленная в группу, тоже перестаёт давать `*`). Без Synod-ветки админ,
// держащий `*` ТОЛЬКО через группу, был бы посчитан «несуществующим» → ложный
// lockout ИЛИ (хуже) ложный пропуск удаления последней `*`-роли.
//
// Два locking-запроса в фиксированном порядке (прямой → Synod, см.
// [directClusterAdminsForUpdateSQL]) — UNION с FOR UPDATE PostgreSQL запрещает.
func (s *Service) lockWildcardAdminsExcludingRole(ctx context.Context, tx ExecQueryRower, excludeRole string) ([]string, error) {
	// Без DISTINCT: FOR UPDATE его запрещает (SQLSTATE 0A000). Для проверки
	// «пусто/непусто» дедуп не нужен; scanAIDs всё равно дедупит для чистоты.
	const directQ = `
SELECT ro.aid
FROM rbac_role_operators ro
JOIN rbac_role_permissions rp ON rp.role_name = ro.role_name
JOIN operators o ON o.aid = ro.aid
WHERE rp.permission = '*' AND o.revoked_at IS NULL AND ro.role_name <> $1
FOR UPDATE OF ro, rp, o
`
	const synodQ = `
SELECT so.aid
FROM synod_operators so
JOIN synod_roles sr ON sr.synod_name = so.synod_name
JOIN rbac_role_permissions rp ON rp.role_name = sr.role_name
JOIN operators o ON o.aid = so.aid
WHERE rp.permission = '*' AND o.revoked_at IS NULL AND sr.role_name <> $1
FOR UPDATE OF so, sr, rp, o
`
	direct, err := scanAIDs(ctx, tx, directQ, excludeRole)
	if err != nil {
		return nil, err
	}
	synod, err := scanAIDs(ctx, tx, synodQ, excludeRole)
	if err != nil {
		return nil, err
	}
	return dedupAIDs(append(direct, synod...)), nil
}

// lockWildcardAdminsExcludingPair берёт lock на self-lockout-ядро и возвращает
// активных AID-ов с эффективным `*` ПОСЛЕ исключения ровно одной ПРЯМОЙ
// membership-строки (excludeRole, excludeAID) — путь `role.revoke-operator`
// снимает именно её из rbac_role_operators.
//
// Synod-ветка (ADR-049(f)) НЕ фильтруется по паре: revoke-operator НЕ трогает
// synod_operators/synod_roles — если excludeAID держит `*` через Synod, он
// остаётся admin-ом и после снятия прямой строки. Это семантически верно: убран
// один путь, групповой путь жив. (Зеркально: другой AID, держащий `*` через
// excludeRole напрямую ИЛИ через Synod с этой ролью, тоже остаётся — снимается
// лишь membership excludeAID, не сама роль.)
//
// Прямая ветка исключает пару: `NOT (role_name=$1 AND aid=$2)`. Два locking-
// запроса в фиксированном порядке (прямой → Synod, см.
// [directClusterAdminsForUpdateSQL]).
func (s *Service) lockWildcardAdminsExcludingPair(ctx context.Context, tx ExecQueryRower, excludeRole, excludeAID string) ([]string, error) {
	// Без DISTINCT (см. lockWildcardAdminsExcludingRole).
	const directQ = `
SELECT ro.aid
FROM rbac_role_operators ro
JOIN rbac_role_permissions rp ON rp.role_name = ro.role_name
JOIN operators o ON o.aid = ro.aid
WHERE rp.permission = '*' AND o.revoked_at IS NULL
  AND NOT (ro.role_name = $1 AND ro.aid = $2)
FOR UPDATE OF ro, rp, o
`
	const synodQ = `
SELECT so.aid
FROM synod_operators so
JOIN synod_roles sr ON sr.synod_name = so.synod_name
JOIN rbac_role_permissions rp ON rp.role_name = sr.role_name
JOIN operators o ON o.aid = so.aid
WHERE rp.permission = '*' AND o.revoked_at IS NULL
FOR UPDATE OF so, sr, rp, o
`
	direct, err := scanAIDs(ctx, tx, directQ, excludeRole, excludeAID)
	if err != nil {
		return nil, err
	}
	synod, err := scanAIDs(ctx, tx, synodQ)
	if err != nil {
		return nil, err
	}
	return dedupAIDs(append(direct, synod...)), nil
}

// assertNotLastWildcardSynod — self-lockout для synod.delete: взять lock на
// effective-cluster-admins и проверить, что останется ≥1 активный AID с `*`
// ПОСЛЕ исчезновения всей группы excludeSynod. Пусто → [ErrWouldLockOutCluster].
//
// Группа удаляется CASCADE-ом в этой же tx → её bundle-роли перестают давать `*`
// ВСЕМ её членам. Поэтому Synod-ветка контрольного запроса исключает строки
// этой группы (`so.synod_name <> excludeSynod`); прямая ветка не трогается
// (delete группы не снимает прямой membership). Выжившие — админы через ДРУГИЕ
// группы ИЛИ напрямую.
func (s *Service) assertNotLastWildcardSynod(ctx context.Context, tx ExecQueryRower, excludeSynod string) error {
	survivors, err := s.lockWildcardAdminsExcludingSynod(ctx, tx, excludeSynod)
	if err != nil {
		return err
	}
	if len(survivors) == 0 {
		return ErrWouldLockOutCluster
	}
	return nil
}

// assertNotLastWildcardSynodRole — self-lockout для synod.revoke-role: взять
// lock на effective-cluster-admins и проверить, что останется ≥1 активный AID с
// `*` ПОСЛЕ снятия роли excludeRole из bundle группы excludeSynod. Пусто →
// [ErrWouldLockOutCluster].
//
// Роль уходит из bundle ровно ЭТОЙ группы (synod_roles-строка удаляется в этой
// же tx) → перестаёт давать `*` через excludeSynod, но через другие группы / тот
// же AID напрямую — остаётся. Synod-ветка исключает пару (excludeSynod,
// excludeRole); прямая ветка цела.
func (s *Service) assertNotLastWildcardSynodRole(ctx context.Context, tx ExecQueryRower, excludeSynod, excludeRole string) error {
	survivors, err := s.lockWildcardAdminsExcludingSynodRole(ctx, tx, excludeSynod, excludeRole)
	if err != nil {
		return err
	}
	if len(survivors) == 0 {
		return ErrWouldLockOutCluster
	}
	return nil
}

// assertNotLastWildcardSynodOperator — self-lockout для synod.remove-operator:
// взять lock на effective-cluster-admins и проверить, что останется ≥1 активный
// AID с `*` ПОСЛЕ исключения ровно пары (excludeSynod, excludeAID) из membership-а
// группы. Пусто → [ErrWouldLockOutCluster].
//
// Снимается одна synod_operators-строка → excludeAID теряет роли excludeSynod, но
// если он держит `*` через ДРУГУЮ группу ИЛИ напрямую — остаётся; другие члены
// excludeSynod не затрагиваются. Synod-ветка исключает пару (excludeSynod,
// excludeAID); прямая ветка цела (excludeAID мог держать `*` напрямую — тогда он
// остаётся admin-ом).
func (s *Service) assertNotLastWildcardSynodOperator(ctx context.Context, tx ExecQueryRower, excludeSynod, excludeAID string) error {
	survivors, err := s.lockWildcardAdminsExcludingSynodOperator(ctx, tx, excludeSynod, excludeAID)
	if err != nil {
		return err
	}
	if len(survivors) == 0 {
		return ErrWouldLockOutCluster
	}
	return nil
}

// lockWildcardAdminsExcludingSynod — admin-set с `*` после исчезновения всей
// группы excludeSynod (synod.delete). Прямая ветка цела; Synod-ветка исключает
// строки группы (`so.synod_name <> $1`). Два locking-запроса в фиксированном
// порядке (прямой → Synod, см. [directClusterAdminsForUpdateSQL]).
func (s *Service) lockWildcardAdminsExcludingSynod(ctx context.Context, tx ExecQueryRower, excludeSynod string) ([]string, error) {
	const synodQ = `
SELECT so.aid
FROM synod_operators so
JOIN synod_roles sr ON sr.synod_name = so.synod_name
JOIN rbac_role_permissions rp ON rp.role_name = sr.role_name
JOIN operators o ON o.aid = so.aid
WHERE rp.permission = '*' AND o.revoked_at IS NULL AND so.synod_name <> $1
FOR UPDATE OF so, sr, rp, o
`
	direct, err := scanAIDs(ctx, tx, directClusterAdminsForUpdateSQL)
	if err != nil {
		return nil, err
	}
	synod, err := scanAIDs(ctx, tx, synodQ, excludeSynod)
	if err != nil {
		return nil, err
	}
	return dedupAIDs(append(direct, synod...)), nil
}

// lockWildcardAdminsExcludingSynodRole — admin-set с `*` после снятия роли
// excludeRole из bundle группы excludeSynod (synod.revoke-role). Прямая ветка
// цела; Synod-ветка исключает пару (`NOT (sr.synod_name=$1 AND sr.role_name=$2)`).
func (s *Service) lockWildcardAdminsExcludingSynodRole(ctx context.Context, tx ExecQueryRower, excludeSynod, excludeRole string) ([]string, error) {
	const synodQ = `
SELECT so.aid
FROM synod_operators so
JOIN synod_roles sr ON sr.synod_name = so.synod_name
JOIN rbac_role_permissions rp ON rp.role_name = sr.role_name
JOIN operators o ON o.aid = so.aid
WHERE rp.permission = '*' AND o.revoked_at IS NULL
  AND NOT (sr.synod_name = $1 AND sr.role_name = $2)
FOR UPDATE OF so, sr, rp, o
`
	direct, err := scanAIDs(ctx, tx, directClusterAdminsForUpdateSQL)
	if err != nil {
		return nil, err
	}
	synod, err := scanAIDs(ctx, tx, synodQ, excludeSynod, excludeRole)
	if err != nil {
		return nil, err
	}
	return dedupAIDs(append(direct, synod...)), nil
}

// lockWildcardAdminsExcludingSynodOperator — admin-set с `*` после исключения
// пары (excludeSynod, excludeAID) из membership-а группы (synod.remove-operator).
// Прямая ветка цела; Synod-ветка исключает пару
// (`NOT (so.synod_name=$1 AND so.aid=$2)`).
func (s *Service) lockWildcardAdminsExcludingSynodOperator(ctx context.Context, tx ExecQueryRower, excludeSynod, excludeAID string) ([]string, error) {
	const synodQ = `
SELECT so.aid
FROM synod_operators so
JOIN synod_roles sr ON sr.synod_name = so.synod_name
JOIN rbac_role_permissions rp ON rp.role_name = sr.role_name
JOIN operators o ON o.aid = so.aid
WHERE rp.permission = '*' AND o.revoked_at IS NULL
  AND NOT (so.synod_name = $1 AND so.aid = $2)
FOR UPDATE OF so, sr, rp, o
`
	direct, err := scanAIDs(ctx, tx, directClusterAdminsForUpdateSQL)
	if err != nil {
		return nil, err
	}
	synod, err := scanAIDs(ctx, tx, synodQ, excludeSynod, excludeAID)
	if err != nil {
		return nil, err
	}
	return dedupAIDs(append(direct, synod...)), nil
}

// scanAIDs — общий сбор одностолбцового AID-результата self-lockout-запросов.
func scanAIDs(ctx context.Context, tx ExecQueryRower, sql string, args ...any) ([]string, error) {
	rows, err := tx.Query(ctx, sql, args...)
	if err != nil {
		return nil, fmt.Errorf("rbac: self-lockout probe: %w", wrapPgErr(err))
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var aid string
		if err := rows.Scan(&aid); err != nil {
			return nil, fmt.Errorf("rbac: self-lockout scan: %w", err)
		}
		out = append(out, aid)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("rbac: self-lockout iter: %w", err)
	}
	return dedupAIDs(out), nil
}
