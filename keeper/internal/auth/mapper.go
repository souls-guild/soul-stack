package auth

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"

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
	// Реальный pgxpool.Pool удовлетворяет интерфейсу; provision идёт одной
	// транзакцией снаружи здесь не нужен — Insert+Grant выполняются
	// последовательно (federated-provision редок, не hot-path).
	DB operator.ExecQueryRower

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
}

// NewMapper конструирует DBMapper.
func NewMapper(cfg MapperConfig) *DBMapper {
	return &DBMapper{cfg: cfg}
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
		// Существующий активный оператор: роли — из групп (источник ролей =
		// группы, развилка №2). Membership синхронизируем (идемпотентный grant),
		// чтобы внешняя смена групп отражалась в RBAC при следующем логине.
		if err := m.syncRoles(ctx, aid, roles); err != nil {
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
	if err := m.syncRoles(ctx, aid, roles); err != nil {
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

// syncRoles делает идемпотентный grant каждой роли (membership-авторитет —
// rbac_role_operators, ADR-028(c)). granted_by_aid = nil (federated-membership
// без оператора-инициатора). Невалидное имя роли в config → ошибка конфигурации.
func (m *DBMapper) syncRoles(ctx context.Context, aid string, roles []string) error {
	for _, role := range roles {
		if !rbac.ValidRoleName(role) {
			return fmt.Errorf("auth/mapper: invalid role name %q in group_role_map", role)
		}
		if err := rbac.GrantOperator(ctx, m.cfg.DB, role, aid, nil); err != nil {
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
