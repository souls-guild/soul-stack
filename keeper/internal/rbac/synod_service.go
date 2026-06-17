package rbac

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// CreateSynodInput — параметры CreateSynod.
type CreateSynodInput struct {
	Name        string
	Description string
	CallerAID   string
}

// CreateSynod создаёт пустую группу. Least-privilege/self-lockout к созданию
// неприменимы: пустая группа прав не выдаёт (роли в bundle добавляются отдельно
// через GrantRole под subset-check). Симметрия [Service.CreateRole].
//
// Возврат:
//   - [ErrInvalidSynodName] — name не по формату (422).
//   - [ErrSynodAlreadyExists] — name занят (409).
//   - wrapped FK-violation — CallerAID не существует в operators.
func (s *Service) CreateSynod(ctx context.Context, in CreateSynodInput) error {
	if !reRoleName.MatchString(in.Name) {
		return fmt.Errorf("%w: %q must match %s", ErrInvalidSynodName, in.Name, reRoleName.String())
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

	if err := CreateSynod(ctx, tx, in.Name, in.Description, createdBy); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("rbac: commit tx: %w", err)
	}
	s.invalidate(ctx)
	return nil
}

// DeleteSynod удаляет группу (каскадом membership + bundle).
//
// Порядок в tx (детерминированный lock-порядок: группа → её роли → admin-set):
//  1. lock строки группы; нет → [ErrSynodNotFound].
//  2. builtin=true → [ErrSynodBuiltin] (ПЕРВОЙ, до lockout — builtin важнее).
//  3. если группа бандлит `*`-дающую роль — self-lockout: исчезновение группы
//     не должно осиротить последнего админа, чей `*` держится через неё
//     (ADR-049(f)). Пусто → [ErrWouldLockOutCluster].
//  4. DELETE.
func (s *Service) DeleteSynod(ctx context.Context, name string) error {
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return fmt.Errorf("rbac: begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	builtin, err := lockSynod(ctx, tx, name)
	if err != nil {
		return err
	}
	if builtin {
		return ErrSynodBuiltin
	}

	// `*` через группу мог быть последним путём её членов к admin — проверяем
	// только если группа реально бандлит `*`-дающую роль (иначе lockout
	// невозможен, лишний запрос не нужен).
	wildcard, err := s.synodGivesWildcard(ctx, tx, name)
	if err != nil {
		return err
	}
	if wildcard {
		if err := s.assertNotLastWildcardSynod(ctx, tx, name); err != nil {
			return err
		}
	}

	if err := DeleteSynod(ctx, tx, name); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("rbac: commit tx: %w", err)
	}
	s.invalidate(ctx)
	return nil
}

// UpdateSynodDescription правит ТОЛЬКО description группы (ADR-049 amend). name
// (PK) immutable — переименование сознательно отвергнуто (audit-drift + окно
// рассинхрона enforcer-снимка + асимметрия с rbac_roles.name).
//
// Без tx/lock/subset/self-lockout: одна UPDATE-строка, description прав не
// выдаёт и не отнимает, поэтому self-lockout невозможен, а least-privilege
// неприменим. builtin РАЗРЕШЁН (description косметика, не поведение). invalidate
// НЕ дёргается: enforcer-снимок ([loadSynodMembership]/[loadSynodRoles]) несёт
// только name/роли/membership — description в матчинг не входит, авторизация от
// его правки не меняется.
//
// Возврат [ErrSynodNotFound] на 0 rows affected (группы нет).
func (s *Service) UpdateSynodDescription(ctx context.Context, name, description string) error {
	tag, err := s.pool.Exec(ctx, updateSynodDescriptionSQL, name, description)
	if err != nil {
		return fmt.Errorf("rbac: update synod %q description: %w", name, wrapPgErr(err))
	}
	if tag.RowsAffected() == 0 {
		return ErrSynodNotFound
	}
	return nil
}

// AddOperatorInput — параметры AddOperator.
type AddOperatorInput struct {
	SynodName string
	AID       string
	CallerAID string
}

// AddOperator добавляет архона в группу (synod_operators). Идемпотентно.
//
// SECURITY (ADR-049(f)): под least-privilege subset-check. Член группы получает
// ВЕСЬ её bundle ролей — caller не вправе ввести в группу архона, если сам не
// держит все эффективные права этого bundle (иначе обход: собрал группу с мощной
// ролью cluster-admin-ом, sub-оператор с synod.add-operator привязал себе/другим
// и поднялся). Проверяются эффективные permissions ВСЕХ ролей группы под их
// default_scope (как role.grant-operator проверяет права гранящейся роли).
//
// Порядок в tx (детерминированный: группа → admin-set/subset):
//  1. lock группы; нет → [ErrSynodNotFound].
//  2. subset-check по эффективным правам bundle группы → [ErrPermissionNotHeld].
//  3. INSERT membership-а; FK на несуществующий AID → [ErrOperatorNotFound].
//
// self-lockout НЕТ: add только расширяет admin-set (член получает роли) —
// запереть кластер не может (симметрия role.grant-operator).
func (s *Service) AddOperator(ctx context.Context, in AddOperatorInput) error {
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return fmt.Errorf("rbac: begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if _, err := lockSynod(ctx, tx, in.SynodName); err != nil {
		return err
	}

	// Least-privilege: эффективные права ВСЕХ ролей группы. CallerAID всегда
	// несёт subject (transport — claims.Subject); пустой caller с непустым
	// required отвергается в assertCallerMayGrant ([ErrPermissionNotHeld]).
	required, err := s.synodEffectivePermissions(ctx, tx, in.SynodName)
	if err != nil {
		return err
	}
	if err := s.assertCallerMayGrant(ctx, tx, in.CallerAID, required); err != nil {
		return err
	}

	if _, err := tx.Exec(ctx, insertSynodOperatorSQL, in.SynodName, in.AID, callerArg(in.CallerAID)); err != nil {
		return mapSynodMemberError(err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("rbac: commit tx: %w", err)
	}
	s.invalidate(ctx)
	return nil
}

// RemoveOperatorInput — параметры RemoveOperator.
type RemoveOperatorInput struct {
	SynodName string
	AID       string
}

// RemoveOperator убирает архона из группы (synod_operators).
//
// SECURITY (ADR-049(f)): self-lockout. Снятие архона из группы отнимает у него
// роли группы (в т.ч. `*`-дающую) — может осиротить последнего админа, чей `*`
// держится ТОЛЬКО через эту группу.
//
// Порядок в tx (детерминированный: membership → группа → admin-set):
//  1. lock membership-строки; нет → [ErrSynodOperatorNotFound].
//  2. если группа бандлит `*` — self-lockout, исключающий пару (synod, aid) из
//     Synod-ветки (excludeAID мог держать `*` ещё напрямую/через другую группу —
//     остаётся); пусто → [ErrWouldLockOutCluster].
//  3. DELETE.
func (s *Service) RemoveOperator(ctx context.Context, in RemoveOperatorInput) error {
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return fmt.Errorf("rbac: begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if err := lockSynodOperator(ctx, tx, in.SynodName, in.AID); err != nil {
		return err
	}
	if _, err := lockSynod(ctx, tx, in.SynodName); err != nil {
		return err
	}

	wildcard, err := s.synodGivesWildcard(ctx, tx, in.SynodName)
	if err != nil {
		return err
	}
	if wildcard {
		if err := s.assertNotLastWildcardSynodOperator(ctx, tx, in.SynodName, in.AID); err != nil {
			return err
		}
	}

	tag, err := tx.Exec(ctx, deleteSynodOperatorSQL, in.SynodName, in.AID)
	if err != nil {
		return fmt.Errorf("rbac: remove operator (%s -> %s): %w", in.SynodName, in.AID, wrapPgErr(err))
	}
	if tag.RowsAffected() == 0 {
		// Lock выше уже гарантировал существование пары; защита от гонки.
		return ErrSynodOperatorNotFound
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("rbac: commit tx: %w", err)
	}
	s.invalidate(ctx)
	return nil
}

// GrantRoleInput — параметры GrantRole.
type GrantRoleInput struct {
	SynodName string
	RoleName  string
	CallerAID string
}

// GrantRole добавляет роль в bundle группы (synod_roles). Идемпотентно.
//
// SECURITY (ADR-049(f)): под least-privilege subset-check. Добавление роли в
// группу выдаёт ВСЕМ её членам эффективные права этой роли — caller не вправе
// добавить роль, права которой выходят за его собственный набор (иначе обход:
// sub-оператор с synod.grant-role бандлит cluster-admin-роль в группу, членом
// которой состоит, и поднимается). Проверяются эффективные permissions
// гранящейся роли под её default_scope (как role.grant-operator).
//
// Порядок в tx (детерминированный: группа → роль → subset):
//  1. lock группы; нет → [ErrSynodNotFound].
//  2. subset-check по эффективным правам роли → [ErrPermissionNotHeld].
//  3. INSERT bundle-строки; FK на несуществующую роль → [ErrRoleNotFound].
//
// self-lockout НЕТ: grant только расширяет admin-set.
func (s *Service) GrantRole(ctx context.Context, in GrantRoleInput) error {
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return fmt.Errorf("rbac: begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if _, err := lockSynod(ctx, tx, in.SynodName); err != nil {
		return err
	}

	// Эффективные права гранящейся роли под её default_scope. Роль может не
	// существовать — тогда rolePermissions вернёт пустой набор, subset пройдёт
	// (нечего проверять), а INSERT упадёт FK-violation-ом → ErrRoleNotFound.
	// Порядок верный: несуществующая роль = 404, не ложный subset-pass с
	// выдачей прав (их нет).
	required, err := s.roleEffectivePermissions(ctx, tx, in.RoleName)
	if err != nil {
		return err
	}
	if err := s.assertCallerMayGrant(ctx, tx, in.CallerAID, required); err != nil {
		return err
	}

	if _, err := tx.Exec(ctx, insertSynodRoleSQL, in.SynodName, in.RoleName, callerArg(in.CallerAID)); err != nil {
		return mapSynodMemberError(err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("rbac: commit tx: %w", err)
	}
	s.invalidate(ctx)
	return nil
}

// RevokeRoleInput — параметры RevokeRole.
type RevokeRoleInput struct {
	SynodName string
	RoleName  string
}

// RevokeRole убирает роль из bundle группы (synod_roles).
//
// SECURITY (ADR-049(f)): self-lockout. Снятие роли из группы отнимает её права у
// ВСЕХ членов группы — если это была последняя `*`-дающая роль группы и кто-то из
// членов держал `*` ТОЛЬКО через неё, кластер залочится.
//
// Порядок в tx (детерминированный: bundle-строка → admin-set):
//  1. lock bundle-строки; нет → [ErrSynodRoleNotFound].
//  2. если снимаемая роль даёт `*` — self-lockout, исключающий пару
//     (synod, role) из Synod-ветки; пусто → [ErrWouldLockOutCluster].
//  3. DELETE.
func (s *Service) RevokeRole(ctx context.Context, in RevokeRoleInput) error {
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return fmt.Errorf("rbac: begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if err := lockSynodRole(ctx, tx, in.SynodName, in.RoleName); err != nil {
		return err
	}

	// Self-lockout нужна только если снимаемая роль даёт `*`: иначе её снятие
	// admin-set не уменьшает.
	perms, err := rolePermissions(ctx, tx, in.RoleName)
	if err != nil {
		return err
	}
	if roleGivesWildcard(perms) {
		if err := s.assertNotLastWildcardSynodRole(ctx, tx, in.SynodName, in.RoleName); err != nil {
			return err
		}
	}

	tag, err := tx.Exec(ctx, deleteSynodRoleSQL, in.SynodName, in.RoleName)
	if err != nil {
		return fmt.Errorf("rbac: revoke role (%s -> %s): %w", in.SynodName, in.RoleName, wrapPgErr(err))
	}
	if tag.RowsAffected() == 0 {
		return ErrSynodRoleNotFound
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("rbac: commit tx: %w", err)
	}
	s.invalidate(ctx)
	return nil
}

// ListSynods возвращает API-каталог групп (имя / description / builtin +
// развёрнутые роли и AID-члены). Read-only, без tx ([LoadSynodViews]).
func (s *Service) ListSynods(ctx context.Context) ([]SynodView, error) {
	return LoadSynodViews(ctx, s.pool)
}

// synodGivesWildcard — true, если хотя бы одна роль bundle группы даёт `*`.
// Self-lockout-пути спрашивают это, чтобы не гонять admin-set-probe впустую,
// когда группа `*` не бандлит (lockout невозможен).
func (s *Service) synodGivesWildcard(ctx context.Context, tx ExecQueryRower, name string) (bool, error) {
	rows, err := tx.Query(ctx, `
SELECT 1
FROM synod_roles sr
JOIN rbac_role_permissions rp ON rp.role_name = sr.role_name
WHERE sr.synod_name = $1 AND rp.permission = '*'
LIMIT 1`, name)
	if err != nil {
		return false, fmt.Errorf("rbac: probe synod %q wildcard: %w", name, wrapPgErr(err))
	}
	defer rows.Close()
	has := rows.Next()
	if err := rows.Err(); err != nil {
		return false, fmt.Errorf("rbac: iter synod %q wildcard: %w", name, err)
	}
	return has, nil
}

// synodEffectivePermissions — объединённые эффективные permissions ВСЕХ ролей
// группы (каждая bare-perm развёрнута под default_scope СВОЕЙ роли, ADR-047 S1).
// Нужен subset-check-у add-operator: член получает весь bundle, caller обязан
// держать всё. Дубль права из двух ролей идемпотентен (assertCallerCovers
// проверяет каждое отдельно).
func (s *Service) synodEffectivePermissions(ctx context.Context, tx ExecQueryRower, synodName string) ([]Permission, error) {
	roles, err := synodRoles(ctx, tx, synodName)
	if err != nil {
		return nil, err
	}
	var out []Permission
	for _, r := range roles {
		eff, err := s.roleEffectivePermissions(ctx, tx, r)
		if err != nil {
			return nil, err
		}
		out = append(out, eff...)
	}
	return out, nil
}

// roleEffectivePermissions — эффективные permissions роли (bare развёрнут под её
// default_scope). Общий хелпер subset-check-а grant-role / add-operator: ровно
// тот разворот, что [Service.GrantOperator] делает для гранящейся роли.
func (s *Service) roleEffectivePermissions(ctx context.Context, tx ExecQueryRower, roleName string) ([]Permission, error) {
	perms, err := rolePermissions(ctx, tx, roleName)
	if err != nil {
		return nil, err
	}
	scope, err := roleDefaultScope(ctx, tx, roleName)
	if err != nil {
		return nil, err
	}
	return requiredPermissions(perms, scope)
}

// callerArg переводит CallerAID-строку в args-значение added_by_aid/granted_by_aid:
// пустая → PG NULL (bootstrap/seed без инициатора-Архонта), иначе строка.
func callerArg(callerAID string) any {
	if callerAID == "" {
		return nil
	}
	return callerAID
}
