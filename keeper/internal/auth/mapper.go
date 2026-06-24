package auth

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"

	"github.com/jackc/pgx/v5"

	"github.com/souls-guild/soul-stack/keeper/internal/operator"
	"github.com/souls-guild/soul-stack/keeper/internal/rbac"
	"github.com/souls-guild/soul-stack/shared/audit"
)

// ProvisioningGate — узкая поверхность проверки политики provisioning_allowed_methods
// (ADR-058 Часть B). Реализуется *serviceregistry.Holder; объявлена интерфейсом,
// чтобы auth-пакет не тянул serviceregistry и тестировался с fake-гейтом.
// ProvisioningMethodAllowed("ldap") отвечает, разрешено ли СОЗДАВАТЬ federated-
// оператора (auto-provision). nil-gate трактуется вызывающим как «пропускать»
// (back-compat: политика не сконфигурирована).
type ProvisioningGate interface {
	ProvisioningMethodAllowed(method string) bool
}

// Txer — узкая фабрика транзакций (подмножество pgxpool.Pool), нужная для
// атомарной реконсиляции ролей. *pgxpool.Pool удовлетворяет автоматически.
// Объявлена интерфейсом, чтобы auth-пакет не тянул pgxpool в зависимости.
type Txer interface {
	BeginTx(ctx context.Context, opts pgx.TxOptions) (pgx.Tx, error)
}

// MapperConfig — зависимости [DBMapper].
type MapperConfig struct {
	// Method — федеративный метод этого mapper-а (ADR-058(a)): operator.AuthMethodLDAP
	// либо operator.AuthMethodOIDC. Определяет, какой `auth_method`/`created_via`
	// пишется в строку при auto-provision и какой метод проверяется в
	// ProvisioningGate. Один [DBMapper] обслуживает один метод (LDAP-mapper и
	// OIDC-mapper конструируются отдельно daemon-ом с одинаковой логикой). Пустое
	// значение → провижининг отвергается ([ErrAuthFailed], defense-in-depth: mapper
	// без явного метода не должен молча создавать оператора неизвестного источника).
	Method operator.AuthMethod

	// GroupRoleMap — внешняя группа → RBAC-роли (config auth.{ldap,oidc}.group_role_map).
	// Источник ролей federated-оператора (ADR-058(d), развилка №2: роли из групп).
	GroupRoleMap map[string][]string

	// DB — write/read-поверхность реестра operators + rbac_role_operators.
	// Реальный pgxpool.Pool удовлетворяет интерфейсу.
	DB operator.ExecQueryRower

	// Tx — фабрика транзакций для атомарной реконсиляции ролей (HIGH-1,
	// ADR-058(d)): grant новых + scoped-revoke ушедших выполняются в ОДНОЙ
	// транзакции, иначе сбой между grant и revoke оставил бы membership
	// рассогласованным. Реальный *pgxpool.Pool удовлетворяет (BeginTx).
	//
	// nil → fallback на не-транзакционный grant-only-путь поверх DB (back-compat
	// для unit-тестов с fake-DB без BeginTx): реконсиляция-revoke тогда не
	// выполняется, но существующий guard «grant из групп» сохраняется. daemon
	// всегда выставляет Tx (d.pool), поэтому в проде реконсиляция атомарна.
	Tx Txer

	// Audit — куда писать `operator.provisioned` (login пишет endpoint).
	Audit audit.Writer

	// ProvisioningGate — политика provisioning_allowed_methods (ADR-058 Часть B):
	// гейтит ТОЛЬКО ветку provision (auto-create нового federated-оператора).
	// nil → гейт выключен (политика не сконфигурирована, back-compat). Existing-
	// оператор (case err==nil в Map) гейт НЕ задействует — логинится независимо
	// от политики.
	ProvisioningGate ProvisioningGate

	// Logger — debug-трасса (без секретов).
	Logger *slog.Logger
}

// DBMapper отображает внешнюю identity (LDAP либо OIDC) на operators(aid) + роли
// (ADR-058(d)). Реализует [Mapper]. Логика для обоих методов одинакова; различает
// их только cfg.Method (записывается в auth_method/created_via, проверяется
// ProvisioningGate-ом).
//
// Решения стадии 1 (ADR-058):
//   - provisioning — auto-provision по группам (развилка №1): первый логин
//     создаёт оператора, ЕСЛИ есть пересечение групп с group_role_map; вне
//     групп — отказ ([ErrNoRoleMapping]), оператор НЕ создаётся;
//   - источник ролей — внешние группы (развилка №2): и для нового, и для
//     существующего оператора роли вычисляются из group_role_map, а не из
//     реестра. Membership синхронизируется в rbac_role_operators (авторитет
//     RBAC — таблица, не JWT-claim, ADR-028(c));
//   - revoked-инвариант: federated-login revoked-оператора запрещён
//     ([ErrOperatorRevoked]).
//
// audit `operator.provisioned` пишет DBMapper (факт создания строки);
// `operator.login` пишет endpoint (одно событие на успешный логин) — разделение,
// чтобы login-событие не задваивалось.
type DBMapper struct {
	cfg MapperConfig
	// managedRoles — объединение values(group_role_map): домен ролей, которым
	// УПРАВЛЯЕТ этот federated-mapper (HIGH-1, ADR-058(d)). Реконсиляция-revoke
	// трогает ТОЛЬКО роли из этого набора — роли, выданные Synod/вручную/иным
	// путём вне group_role_map, не снимаются. Считается один раз в NewMapper.
	managedRoles map[string]struct{}
}

// NewMapper конструирует DBMapper.
func NewMapper(cfg MapperConfig) *DBMapper {
	return &DBMapper{cfg: cfg, managedRoles: managedRoleSet(cfg.GroupRoleMap)}
}

// managedRoleSet собирает множество всех ролей, упомянутых в значениях
// group_role_map — домен, которым управляет federated-реконсиляция (HIGH-1).
func managedRoleSet(grm map[string][]string) map[string]struct{} {
	set := make(map[string]struct{})
	for _, roles := range grm {
		for _, role := range roles {
			set[role] = struct{}{}
		}
	}
	return set
}

// Map реализует [Mapper]: ext → MappedOperator либо sentinel-ошибка.
//
// AID берётся из ext.AID (Authenticator выводит его из cfg.AIDAttr, дефолт
// `uid`, см. ldap.Authenticator). Невалидный AID → [ErrAuthFailed]
// (anti-oracle: наружу не утекает причина).
func (m *DBMapper) Map(ctx context.Context, ext ExternalIdentity) (MappedOperator, error) {
	if m.cfg.Method == "" {
		// Defense-in-depth: mapper без явного метода не должен молча создавать
		// оператора неизвестного источника. daemon всегда выставляет Method.
		return MappedOperator{}, ErrAuthFailed
	}
	aid := ext.AID
	if !operator.ValidAID(aid) {
		if m.cfg.Logger != nil {
			m.cfg.Logger.Debug("auth/mapper: derived AID failed validation",
				slog.String("aid", aid))
		}
		return MappedOperator{}, ErrAuthFailed
	}

	roles := m.rolesForGroups(ext.Groups)
	if len(roles) == 0 {
		// Вне группового маппинга — отказ, оператор НЕ создаётся (ADR-058(d)).
		return MappedOperator{}, ErrNoRoleMapping
	}

	op, err := operator.SelectByAID(ctx, m.cfg.DB, aid)
	switch {
	case err == nil:
		if op.IsRevoked() {
			return MappedOperator{}, ErrOperatorRevoked
		}
		// CRIT-1 (account-takeover, ADR-058(d) revocation-инвариант усилен):
		// federated-путь обслуживает ТОЛЬКО операторов, заведённых ЭТИМ же
		// federated-методом. Если existing-оператор живёт под другим auth_method
		// (bootstrap/system `jwt`, mTLS, ИЛИ другой federated-метод), отказываем —
		// иначе любой, кто контролирует внешний IdP, мог бы выпустить себе валидную
		// derived-AID, совпавшую с AID привилегированного оператора (например,
		// bootstrap cluster-admin), и присвоить его сессию. ErrAuthFailed
		// (anti-oracle: наружу 401 без причины — не раскрываем, что AID существует
		// под другим методом). Bootstrap (`auth_method=jwt`) и `archon-system`/
		// system-операторы тем самым защищены автоматически: их auth_method ∉
		// {ldap,oidc}, federated-mapper их не примет.
		if op.AuthMethod != m.cfg.Method {
			if m.cfg.Logger != nil {
				m.cfg.Logger.Warn("auth/mapper: federated login rejected — AID belongs to a different auth_method",
					slog.String("aid", aid),
					slog.String("mapper_method", string(m.cfg.Method)))
			}
			return MappedOperator{}, ErrAuthFailed
		}
		// Существующий активный оператор того же метода: роли — из групп
		// (источник ролей = группы, развилка №2). Membership реконсилируем
		// (grant новых + scoped-revoke ушедших, HIGH-1), чтобы внешняя смена
		// групп отражалась в RBAC при следующем логине.
		if err := m.reconcileRoles(ctx, aid, roles); err != nil {
			return MappedOperator{}, err
		}
		return MappedOperator{AID: aid, Roles: roles, Provisioned: false}, nil

	case errors.Is(err, operator.ErrOperatorNotFound):
		// Auto-provision (развилка №1): юзер в группе → создаём оператора.
		return m.provision(ctx, aid, ext, roles)

	default:
		return MappedOperator{}, fmt.Errorf("auth/mapper: select operator: %w", err)
	}
}

// provision создаёт нового federated-оператора (auth_method=cfg.Method) + membership
// + audit `operator.provisioned`.
//
// created_via=string(cfg.Method) (ldap|oidc), created_by_aid=NULL (ADR-058(d)):
// federated-login инициирован внешним IdP, оператора-инициатора нет. NULL у
// created_by_aid теперь легален для не-bootstrap-строк — bootstrap-инвариант
// перенесён на created_via='bootstrap' (миграция 085), поэтому отдельный
// reserved-AID-маркер больше не нужен. Источник атрибутируется самим created_via.
func (m *DBMapper) provision(ctx context.Context, aid string, ext ExternalIdentity, roles []string) (MappedOperator, error) {
	method := string(m.cfg.Method)
	// Гейт политики provisioning_allowed_methods (ADR-058 Часть B): ТОЛЬКО на
	// создании. ДО Insert — оператор не должен появиться при запрещённом методе.
	// gate==nil → пропускаем (политика не сконфигурирована, back-compat).
	if m.cfg.ProvisioningGate != nil && !m.cfg.ProvisioningGate.ProvisioningMethodAllowed(method) {
		return MappedOperator{}, ErrProvisioningDisabled
	}

	displayName := ext.Username
	if displayName == "" {
		displayName = aid
	}
	// created_via — строка того же домена, что и auth_method (ldap|oidc); enum
	// CreatedVia — alias на string, значения совпадают (ADR-058(d)).
	op := &operator.Operator{
		AID:          aid,
		DisplayName:  displayName,
		AuthMethod:   m.cfg.Method,
		CreatedByAID: nil,
		CreatedVia:   method,
		Metadata:     map[string]any{"federated_source": method},
	}
	if err := operator.Insert(ctx, m.cfg.DB, op); err != nil {
		return MappedOperator{}, fmt.Errorf("auth/mapper: provision insert: %w", err)
	}
	// Свежесозданный оператор: ролей ещё нет, реконсилировать-revoke нечего —
	// grant-only поверх DB (та же транзакционность снаружи не нужна, federated-
	// provision редок).
	if err := m.grantRoles(ctx, m.cfg.DB, aid, roles); err != nil {
		return MappedOperator{}, err
	}

	// audit `operator.provisioned` (без секретов: пароль/bind-creds не приходят
	// в ext, группы — не секрет).
	ev := &audit.Event{
		AuditID:   audit.NewULID(),
		EventType: audit.EventOperatorProvisioned,
		Source:    audit.SourceAPI,
		ArchonAID: aid,
		Payload: map[string]any{
			"aid":          aid,
			"auth_method":  method,
			"display_name": displayName,
			"roles":        roles,
			"groups":       ext.Groups,
		},
	}
	if err := m.cfg.Audit.Write(ctx, ev); err != nil {
		// Оператор уже создан; audit потерян. Не проваливаем логин (operator —
		// истина источника), но логируем для ручной сверки.
		if m.cfg.Logger != nil {
			m.cfg.Logger.Error("auth/mapper: provision audit write failed (operator created, audit lost)",
				slog.String("aid", aid), slog.Any("error", err))
		}
	}
	return MappedOperator{AID: aid, Roles: roles, Provisioned: true}, nil
}

// reconcileRoles приводит ПРЯМОЙ membership оператора (rbac_role_operators) к
// набору `want` (роли из текущих групп пользователя) — grant новых + scoped-
// revoke ушедших (HIGH-1, реализация ADR-058(d) «роли = внешние группы»).
//
// Scope revoke (КРИТИЧНО): снимаются ТОЛЬКО роли из домена, которым управляет
// этот mapper (m.managedRoles = объединение values(group_role_map)). Роли,
// выданные Synod / вручную / иным путём ВНЕ group_role_map, НЕ трогаются —
// federated-реконсиляция владеет лишь своим доменом.
//
// Алгоритм: revoke = (текущий прямой membership ∩ managedRoles) \ want;
// grant = want \ текущий. Обе мутации — в ОДНОЙ транзакции (Tx-фабрика):
// сбой между grant и revoke не должен оставить membership рассогласованным.
//
// Tx==nil (unit-тест с fake-DB без BeginTx) → fallback на grant-only поверх DB
// (back-compat): revoke не выполняется, но grant из групп сохраняется. daemon
// всегда выставляет Tx, поэтому в проде реконсиляция атомарна и со снятием.
func (m *DBMapper) reconcileRoles(ctx context.Context, aid string, want []string) error {
	for _, role := range want {
		if !rbac.ValidRoleName(role) {
			return fmt.Errorf("auth/mapper: invalid role name %q in group_role_map", role)
		}
	}

	if m.cfg.Tx == nil {
		// Back-compat без транзакции: только grant (revoke требует чтения текущего
		// membership + атомарности с grant — без Tx не гарантируем).
		return m.grantRoles(ctx, m.cfg.DB, aid, want)
	}

	tx, err := m.cfg.Tx.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return fmt.Errorf("auth/mapper: begin reconcile tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }() // no-op после Commit

	current, err := rbac.DirectRolesOf(ctx, tx, aid)
	if err != nil {
		return err
	}

	wantSet := make(map[string]struct{}, len(want))
	for _, r := range want {
		wantSet[r] = struct{}{}
	}
	currentSet := make(map[string]struct{}, len(current))
	for _, r := range current {
		currentSet[r] = struct{}{}
	}

	// Revoke: текущие роли в managed-домене, которых больше нет в want.
	for _, role := range current {
		if _, managed := m.managedRoles[role]; !managed {
			continue // вне group_role_map-домена — не наша зона (Synod/ручная)
		}
		if _, keep := wantSet[role]; keep {
			continue
		}
		if err := rbac.RevokeOperator(ctx, tx, role, aid); err != nil {
			// Пары может уже не быть (гонка с ручным revoke) — это ок, не валим.
			if errors.Is(err, rbac.ErrRoleOperatorNotFound) {
				continue
			}
			return fmt.Errorf("auth/mapper: reconcile revoke role %q: %w", role, err)
		}
	}

	// Grant: роли из want, которых ещё нет (идемпотентно, но пропускаем уже-есть).
	for _, role := range want {
		if _, have := currentSet[role]; have {
			continue
		}
		if err := rbac.GrantOperator(ctx, tx, role, aid, nil); err != nil {
			return fmt.Errorf("auth/mapper: reconcile grant role %q: %w", role, err)
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("auth/mapper: commit reconcile tx: %w", err)
	}
	return nil
}

// grantRoles делает идемпотентный grant каждой роли поверх db (pool ИЛИ tx).
// granted_by_aid = nil (federated-membership без оператора-инициатора).
// Используется provision-путём (свежий оператор) и Tx==nil-fallback-ом
// reconcileRoles. Невалидное имя роли — ошибка конфигурации.
func (m *DBMapper) grantRoles(ctx context.Context, db operator.ExecQueryRower, aid string, roles []string) error {
	for _, role := range roles {
		if !rbac.ValidRoleName(role) {
			return fmt.Errorf("auth/mapper: invalid role name %q in group_role_map", role)
		}
		if err := rbac.GrantOperator(ctx, db, role, aid, nil); err != nil {
			return fmt.Errorf("auth/mapper: grant role %q: %w", role, err)
		}
	}
	return nil
}

// rolesForGroups пересекает группы пользователя с group_role_map и собирает
// дедуплицированный, стабильно отсортированный набор ролей.
func (m *DBMapper) rolesForGroups(groups []string) []string {
	if len(m.cfg.GroupRoleMap) == 0 || len(groups) == 0 {
		return nil
	}
	seen := make(map[string]struct{})
	for _, g := range groups {
		for _, role := range m.cfg.GroupRoleMap[g] {
			seen[role] = struct{}{}
		}
	}
	if len(seen) == 0 {
		return nil
	}
	out := make([]string, 0, len(seen))
	for role := range seen {
		out = append(out, role)
	}
	sort.Strings(out)
	return out
}

// compile-time assertion: *DBMapper реализует Mapper.
var _ Mapper = (*DBMapper)(nil)
