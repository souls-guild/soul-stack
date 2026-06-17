package operator

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/souls-guild/soul-stack/keeper/internal/rbac"
)

// ErrWouldLockOutCluster — попытка ревокнуть последнего активного
// cluster-admin-а. Sentinel выделен для маппинга HTTP/MCP-сторонами в
// `would-lock-out-cluster` (409 / MCP-error code) без шарения strings.
//
// rbac.md → § Инвариант self-lockout: «нельзя ревокнуть последнего AID с
// активным wildcard `*`-permission».
var ErrWouldLockOutCluster = errors.New("operator: would lock out cluster (target is the last active cluster-admin)")

// ServiceDeps — зависимости [Service]. Все поля immutable после конструктора.
//
// Pool — узкий ExecQueryRower + BeginTx (для атомарной Revoke).
// Issuer — выпуск JWT.
// RBAC — read-only поверхность для RolesOf (выпуск токена с current ролями
// из БД-снимка RBAC, ADR-028). Lockout-probe для Revoke с ClusterAdmins()-снимка НЕ зависит
// (Slice 3): admin-set берётся из БД под FOR UPDATE (rbac.LockEffectiveClusterAdmins).
// TTLDefault — TTL JWT, выпускаемых Create/IssueToken.
type ServiceDeps struct {
	Pool       ServicePool
	Issuer     JWTIssuer
	RBAC       RBACSource
	TTLDefault time.Duration
	Logger     *slog.Logger
}

// ServicePool — узкое подмножество pgxpool.Pool, нужное service-у.
// Реальный `*pgxpool.Pool` удовлетворяет автоматически.
type ServicePool interface {
	ExecQueryRower
	BeginTx(ctx context.Context, opts pgx.TxOptions) (pgx.Tx, error)
}

// JWTIssuer — узкая поверхность над `*keeper/internal/jwt.Issuer`,
// нужная service-у. Совпадает с поверхностью HTTP-handler-а (handlers.JWTIssuer);
// объявляется здесь, чтобы service не тянул handlers-пакет.
type JWTIssuer interface {
	Issue(aid string, roles []string, ttl time.Duration, bootstrapInitial bool) (string, error)
}

// RBACSource — read-only поверхность над RBAC enforcer-ом. Реализуется и
// [rbac.Enforcer], и [rbac.Holder] (с hot-reload). Нужен только RolesOf
// (выпуск JWT с current-ролями из БД-снимка RBAC, ADR-028): lockout-probe
// берёт admin-set из БД под FOR UPDATE (rbac.LockEffectiveClusterAdmins,
// Slice 3), не из снимка.
type RBACSource interface {
	RolesOf(aid string) []string
}

// Invalidator — поверхность cluster-wide RBAC-инвалидации (ADR-014 Amendment
// 2026-05-27, JWT immediate revoke). После успешного commit-а Revoke
// [Service] вызывает Invalidate, чтобы остальные Keeper-ноды near-instant
// перечитали RBAC-снимок (и подхватили `operators.revoked_at` свежей строки)
// вместо ожидания TTL-poll-а. Реализуется в `keeper run` адаптером поверх
// [keeperredis.PublishRBACInvalidate] — тот же топик `rbac:invalidate`, что
// и role-мутации (ADR-028(d)).
//
// Контракт совпадает с [rbac.Invalidator]; локальное определение нужно, чтобы
// пакет operator не тянул rbac.Service-зависимость. Best-effort: ошибку
// публикации НЕ возвращает (Revoke уже зафиксирован в БД), реализация
// логирует и глотает.
type Invalidator interface {
	Invalidate(ctx context.Context)
}

// Service — бизнес-логика Operator-CRUD под Operator API (ADR-014) +
// MCP-tools (M0.7). Один источник правды для HTTP-handler-ов и
// MCP-tool-handler-ов; transport-фасад (HTTP/MCP) только декодирует input
// и кодирует output, бизнес-инварианты живут здесь.
//
// Безопасен для конкурентного использования: deps immutable, состояние не
// держится; cluster-wide-инвалидация — через atomic-late-binding
// [SetInvalidator].
type Service struct {
	pool       ServicePool
	issuer     JWTIssuer
	rbac       RBACSource
	ttlDefault time.Duration
	logger     *slog.Logger

	// inv — опциональный cluster-wide invalidator (ADR-014 Amendment
	// 2026-05-27). Late-binding через [Service.SetInvalidator]: Redis-клиент
	// в `keeper run` поднимается ПОСЛЕ NewService, поэтому инъекция отложена
	// (паттерн rbac.Service.SetInvalidator / serviceregistry.Service.SetInvalidator).
	// atomic.Pointer — конкурентная запись сеттером vs. чтение из Revoke без
	// отдельного mutex-а.
	inv atomic.Pointer[Invalidator]
}

// NewService собирает service. nil-логгер допустим — caller, не пишущий
// логи (MCP), просто передаёт нулевой; внутри будем no-op-ить через nil-check
// (slog v0.0+: nil-logger panic-ит, поэтому подменяем на discard в caller-е).
func NewService(d ServiceDeps) (*Service, error) {
	if d.Pool == nil {
		return nil, errors.New("operator: ServiceDeps.Pool is nil")
	}
	if d.Issuer == nil {
		return nil, errors.New("operator: ServiceDeps.Issuer is nil")
	}
	if d.RBAC == nil {
		return nil, errors.New("operator: ServiceDeps.RBAC is nil")
	}
	if d.TTLDefault <= 0 {
		return nil, errors.New("operator: ServiceDeps.TTLDefault must be positive")
	}
	return &Service{
		pool:       d.Pool,
		issuer:     d.Issuer,
		rbac:       d.RBAC,
		ttlDefault: d.TTLDefault,
		logger:     d.Logger,
	}, nil
}

// SetInvalidator late-binding-ом подключает cluster-wide invalidator (ADR-014
// Amendment 2026-05-27). Вызывается из `keeper run` после подъёма Redis-
// клиента. nil — снять invalidator (вернуться к чистому TTL-poll-у).
// Идемпотентен, потокобезопасен.
func (s *Service) SetInvalidator(inv Invalidator) {
	if inv == nil {
		s.inv.Store(nil)
		return
	}
	s.inv.Store(&inv)
}

// invalidate шлёт cluster-wide invalidate-сигнал после успешного commit-а
// Revoke (ADR-014 Amendment 2026-05-27). No-op, если invalidator не подключён
// (single-Keeper/dev). Best-effort: реализация Invalidate сама логирует и
// глотает ошибку publish-а — Revoke уже зафиксирован, потеря сигнала
// компенсируется TTL-poll-ом ([rbac.DefaultRefreshInterval]).
func (s *Service) invalidate(ctx context.Context) {
	if p := s.inv.Load(); p != nil {
		(*p).Invalidate(ctx)
	}
}

// CreateInput — параметры Create. Поля валидируются service-ом: AID по
// AIDPattern, DisplayName — non-empty (default = AID, если пуст), CallerAID —
// non-empty (контракт transport-а: middleware гарантирует non-empty Subject).
//
// Roles — опциональный список ролей, которые надо приатачить новому Архонту
// в той же транзакции, что INSERT operator-а. Любая ошибка (несуществующая
// роль, FK-violation, дубль grant) → rollback, оператор НЕ создаётся
// (atomic create+grant, UX-fix: оператор делает один API-вызов вместо
// двух-трёх). Пустой/nil — старый path без tx (backward-compat).
type CreateInput struct {
	AID         string
	DisplayName string
	CallerAID   string
	Roles       []string
}

// CreateResult — результат Create. JWT и ExpiresAt — выпущенный токен, который
// возвращается caller-у ровно один раз (нет re-issue path-а без явного
// issue-token-вызова).
//
// GrantedRoles — детерминированный (порядок входа) список ролей, прикреплённых
// в той же транзакции, что INSERT (CreateInput.Roles). Пустой/nil, если caller
// не передал Roles. JWT в этом ответе выпускается ПОСЛЕ commit-а tx — токен
// уже несёт granted-роли (RolesOf после commit видит свежий membership).
type CreateResult struct {
	AID          string
	DisplayName  string
	AuthMethod   AuthMethod
	CreatedAt    time.Time
	CreatedByAID string
	GrantedRoles []string
	JWT          string
	ExpiresAt    time.Time
}

// Create вставляет нового Архонта и выпускает JWT.
//
// Возврат sentinel-ошибок (transport маппит в HTTP 4xx / MCP-error code):
//   - [ErrOperatorAlreadyExists] — AID занят (409 / `operator-already-exists`).
//   - fmt.Errorf("invalid AID …") — AID не проходит regex (422).
//   - JWT-issue-failure после успешного Insert — fmt.Errorf-обёртка, manual
//     reconciliation (operator в БД, audit_log пишет transport-сторона).
//
// Если in.Roles непуст — atomic create+grant (UX-fix): Insert operator-а
// и все GrantOperator-ы идут одной PostgreSQL-tx. Любая ошибка
// (несуществующая роль/AID, дубль) → rollback, оператор НЕ создаётся.
// Сентинелы [rbac.ErrRoleNotFound] / [rbac.ErrOperatorNotFound] прокидываются
// как есть для маппинга в transport-слое (422 / 404).
func (s *Service) Create(ctx context.Context, in CreateInput) (*CreateResult, error) {
	if !ValidAID(in.AID) {
		return nil, fmt.Errorf("operator: invalid AID %q (must match %s)", in.AID, AIDPattern)
	}
	if in.CallerAID == "" {
		return nil, errors.New("operator: CallerAID is empty (transport must populate)")
	}
	// Pre-валидация имён ролей до round-trip-а (better error, без tx-hold-а).
	// SQL CHECK rbac_roles_name_format всё равно поймает мусор, но прикладная
	// проверка даёт чистую validation-failed-ошибку, а не FK/CHECK-violation.
	for _, r := range in.Roles {
		if !rbac.ValidRoleName(r) {
			return nil, fmt.Errorf("operator: invalid role name %q (must match %s)", r, rbac.RoleNamePattern)
		}
	}

	displayName := in.DisplayName
	if displayName == "" {
		displayName = in.AID
	}

	creator := in.CallerAID
	op := &Operator{
		AID:          in.AID,
		DisplayName:  displayName,
		AuthMethod:   AuthMethodJWT,
		CreatedByAID: &creator,
	}

	grantedRoles, err := s.insertWithOptionalRoles(ctx, op, in.Roles)
	if err != nil {
		return nil, err
	}

	// Cluster-wide RBAC-инвалидация после atomic create+grant (parity с
	// rbac.Service.GrantOperator): остальные Keeper-ноды near-instant
	// перечитают снимок RBAC и подхватят новый membership. No-op без ролей
	// (без membership-изменений снимок RBAC не меняется — TTL-poll увидит
	// нового AID самостоятельно через `loadRevoked` / нет, через обычное
	// чтение operators не нужно). Зовём только когда были grants — иначе
	// лишний трафик publish-а.
	if len(grantedRoles) > 0 {
		s.invalidate(ctx)
	}

	// JWT выпускается ПОСЛЕ commit-а tx — RolesOf видит свежий membership
	// (с задержкой TTL-poll, но invalidate выше ускоряет распространение).
	// Берём текущий снимок: при atomic create+grant локальный enforcer
	// первого Keeper-а уже подхватит обновление при следующем reload (best-
	// effort; токен всё равно несёт roles только для информации, authz
	// проверяется по свежему БД-снимку при каждом запросе — ADR-028).
	roles := s.rbac.RolesOf(in.AID)
	expiresAt := time.Now().UTC().Add(s.ttlDefault)
	token, err := s.issuer.Issue(in.AID, roles, s.ttlDefault, false)
	if err != nil {
		// Insert уже committed. Caller (transport-сторона) логирует
		// «manual reconciliation may be needed». Сам service логирует
		// тоже, чтобы трасса была независимой от transport-а.
		if s.logger != nil {
			s.logger.Error("operator.Create: issue JWT failed AFTER insert committed; manual reconciliation may be needed",
				slog.String("aid", in.AID),
				slog.String("by_aid", in.CallerAID),
				slog.Any("error", err),
			)
		}
		return nil, fmt.Errorf("operator: issue JWT failed: %w", err)
	}

	// SelectByAID после Insert — чтобы вернуть created_at из БД
	// (DEFAULT NOW()), а не локальное «сейчас». При ошибке Select-а не
	// проваливаем Create — Insert + JWT уже успешны; падаём back на
	// локальное время. Симметрично HTTP-handler-у (M0.6b).
	saved, err := SelectByAID(ctx, s.pool, in.AID)
	if err != nil {
		if s.logger != nil {
			s.logger.Warn("operator.Create: post-insert SelectByAID failed",
				slog.String("aid", in.AID),
				slog.String("by_aid", in.CallerAID),
				slog.Any("error", err),
			)
		}
		saved = op
		saved.CreatedAt = time.Now().UTC()
	}

	return &CreateResult{
		AID:          saved.AID,
		DisplayName:  saved.DisplayName,
		AuthMethod:   saved.AuthMethod,
		CreatedAt:    saved.CreatedAt,
		CreatedByAID: creator,
		GrantedRoles: grantedRoles,
		JWT:          token,
		ExpiresAt:    expiresAt,
	}, nil
}

// insertWithOptionalRoles — внутренний путь Create-а. Если roles пуст —
// fast-path без tx (Insert через s.pool, нулевой риск регрессии для
// существующих вызовов без Roles). Иначе — atomic-tx: BeginTx → Insert →
// rbac.GrantOperator x N → Commit; любая ошибка → rollback, оператор не
// создаётся.
//
// Возвращает actually-granted-роли (порядок входа). Сентинел-ошибки
// (ErrOperatorAlreadyExists / rbac.ErrRoleNotFound / rbac.ErrOperatorNotFound)
// прокидываются как есть для маппинга в transport-слое.
func (s *Service) insertWithOptionalRoles(ctx context.Context, op *Operator, roles []string) ([]string, error) {
	if len(roles) == 0 {
		if err := Insert(ctx, s.pool, op); err != nil {
			return nil, err
		}
		return nil, nil
	}

	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return nil, fmt.Errorf("operator: begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if err := Insert(ctx, tx, op); err != nil {
		return nil, err
	}

	// GrantOperator идёт repository-функцией (rbac.GrantOperator), а не
	// rbac.Service.GrantOperator-фасадом: последний открывает свою tx и
	// делает least-privilege-check, что нам не нужно (Create — privileged
	// path `operator.create`-permission; оператор, который смог создать
	// Архонта, может и атачить ему роли).
	//
	// granted_by_aid = CallerAID — отслеживаем инициатора в audit-trail.
	creator := op.CreatedByAID
	for _, role := range roles {
		if err := rbac.GrantOperator(ctx, tx, role, op.AID, creator); err != nil {
			return nil, mapGrantRoleError(err, role)
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("operator: commit tx: %w", err)
	}
	return append([]string(nil), roles...), nil
}

// mapGrantRoleError превращает rbac-repository-ошибку GrantOperator в
// прикладные sentinel-ы: FK-violation на role_name → [rbac.ErrRoleNotFound],
// FK на aid → [rbac.ErrOperatorNotFound] (последний здесь невозможен —
// operator только что вставлен в той же tx, но защищаемся явно для
// диагностики). repository-функция уже маппит через [rbac.wrapPgErr], но
// конкретный sentinel `role-not-found` не выделяет; матчим SQLSTATE 23503
// и имя constraint-а в тексте сообщения.
//
// role-параметр идёт в сообщение для UX («роль X не найдена»), это не
// машинно-парсимое поле — transport-слой берёт sentinel.
func mapGrantRoleError(err error, role string) error {
	if err == nil {
		return nil
	}
	// rbac.GrantOperator оборачивает оригинал через wrapPgErr — pg-код
	// доступен внутри. Дёргаем pgconn.PgError напрямую, как mapInsertError.
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == pgErrCodeForeignKeyViolation {
		// Constraint-name содержит ссылку на родительскую таблицу:
		// `rbac_role_operators_role_name_fkey` → rbac_roles (роль),
		// `rbac_role_operators_aid_fkey` → operators (AID). Различаем
		// по подстроке — имена constraint-ов фиксированы миграцией.
		if strings.Contains(pgErr.ConstraintName, "role_name") {
			return fmt.Errorf("%w: %q: %w", rbac.ErrRoleNotFound, role, err)
		}
		return fmt.Errorf("%w (constraint %s): %w", rbac.ErrOperatorNotFound, pgErr.ConstraintName, err)
	}
	return fmt.Errorf("operator: grant role %q: %w", role, err)
}

// RevokeInput — параметры Revoke.
type RevokeInput struct {
	AID       string
	Reason    string
	CallerAID string
}

// Revoke снимает active-флаг с оператора (RevokedAt=NOW), сохраняя reason
// в metadata.revoke_reason (если непустой).
//
// Атомарность (architect-verdict M0.6b §1): self-lockout probe и UPDATE
// идут в одной транзакции с SELECT … FOR UPDATE на admin-set. Это
// сериализует конкурентные revoke-вызовы.
//
// Возврат sentinel-ошибок:
//   - [ErrOperatorNotFound] — AID не существует (404).
//   - [ErrOperatorAlreadyRevoked] — уже ревокнут (409 / `operator-revoked`).
//   - [ErrWouldLockOutCluster] — target — единственный активный
//     cluster-admin (409 / `would-lock-out-cluster`).
//   - fmt.Errorf("invalid AID …") — AID не проходит regex (422).
//   - прочие — wrap pgx-ошибок (500).
func (s *Service) Revoke(ctx context.Context, in RevokeInput) error {
	if !ValidAID(in.AID) {
		return fmt.Errorf("operator: invalid AID %q (must match %s)", in.AID, AIDPattern)
	}

	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return fmt.Errorf("operator: begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// Источник admin-set — БД под FOR UPDATE (rbac.LockEffectiveClusterAdmins),
	// НЕ in-memory ClusterAdmins()-снимок (Slice 3, architect-verdict §2).
	// Развёрнутое «почему БД, а не снимок» (staleness-дыра + сериализация с
	// role-мутациями) — в [rbac.directClusterAdminsForUpdateSQL]; не дублируем.
	//
	// Lock-порядок (детерминированный, против deadlock с role-мутациями):
	// сначала ro/rp/o (FOR UPDATE OF в LockEffectiveClusterAdmins), затем
	// UPDATE operators-строки target в Revoke. Единое ядро = единый порядок.
	admins, err := rbac.LockEffectiveClusterAdmins(ctx, tx)
	if err != nil {
		return fmt.Errorf("operator: lock effective cluster-admins: %w", err)
	}

	// Инвариант: после ревокации target останется ≥1 активный оператор с
	// эффективным `*`. target после revoke перестаёт быть активным admin-ом
	// (revoked_at != NULL), поэтому исключаем его из admin-set фильтром по AID;
	// если target вообще не admin — фильтр ничего не меняет и lockout невозможен.
	survivors := excludeAID(admins, in.AID)
	if isInSet(admins, in.AID) && len(survivors) == 0 {
		return ErrWouldLockOutCluster
	}

	if err := Revoke(ctx, tx, in.AID, in.Reason); err != nil {
		// ErrOperatorNotFound / ErrOperatorAlreadyRevoked — прокидываем как
		// есть для маппинга в transport-слое.
		return err
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("operator: commit tx: %w", err)
	}
	// Cluster-wide RBAC-инвалидация (ADR-014 Amendment 2026-05-27): publish
	// `rbac:invalidate` после успешного commit-а — остальные Keeper-ноды
	// near-instant перечитают RBAC-снимок и подхватят `operators.revoked_at`
	// этой строки (Snapshot.Revoked). Best-effort, ошибку publish-а
	// проглатывает реализация Invalidate.
	s.invalidate(ctx)
	return nil
}

// IssueTokenInput — параметры IssueToken.
type IssueTokenInput struct {
	AID       string
	CallerAID string
}

// IssueTokenResult — выпущенный токен.
type IssueTokenResult struct {
	AID       string
	JWT       string
	ExpiresAt time.Time
}

// IssueToken выпускает новый JWT для существующего активного Архонта.
// Roles резолвятся из БД-снимка RBAC на момент выпуска (s.rbac.RolesOf,
// ADR-028). JWT-roles — informational и НЕ авторитетны для authz: при
// каждом запросе Keeper перепроверяет права по актуальному БД-снимку.
//
// Возврат:
//   - [ErrOperatorNotFound] — AID не существует (404).
//   - [ErrOperatorRevoked-sentinel] — оператор ревокнут (409).
//   - fmt.Errorf("invalid AID …") — AID не проходит regex (422).
//
// ErrOperatorRevoked здесь — re-use [ErrOperatorAlreadyRevoked] для маппинга
// в `operator-revoked`-error (transport).
func (s *Service) IssueToken(ctx context.Context, in IssueTokenInput) (*IssueTokenResult, error) {
	if !ValidAID(in.AID) {
		return nil, fmt.Errorf("operator: invalid AID %q (must match %s)", in.AID, AIDPattern)
	}
	op, err := SelectByAID(ctx, s.pool, in.AID)
	if err != nil {
		return nil, err
	}
	if op.IsRevoked() {
		return nil, ErrOperatorAlreadyRevoked
	}
	roles := s.rbac.RolesOf(in.AID)
	expiresAt := time.Now().UTC().Add(s.ttlDefault)
	token, err := s.issuer.Issue(in.AID, roles, s.ttlDefault, false)
	if err != nil {
		return nil, fmt.Errorf("operator: issue JWT failed: %w", err)
	}
	return &IssueTokenResult{
		AID:       in.AID,
		JWT:       token,
		ExpiresAt: expiresAt,
	}, nil
}

// List возвращает страницу Архонтов под фильтром. Тонкая обёртка над [List];
// service-слой нужен для симметрии с Create/Revoke/IssueToken (один источник
// правды для HTTP/MCP, M0.7 #6).
func (s *Service) List(ctx context.Context, f ListFilter, offset, limit int) ([]*Operator, int, error) {
	return List(ctx, s.pool, f, offset, limit)
}

// Get читает Архонта по AID. Тонкая обёртка над [SelectByAID]; sentinel-
// ошибки прокидываются как есть для маппинга в transport-слое.
func (s *Service) Get(ctx context.Context, aid string) (*Operator, error) {
	if !ValidAID(aid) {
		return nil, fmt.Errorf("operator: invalid AID %q (must match %s)", aid, AIDPattern)
	}
	return SelectByAID(ctx, s.pool, aid)
}

// isInSet — линейная проверка вхождения (admin-set десятки AID-ов).
func isInSet(set []string, target string) bool {
	for _, a := range set {
		if a == target {
			return true
		}
	}
	return false
}

// excludeAID возвращает копию set без вхождений target. Используется
// Revoke-lockout-probe: target после ревокации перестаёт быть активным
// admin-ом, оставшийся набор — «выжившие» эффективные `*`-admin-ы.
func excludeAID(set []string, target string) []string {
	out := make([]string, 0, len(set))
	for _, a := range set {
		if a != target {
			out = append(out, a)
		}
	}
	return out
}
