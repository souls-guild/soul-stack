package rbac

import (
	"errors"
	"fmt"
	"sort"
	"time"
)

// ErrPermissionDenied — sentinel deny-результат. Все остальные ошибки
// Check (например, unknown action в каталоге) возвращаются обёрнутыми,
// но НЕ через этот sentinel — middleware маппит их одинаково (403), но
// тестам нужно различать «явный deny» и «misconfigured-call».
var ErrPermissionDenied = errors.New("rbac: permission denied")

// ErrOperatorRevoked — sentinel deny-результат для Архонта, у которого в
// реестре `operators` выставлен `revoked_at` (ADR-014 Amendment 2026-05-27,
// JWT immediate revoke). Транспорт маппит его в 401 (parity с expired JWT),
// а НЕ в 403 как [ErrPermissionDenied] — токен формально валиден, но
// больше не доверенный.
var ErrOperatorRevoked = errors.New("rbac: operator revoked")

// Role — runtime-форма роли из БД-снимка (после parse permissions).
// Не экспортируется в API (handler-ы видят только Enforcer); внешний код
// узнаёт о «ролях оператора» через [Enforcer.RolesOf].
type Role struct {
	Name        string
	Permissions []Permission

	// DefaultScope — распарсенный role default_scope (ADR-047 S1), наследуемый
	// permission-ами роли без собственного селектора. nil = NULL = измерение
	// НЕ введено (bare-permissions роли → unrestricted, backcompat). Форма
	// идентична [Permission.Selector] — тот же closed enum ключей.
	DefaultScope map[string][]string
}

// Enforcer — in-memory snapshot RBAC-каталога. Безопасен для конкурентного
// чтения после конструктора (immutable; hot-reload по ADR-021 будет
// заменять weak-ref-pointer целиком через [config.Store], этот тип сам
// reload не делает).
type Enforcer struct {
	// rolesByAID — резолв «AID → []*Role». Указатели — чтобы не копировать
	// списки permissions на каждый Check (роль может содержать десятки
	// permission-ов).
	rolesByAID map[string][]*Role

	// roles — все роли в порядке declaration-а. Используется RolesOf и
	// диагностикой.
	roles []*Role

	// revoked — копия Snapshot.Revoked. Хранится в enforcer-е (а не отдельной
	// проекцией), чтобы Check был дешёвой одной map-lookup-проверкой без
	// синхронизации с внешней структурой (immutable после конструктора).
	revoked map[string]time.Time
}

// NewEnforcerFromSnapshot строит Enforcer из БД-снимка (ADR-028(d)) —
// единственный источник RBAC-каталога (config-RBAC удалён hard-cut-ом
// ADR-028(g)). Permission-строки парсятся через [ParsePermission]; матчинг и
// поверхность — Check / RolesOf / ClusterAdmins / HasWildcard.
//
// nil-снимок → пустой enforcer (default deny). Невалидная permission в БД
// (например, имя вне каталога после рассинхрона версий) → ошибка; caller
// ([Holder]) на TTL-refresh-fail оставляет прежний снимок + warn, как и
// делал на config-reload-fail.
//
// Membership-строки, ссылающиеся на роль вне snapshot.Roles, игнорируются
// (защита от рассинхрона; в норме FK rbac_role_operators.role_name это
// исключает).
func NewEnforcerFromSnapshot(snap *Snapshot) (*Enforcer, error) {
	e := &Enforcer{
		rolesByAID: make(map[string][]*Role),
	}
	if snap == nil {
		return e, nil
	}
	// Revoked-проекция (ADR-014 Amendment 2026-05-27): копию не делаем,
	// Snapshot.Revoked после конструирования enforcer-а уже не мутируется
	// (caller — Holder — после Refresh всегда строит свежий enforcer).
	e.revoked = snap.Revoked

	byName := make(map[string]*Role, len(snap.Roles))
	for name, rawPerms := range snap.Roles {
		role := &Role{Name: name}
		for _, raw := range rawPerms {
			p, err := ParsePermission(raw)
			if err != nil {
				return nil, fmt.Errorf("rbac: role %q permission %q: %w", name, raw, err)
			}
			role.Permissions = append(role.Permissions, p)
		}
		// default_scope роли (ADR-047 S1): парсится тем же [parseSelector],
		// что per-perm-селектор. Отсутствие ключа в RoleScopes = NULL = nil
		// scope (роль без scope-ограничения).
		if rawScope, ok := snap.RoleScopes[name]; ok {
			scope, err := ParseDefaultScope(rawScope)
			if err != nil {
				return nil, fmt.Errorf("rbac: role %q default_scope %q: %w", name, rawScope, err)
			}
			role.DefaultScope = scope
		}
		byName[name] = role
		e.roles = append(e.roles, role)
	}

	for aid, roleNames := range snap.Membership {
		for _, name := range roleNames {
			role, ok := byName[name]
			if !ok {
				// Роль из membership-а отсутствует в каталоге (рассинхрон) —
				// пропускаем: привязка к несуществующей роли ничего не даёт.
				continue
			}
			e.rolesByAID[aid] = append(e.rolesByAID[aid], role)
		}
	}

	return e, nil
}

// Check возвращает nil, если AID имеет permission для (resource, action)
// в данном context-е; иначе — wrapped [ErrPermissionDenied].
//
// Алгоритм по rbac.md § Семантика конфликта (OR среди allow):
//  1. Найти роли AID-а.
//  2. Для каждой permission в ролях — Matches(resource, action, context).
//  3. Хотя бы одна true → allow (return nil).
//  4. Иначе → ErrPermissionDenied.
//
// context — runtime-фильтр, передаётся middleware из request (например,
// `{"service": ..., "incarnation": ...}` для incarnation endpoints).
// Пустая map допустима — означает «без контекста»; в этом случае permissions
// с селекторами не сматчат, только bare-permissions и full-wildcard.
func (e *Enforcer) Check(aid, resource, action string, context map[string]string) error {
	if resource == "" || action == "" {
		return fmt.Errorf("rbac: Check called with empty resource/action")
	}
	// Revoked-shortcut (ADR-014 Amendment 2026-05-27): ревокнутый Архонт
	// получает deny независимо от ролей. Проверка идёт ПЕРЕД любой
	// permission-логикой — иначе bare `*`-роль пропустила бы revoked AID.
	if revokedAt, ok := e.revoked[aid]; ok {
		return fmt.Errorf("%w: AID %q revoked at %s",
			ErrOperatorRevoked, aid, revokedAt.UTC().Format(time.RFC3339))
	}
	roles, ok := e.rolesByAID[aid]
	if !ok || len(roles) == 0 {
		return fmt.Errorf("%w: AID %q has no roles, resource=%q action=%q",
			ErrPermissionDenied, aid, resource, action)
	}
	for _, role := range roles {
		for _, p := range role.Permissions {
			if p.Matches(resource, action, context) {
				return nil
			}
		}
	}
	return fmt.Errorf("%w: AID %q lacks %s.%s (roles: %s)",
		ErrPermissionDenied, aid, resource, action, joinRoleNames(roles))
}

// HasWildcard — true, если у AID есть хотя бы одна `*`-permission
// (через любую из ролей). Используется self-lockout инвариантом —
// «нельзя ревокнуть последнего cluster-admin» (rbac.md → Инвариант self-lockout).
func (e *Enforcer) HasWildcard(aid string) bool {
	for _, role := range e.rolesByAID[aid] {
		for _, p := range role.Permissions {
			if p.IsWildcard {
				return true
			}
		}
	}
	return false
}

// ClusterAdmins возвращает список AID-ов с активной wildcard-permission.
// Используется для self-lockout проверки: «если ревок-target — единственный
// активный cluster-admin → 409 would-lock-out-cluster».
//
// Returned list — снимок состояния enforcer-а, не учитывает revoked_at
// в БД (это слой выше — caller отфильтровывает revoked).
func (e *Enforcer) ClusterAdmins() []string {
	// Множество, чтобы дедуплицировать AID-ы, у которых wildcard через
	// несколько ролей.
	seen := make(map[string]struct{})
	for aid := range e.rolesByAID {
		if e.HasWildcard(aid) {
			seen[aid] = struct{}{}
		}
	}
	out := make([]string, 0, len(seen))
	for aid := range seen {
		out = append(out, aid)
	}
	return out
}

// CovenScope возвращает coven-scope оператора для конкретного (resource,
// action) — множество Coven-меток, на которые распространяется его право,
// и флаг unrestricted.
//
// Семантика — дуал [Permission.Matches] по ключу `coven`:
//   - unrestricted=true, если у AID есть хоть одна матчащая (resource, action)
//     permission БЕЗ селектора вообще (nil), либо `*` (full-wildcard), либо
//     с ключом `coven` со значением-wildcard `*`. Такой оператор не ограничен
//     по coven — bulk-WHERE не добавляет coven-фильтр, а назначаемая метка
//     проходит без scope-проверки.
//   - unrestricted=false → covens = объединение всех конкретных значений
//     `coven=`-ключа по матчащим permission-ам (дедуп, отсортировано).
//   - permission с непустым селектором, но БЕЗ ключа `coven` (только
//     `host=`/`incarnation=`/`service=`) НЕ делает оператора unrestricted по
//     coven: её вклад covens=nil, unrestricted=false. Это симметрично
//     [Permission.Matches] — там host-only-permission не сматчит запрос без
//     `host` в контексте, значит она ограничивает в другом измерении, а не
//     «разрешает любой coven». Латентный escalation-footgun, если CovenScope
//     вызовут без route-гейта.
//
// Union по нескольким матчащим ролям: если ХОТЬ ОДНА из них unrestricted →
// итог unrestricted=true; иначе covens — union конкретных coven-значений из
// ролей с ключом `coven`. Пустой covens при unrestricted=false означает
// «оператор не вправе трогать ни один coven для этого действия» (например,
// право только с `host=`-селектором).
//
// Используется bulk-API scope-intersection-ом (selector ∩ scope, ТЗ-пилот
// `POST /v1/souls/coven`): целевые хосты ⊆ scope + назначаемая метка ∈ scope.
// Симметрично least-privilege subset-check в [Service] (тот режет ВЫДАЧУ прав,
// этот — ОБЪЁМ массовой мутации).
//
// S0 (ADR-047): CovenScope — тонкая проекция [Enforcer.ResolvePurview] на
// измерение `coven`. Вся логика (дуал [Permission.Matches] по `coven`, union
// по ролям, семантика bare/`*`/`coven=*` → unrestricted) живёт в ResolvePurview;
// здесь — лишь распаковка Purview в прежнюю `(covens, unrestricted)`-форму,
// чтобы не трогать единственного потребителя (soul.coven-assign). Подробная
// нормативная семантика wildcard-ветки `coven=*` и host-only-селектора —
// в [Enforcer.ResolvePurview] и [Permission.Matches].
func (e *Enforcer) CovenScope(aid, resource, action string) (covens []string, unrestricted bool) {
	p := e.ResolvePurview(aid, resource, action)
	return p.Covens, p.Unrestricted
}

// HoldsAction — existence-gate read-эндпоинтов (ADR-047 §г amendment
// 2026-06-04): держит ли оператор (resource, action) хоть в КАКОМ-ТО scope,
// игнорируя request-контекст. Это другой вопрос, чем scope-aware [Enforcer.Check]:
// тот отвечает «применима ли permission в данном scope-контексте» и для
// scoped-permission с пустым контекстом даёт ложный deny (селектор-ключа нет в
// nil-контексте) — что отрезало бы scoped-оператора от собственного списка ДО
// handler-а. Gate спрашивает лишь о НАЛИЧИИ права; сужение по scope делает
// handler после фетча строк (per-resource резолверы soulpurview/statepredicate).
//
// Семантика поверх [Enforcer.ResolvePurview] (без новой матчинг-логики):
//   - bare-permission / `*` / любое заполненное измерение (coven/regex/
//     soulprint/state) → true (existence держится);
//   - no-permission (Purview{} — нет матчащей роли / неизвестный AID) → false;
//   - Deny → false (forward-compat S2+: «введённое пустое измерение» = явный
//     scope-deny; в coven-MVP Deny всегда false, ветка — заготовка).
func (e *Enforcer) HoldsAction(aid, resource, action string) bool {
	return holdsFromPurview(e.ResolvePurview(aid, resource, action))
}

// holdsFromPurview — предикат existence-gate над готовой [Purview] (единый
// источник правды для [Enforcer.HoldsAction] и его теста). Вынесен из тела
// метода, чтобы guard-тест forward-compat-ветки `Deny` (ResolvePurview в
// coven-MVP Deny не выставляет) проверял ту же формулу, а не её дубликат —
// иначе при правке формулы тест и метод молча расходятся.
//
//   - Deny → false (forward-compat S2+: «введённое пустое измерение» = явный
//     scope-deny);
//   - иначе bare/`*` (Unrestricted) ИЛИ любое заполненное измерение
//     (coven/regex/soulprint/state) → true.
func holdsFromPurview(p Purview) bool {
	if p.Deny {
		return false
	}
	return p.Unrestricted ||
		len(p.Covens)+len(p.Regexes)+len(p.SoulprintExprs)+len(p.StateExprs) > 0
}

// RolesOf возвращает имена ролей, привязанных к AID. Используется
// `IssueToken` (PM-decision M0.6b #5) — выдать JWT с current roles из
// keeper.yml, не roles из старого JWT.
func (e *Enforcer) RolesOf(aid string) []string {
	roles := e.rolesByAID[aid]
	if len(roles) == 0 {
		return nil
	}
	out := make([]string, 0, len(roles))
	for _, r := range roles {
		out = append(out, r.Name)
	}
	return out
}

// EffectivePermission — одно эффективное право оператора с разложенным scope.
// Возвращается [Enforcer.PermissionsOf] для self-describing-эндпоинта
// `GET /v1/me/permissions` (permission-aware UI: «можно ли resource.action и в
// каком scope»).
//
// Wildcard=true — full-`*` (cluster-admin): Resource/Action пусты, Scope —
// нулевой (право не ограничено ничем; UI трактует как «можно всё»). В этом
// случае PermissionsOf отдаёт РОВНО один элемент-маркер и больше ничего.
type EffectivePermission struct {
	// Wildcard — оператор имеет full-`*` (cluster-admin). При true Resource/
	// Action пусты, Scope игнорируется.
	Wildcard bool

	// Resource/Action — конкретная пара права (`incarnation`/`run`). Action
	// может быть `*` (resource-wildcard, например `incarnation.*`).
	Resource string
	Action   string

	// Scope — разрешённый scope этого права по измерениям ([ResolvePurview]):
	// covens / regex / soulprint + флаг Unrestricted. UI решает, рисовать ли
	// scope-сводку. Для resource-wildcard (`incarnation.*`) scope резолвится по
	// самому wildcard-action `*` — это верхняя граница для resource.
	Scope Purview
}

// PermissionsOf возвращает все эффективные права AID — распаковку
// `rolesByAID[aid][*].Permissions` с дедупом по (resource, action) и scope из
// [Enforcer.ResolvePurview] на каждую пару. Детерминированный порядок
// (resource, затем action) для стабильности UI/тестов.
//
// Семантика:
//   - full-`*` хоть в одной роли → РОВНО один [EffectivePermission]{Wildcard:true}
//     (cluster-admin не ограничен; перечислять весь каталог бессмысленно).
//   - иначе — по одной записи на уникальную (resource, action); Scope резолвится
//     [ResolvePurview], который сам делает union по ролям и наследование
//     default_scope (ADR-047). Это НЕ дублирует Matches/subset-логику — только
//     читает уже распарсенные Permission-ы и существующий resolver.
//   - неизвестный AID / AID без ролей → nil.
//
// resource-wildcard (`incarnation.*`) отдаётся как есть (Action="*"); UI
// трактует `*` как «любой action этого resource».
func (e *Enforcer) PermissionsOf(aid string) []EffectivePermission {
	// Revoked-shortcut (ADR-047 G1, зеркало [Enforcer.Check]/[Enforcer.ResolvePurview]):
	// ревокнутый Архонт получает пустой список прав независимо от ролей — ПЕРЕД
	// веткой IsWildcard, иначе revoked cluster-admin (`*`) увидел бы свой бывший
	// wildcard-маркер на `GET /v1/me/permissions`. revoked = «нет прав» (wildcard
	// и scoped одинаково), форма результата — та же, что у unknown-AID-ветки (nil).
	if _, ok := e.revoked[aid]; ok {
		return nil
	}
	roles, ok := e.rolesByAID[aid]
	if !ok || len(roles) == 0 {
		return nil
	}

	// Дедуп по (resource, action). Full-`*` обрабатывается отдельной ранней
	// ветвью — в seen он не попадает.
	type pair struct{ resource, action string }
	seen := make(map[pair]struct{})
	for _, role := range roles {
		for _, p := range role.Permissions {
			if p.IsWildcard {
				return []EffectivePermission{{Wildcard: true}}
			}
			seen[pair{p.Resource, p.Action}] = struct{}{}
		}
	}

	out := make([]EffectivePermission, 0, len(seen))
	for pr := range seen {
		out = append(out, EffectivePermission{
			Resource: pr.resource,
			Action:   pr.action,
			Scope:    e.ResolvePurview(aid, pr.resource, pr.action),
		})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Resource != out[j].Resource {
			return out[i].Resource < out[j].Resource
		}
		return out[i].Action < out[j].Action
	})
	return out
}

// RoleCount — число ролей в снимке. Источник keeper_rbac_snapshot_roles:
// Enforcer immutable, после построения Snapshot не держится, поэтому метрика
// читает его отсюда, а не из [Snapshot].
func (e *Enforcer) RoleCount() int {
	return len(e.roles)
}

// OperatorCount — число операторов с ≥1 ролевой привязкой в снимке. Источник
// keeper_rbac_snapshot_operators: rolesByAID содержит только AID с привязками
// к существующим ролям (AID без привязок = default-deny, в map не попадает).
func (e *Enforcer) OperatorCount() int {
	return len(e.rolesByAID)
}

// joinRoleNames — comma-separated имена ролей для diagnostic-сообщений.
func joinRoleNames(roles []*Role) string {
	if len(roles) == 0 {
		return "<none>"
	}
	out := roles[0].Name
	for _, r := range roles[1:] {
		out += "," + r.Name
	}
	return out
}
