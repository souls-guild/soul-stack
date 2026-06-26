package rbac

import (
	"context"
	"fmt"
)

// ErrPermissionNotHeld — least-privilege-нарушение (subset-check): caller
// пытается выдать через роль permission, которым НЕ обладает сам (создать роль
// с таким правом / добавить его в роль / привязать роль с таким правом).
//
// Защита от вертикальной эскалации: оператор с `role.create` +
// `role.grant-operator`, но без `*`, не должен создать роль с `permissions:
// ["*"]` и стать эффективным cluster-admin. Инвариант — «нельзя выдать право,
// которого не имеешь сам» (rbac.md → § Инвариант least-privilege).
//
// ОТДЕЛЬНЫЙ sentinel от [ErrPermissionDenied]: тот — «нет права на саму
// операцию» (проверяется middleware/tool ДО Service); этот — «операция
// разрешена, но её содержимое превышает права caller-а» (проверяется внутри
// Service). Transport маппит оба в forbidden/403, но смысловое различие важно
// для логов и тестов.
var ErrPermissionNotHeld = fmt.Errorf("rbac: caller may not grant a permission it does not hold (least-privilege)")

// selectAIDPermissionsSQL — permission-строки активного оператора (revoked_at
// IS NULL) через все его роли, ВМЕСТЕ с default_scope роли (ADR-047 S1): bare-
// perm caller-а наследует scope своей роли, иначе least-privilege сравнивает
// сырьё и bare со scope=prod ложно покрывает любой coven (privilege-escalation).
// Read-only (без FOR UPDATE): subset-check — authorization-gate, а НЕ
// consistency-инвариант, как self-lockout; same-tx-read под READ COMMITTED видит
// committed-состояние и строго свежее enforcer-снимка ([PermissionChecker.Check]).
// Без FOR UPDATE гейт не добавляет новых row-lock-ов и не трогает
// детерминированный lock-порядок self-lockout-ядра (роль → permissions →
// operators) — нет deadlock-риска между конкурентными мутациями.
//
// Маркер `SELECT rp.permission` уникален среди RBAC-запросов (self-lockout-ядро
// и lockRole селектят `ro.aid`/`builtin`) — fake-pool-ы тестов классифицируют
// caller-perms-запрос по нему без коллизии.
//
// Synod (ADR-049(f)): эффективные права caller-а = прямые ∪ через Synod. Вторая
// ветка UNION разворачивает роли через все Synod-ы caller-а (synod_operators ⋈
// synod_roles) — без неё least-privilege недосчитывает права, пришедшие через
// группу: оператор, чьё право X держится ТОЛЬКО через Synod, ложно получил бы
// deny на выдачу X (escalation-via-group — обратная сторона: НЕ-видение своих
// прав), а его эффективный scope не наследовался бы от роли группы. UNION (не
// UNION ALL) схлопывает дубль `(permission, default_scope)` роли, пришедшей и
// напрямую, и через Synod — союз множеств, как в snapshot-сборке. Обе ветки
// фильтруют по `o.revoked_at IS NULL` — revoked caller прав не держит ни одним
// путём.
const selectAIDPermissionsSQL = `
SELECT rp.permission, r.default_scope
FROM rbac_role_permissions rp
JOIN rbac_role_operators ro ON ro.role_name = rp.role_name
JOIN rbac_roles r ON r.name = rp.role_name
JOIN operators o ON o.aid = ro.aid
WHERE ro.aid = $1 AND o.revoked_at IS NULL
UNION
SELECT rp.permission, r.default_scope
FROM synod_operators so
JOIN synod_roles sr ON sr.synod_name = so.synod_name
JOIN rbac_role_permissions rp ON rp.role_name = sr.role_name
JOIN rbac_roles r ON r.name = sr.role_name
JOIN operators o ON o.aid = so.aid
WHERE so.aid = $1 AND o.revoked_at IS NULL
`

// assertCallerMayGrant — least-privilege-гейт Service-мутаций: проверяет, что
// caller вправе выдать КАЖДУЮ permission из required (см. [assertCallerCovers]).
// Вызывается ВНУТРИ tx мутации (CreateRole / UpdateRolePermissions /
// GrantOperator), до записи.
//
// Пустой required → no-op без обращения к БД (нечего проверять — роль без новых
// прав / без прав вообще). callerAID == "" недопустим при непустом required:
// transport всегда несёт caller-а (claims.Subject); пустой caller с правами на
// выдачу — это misconfiguration, отказываем ([ErrPermissionNotHeld]) вместо
// «тихо разрешить».
func (s *Service) assertCallerMayGrant(ctx context.Context, db ExecQueryRower, callerAID string, required []Permission) error {
	if len(required) == 0 {
		return nil
	}
	if callerAID == "" {
		return fmt.Errorf("%w: missing caller", ErrPermissionNotHeld)
	}
	callerPerms, err := callerPermissions(ctx, db, callerAID)
	if err != nil {
		return err
	}
	return assertCallerCovers(callerPerms, required)
}

// requiredPermissions разворачивает гранящиеся permission-строки в ЭФФЕКТИВНЫЕ
// permissions под default_scope роли (ADR-047 S1): bare-perm выдаваемой роли
// наследует её scope, иначе least-privilege сравнил бы сырьё (bare покрывает
// любой coven → caller со scope=prod выдал бы роль scope=staging).
//
// rawScope — RAW default_scope гранящейся роли (nil = NULL = роль без scope,
// bare остаётся unrestricted). Строки уже провалидированы [ParsePermission] в
// Service-методе ДО tx; повторный parse тут не должен падать, но ошибку
// пробрасываем (defensive против рассинхрона), не маскируя под subset.
func requiredPermissions(rawPerms []string, rawScope *string) ([]Permission, error) {
	if len(rawPerms) == 0 {
		return nil, nil
	}
	var scope map[string][]string
	if rawScope != nil {
		var err error
		scope, err = ParseDefaultScope(*rawScope)
		if err != nil {
			return nil, fmt.Errorf("rbac: granted role default_scope %q: %w", *rawScope, err)
		}
	}
	perms := make([]Permission, 0, len(rawPerms))
	for _, raw := range rawPerms {
		p, err := ParsePermission(raw)
		if err != nil {
			return nil, fmt.Errorf("rbac: invalid permission %q: %w", raw, err)
		}
		perms = append(perms, p)
	}
	return effectivePermissions(perms, scope), nil
}

// addedPermissions возвращает permissions из newPerms, которых нет в oldPerms
// (множество добавляемых). UpdateRolePermissions ограничивает least-privilege
// только их: удаление прав не эскалация (см. [Service.UpdateRolePermissions]).
// Порядок newPerms сохраняется; дубли в newPerms схлопываются.
func addedPermissions(oldPerms, newPerms []string) []string {
	old := make(map[string]struct{}, len(oldPerms))
	for _, p := range oldPerms {
		old[p] = struct{}{}
	}
	seen := make(map[string]struct{}, len(newPerms))
	var added []string
	for _, p := range newPerms {
		if _, inOld := old[p]; inOld {
			continue
		}
		if _, dup := seen[p]; dup {
			continue
		}
		seen[p] = struct{}{}
		added = append(added, p)
	}
	return added
}

// callerPermissions читает ЭФФЕКТИВНЫЕ permissions caller-а: каждая строка
// парсится через [ParsePermission], затем bare-perm (Selector==nil) наследует
// default_scope своей роли через [effectivePermissions] (ADR-047 S1) — ровно
// как [Enforcer.ResolvePurview]. Без этого least-privilege сравнивал бы сырьё:
// bare со scope=prod покрыл бы любой coven (privilege-escalation).
//
// Невалидная permission/default_scope в БД (рассинхрон версий) → ошибка:
// subset-check не должен «проглотить» битое право и случайно разрешить выдачу.
// Активный оператор без ролей → пустой набор (default deny). default_scope NULL
// (scope==nil) → bare остаётся unrestricted (backcompat, исключение №2).
func callerPermissions(ctx context.Context, db ExecQueryRower, callerAID string) ([]Permission, error) {
	rows, err := db.Query(ctx, selectAIDPermissionsSQL, callerAID)
	if err != nil {
		return nil, fmt.Errorf("rbac: read caller permissions: %w", wrapPgErr(err))
	}
	defer rows.Close()
	var out []Permission
	for rows.Next() {
		var raw string
		var rawScope *string
		if err := rows.Scan(&raw, &rawScope); err != nil {
			return nil, fmt.Errorf("rbac: scan caller permission: %w", err)
		}
		p, err := ParsePermission(raw)
		if err != nil {
			return nil, fmt.Errorf("rbac: caller permission %q: %w", raw, err)
		}
		var scope map[string][]string
		if rawScope != nil {
			scope, err = ParseDefaultScope(*rawScope)
			if err != nil {
				return nil, fmt.Errorf("rbac: caller role default_scope %q: %w", *rawScope, err)
			}
		}
		if !p.IsWildcard && p.Selector == nil && scope != nil {
			p.Selector = scope
		}
		out = append(out, p)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("rbac: iter caller permissions: %w", err)
	}
	return out, nil
}

// effectivePermissions разворачивает bare-permissions (Selector==nil) под
// default_scope роли — ровно так же, как [Enforcer.ResolvePurview] резолвит
// эффективный селектор (ADR-047 S1): `eff = p.Selector; if nil → scope`.
// Единый источник правила «bare наследует default_scope»; subset-check
// сравнивает ЭФФЕКТИВНЫЕ права обеих сторон, а не сырьё (иначе bare-perm со
// scope=prod ложно покрывает любой coven → privilege-escalation).
//
// scope==nil (роль без default_scope, NULL) — bare остаётся bare (unrestricted):
// caller без scope вправе выдать любой селектор (backcompat, исключение №2
// ADR-047). `*`-permission и per-perm-селектор не трогаются (scope их не
// переопределяет — симметрия с ResolvePurview).
//
// Возвращает новый slice (вход не мутируется): bare-perm с непустым scope
// заменяется копией с Selector=scope. Холодный путь (subset-check на мутации) —
// аллокация допустима.
func effectivePermissions(perms []Permission, scope map[string][]string) []Permission {
	if scope == nil {
		return perms
	}
	out := make([]Permission, len(perms))
	for i, p := range perms {
		if !p.IsWildcard && p.Selector == nil {
			p.Selector = scope
		}
		out[i] = p
	}
	return out
}

// assertCallerCovers проверяет, что КАЖДАЯ required-permission покрыта
// эффективным набором caller-а (least-privilege subset-check). Любая
// непокрытая → [ErrPermissionNotHeld] (caller выдаёт право, которого не имеет).
//
// Обе стороны должны быть УЖЕ эффективными (bare развёрнут под default_scope
// своей роли через [effectivePermissions]) — иначе bare со scope=prod ложно
// покрывает любой coven (privilege-escalation, ADR-047 S1).
//
// Семантика покрытия — существующая implication из [Permission.Matches] (НЕ
// изобретаем новую): caller «имеет» required, если хотя бы одна его permission
// Matches (resource, action) с required-селектором как context-ом. `*` у
// caller-а покрывает всё (IsWildcard → Matches=true).
//
// required-`*` (caller выдаёт full-wildcard) покрывается ТОЛЬКО caller-овским
// `*`: bare `*` как (resource, action) не матчит ни одну non-wildcard
// permission caller-а — значит suboperator не может выдать `*`.
//
// callerPerms резолвится один раз вызывающим — не на каждую required-permission.
func assertCallerCovers(callerPerms, required []Permission) error {
	for _, req := range required {
		if !callerHolds(callerPerms, req) {
			return fmt.Errorf("%w: %s", ErrPermissionNotHeld, permString(req))
		}
	}
	return nil
}

// permString — диагностическая форма permission для error-сообщений subset-а
// (не нормативный сериализатор; только для логов/тестов least-privilege-отказа).
func permString(p Permission) string {
	if p.IsWildcard {
		return "*"
	}
	s := p.Resource + "." + p.Action
	for key, vals := range p.Selector {
		s += " on " + key + "="
		for i, v := range vals {
			if i > 0 {
				s += ","
			}
			s += v
		}
	}
	return s
}

// callerHolds — true, если эффективный набор caller-а покрывает required.
//
// required-`*` (IsWildcard): покрывается ТОЛЬКО caller-овским `*`. Прямой
// проход по Matches-у тут не годится — Matches(resource, action) требует
// конкретные resource/action, а у `*` их нет. Поэтому wildcard-required
// обрабатывается явной веткой: ищем `*` в наборе caller-а.
//
// non-wildcard required: проверяем КАЖДУЮ (ключ, значение)-комбинацию
// селектора отдельно. `x on coven=prod,stage` означает «prod ИЛИ stage» —
// выдача такой permission конферит ОБА значения, поэтому caller обязан
// покрывать каждое. Если caller имеет лишь `x on coven=prod`, выдать
// `coven=prod,stage` нельзя (эскалация на stage). Для каждой такой
// (resource, action, {ключ: значение})-точки спрашиваем caller-permissions
// через их родной [Permission.Matches]; required покрыт, только если покрыты
// все точки.
func callerHolds(callerPerms []Permission, req Permission) bool {
	if req.IsWildcard {
		for _, cp := range callerPerms {
			if cp.IsWildcard {
				return true
			}
		}
		return false
	}

	// Без селектора — одна точка с nil-context-ом.
	if len(req.Selector) == 0 {
		return matchesAny(callerPerms, req.Resource, req.Action, nil)
	}

	// С селектором — каждая (ключ, значение)-точка должна быть покрыта.
	for key, values := range req.Selector {
		if key == "regex" {
			// ADR-047 S2a: regex-subset = string-equality fail-closed. Покрытие
			// одного regex другим («^web- ⊇ ^web-prod-?») статически неразрешимо
			// в общем случае → MVP: caller вправе выдать ТОЛЬКО идентичный
			// паттерн (есть в его эффективном regex-наборе) ЛИБО имеет более
			// широкое право (`*` / bare без regex-селектора этого resource.action).
			// Через Matches идти НЕЛЬЗЯ: regex-строка как host-контекст ложно
			// сматчилась бы с caller-regex (^web-prod- матчит ^web-).
			for _, pat := range values {
				if !callerHoldsRegex(callerPerms, req.Resource, req.Action, pat) {
					return false
				}
			}
			continue
		}
		if key == "soulprint" {
			// ADR-047 S2b: soulprint-subset = string-equality fail-closed.
			// Покрытие одного CEL-предиката другим (логический containment)
			// статически неразрешимо → MVP: caller вправе выдать ТОЛЬКО
			// идентичный предикат (есть в его эффективном soulprint-наборе) ЛИБО
			// имеет более широкое право (`*` / bare без soulprint-селектора
			// этого resource.action). Симметрично regex-ветке.
			for _, expr := range values {
				if !callerHoldsSoulprint(callerPerms, req.Resource, req.Action, expr) {
					return false
				}
			}
			continue
		}
		if key == "state" {
			// ADR-047 S2c: state-subset = string-equality fail-closed.
			// Покрытие одного state-CEL-предиката другим (логический containment)
			// статически неразрешимо → MVP: caller вправе выдать ТОЛЬКО идентичный
			// предикат (есть в его эффективном state-наборе) ЛИБО имеет более
			// широкое право (`*` / bare без state-селектора этого resource.action).
			// Симметрично soulprint/regex-веткам.
			for _, expr := range values {
				if !callerHoldsState(callerPerms, req.Resource, req.Action, expr) {
					return false
				}
			}
			continue
		}
		if key == "trait" {
			// ADR-047 amendment (ADR-060 п.7 slice 1): trait-subset = string-equality
			// fail-closed. trait — точная `key:value`-пара (не предикат), но логика
			// покрытия та же, что у state/soulprint: caller вправе выдать ТОЛЬКО
			// идентичную пару (есть в его эффективном trait-наборе) ЛИБО имеет более
			// широкое право (`*` / bare без trait-селектора этого resource.action).
			// Через Matches идти НЕЛЬЗЯ (trait fail-closed без traits в context) —
			// прямое сравнение пар, симметрично state/soulprint-веткам.
			for _, pair := range values {
				if !callerHoldsTrait(callerPerms, req.Resource, req.Action, pair) {
					return false
				}
			}
			continue
		}
		for _, v := range values {
			if !matchesAny(callerPerms, req.Resource, req.Action, map[string]string{key: v}) {
				return false
			}
		}
	}
	return true
}

// callerHoldsRegex — покрывает ли caller выдачу regex-паттерна pat для
// (resource, action). MVP-семантика (ADR-047 S2a, fail-closed): покрыто, если
// у caller-а есть
//   - `*`-permission (покрывает всё), ИЛИ
//   - матчащая (resource, action) BARE-permission (Selector==nil — caller
//     ничем не ограничен, вправе выдать любой regex), ИЛИ
//   - матчащая permission с ИДЕНТИЧНЫМ regex-паттерном (string-equality).
//
// Caller с ОГРАНИЧЕНИЕМ В ДРУГОМ измерении (`coven=prod` без regex) НЕ покрывает
// regex-грант — симметрия с exact-ключами ([Permission.Matches]: ключ-not-in-
// context → deny). Сужение regex (caller `^web-` выдаёт `^web-prod-`) статически
// недоказуемо → DENY. Containment regex НЕ реализуется (см. ВНИМАНИЕ в ТЗ S2a).
func callerHoldsRegex(callerPerms []Permission, resource, action, pat string) bool {
	for _, cp := range callerPerms {
		if cp.IsWildcard {
			return true
		}
		if cp.Resource != resource {
			continue
		}
		if cp.Action != "*" && cp.Action != action {
			continue
		}
		if cp.Selector == nil {
			// bare-permission caller-а — не ограничен по regex, покрывает любой.
			return true
		}
		for _, cpat := range cp.Selector["regex"] {
			if cpat == pat {
				return true
			}
		}
	}
	return false
}

// callerHoldsSoulprint — покрывает ли caller выдачу soulprint-предиката expr для
// (resource, action). MVP-семантика (ADR-047 S2b, fail-closed, параллель
// [callerHoldsRegex]): покрыто, если у caller-а есть
//   - `*`-permission (покрывает всё), ИЛИ
//   - матчащая (resource, action) BARE-permission (Selector==nil — caller ничем
//     не ограничен, вправе выдать любой soulprint-предикат), ИЛИ
//   - матчащая permission с ИДЕНТИЧНЫМ soulprint-предикатом (string-equality).
//
// Caller с ОГРАНИЧЕНИЕМ В ДРУГОМ измерении (`coven=prod` / иной soulprint) НЕ
// покрывает soulprint-грант — симметрия с exact-ключами и regex. Логический
// containment CEL-предикатов недоказуем статически → DENY (fail-closed).
func callerHoldsSoulprint(callerPerms []Permission, resource, action, expr string) bool {
	for _, cp := range callerPerms {
		if cp.IsWildcard {
			return true
		}
		if cp.Resource != resource {
			continue
		}
		if cp.Action != "*" && cp.Action != action {
			continue
		}
		if cp.Selector == nil {
			// bare-permission caller-а — не ограничен по soulprint, покрывает любой.
			return true
		}
		for _, cexpr := range cp.Selector["soulprint"] {
			if cexpr == expr {
				return true
			}
		}
	}
	return false
}

// callerHoldsState — покрывает ли caller выдачу state-предиката expr для
// (resource, action). MVP-семантика (ADR-047 S2c, fail-closed, параллель
// [callerHoldsSoulprint]): покрыто, если у caller-а есть
//   - `*`-permission (покрывает всё), ИЛИ
//   - матчащая (resource, action) BARE-permission (Selector==nil — caller ничем
//     не ограничен, вправе выдать любой state-предикат), ИЛИ
//   - матчащая permission с ИДЕНТИЧНЫМ state-предикатом (string-equality).
//
// Caller с ОГРАНИЧЕНИЕМ В ДРУГОМ измерении (`coven=prod` / иной state) НЕ
// покрывает state-грант — симметрия с exact-ключами, regex и soulprint.
// Логический containment CEL-предикатов недоказуем статически → DENY (fail-closed).
func callerHoldsState(callerPerms []Permission, resource, action, expr string) bool {
	for _, cp := range callerPerms {
		if cp.IsWildcard {
			return true
		}
		if cp.Resource != resource {
			continue
		}
		if cp.Action != "*" && cp.Action != action {
			continue
		}
		if cp.Selector == nil {
			// bare-permission caller-а — не ограничен по state, покрывает любой.
			return true
		}
		for _, cexpr := range cp.Selector["state"] {
			if cexpr == expr {
				return true
			}
		}
	}
	return false
}

// callerHoldsTrait — покрывает ли caller выдачу trait-пары pair (`key:value`) для
// (resource, action). MVP-семантика (ADR-047 amendment / ADR-060 п.7 slice 1,
// fail-closed, параллель [callerHoldsState]): покрыто, если у caller-а есть
//   - `*`-permission (покрывает всё), ИЛИ
//   - матчащая (resource, action) BARE-permission (Selector==nil — caller ничем
//     не ограничен, вправе выдать любую trait-пару), ИЛИ
//   - матчащая permission с ИДЕНТИЧНОЙ trait-парой (string-equality).
//
// Caller с ОГРАНИЧЕНИЕМ В ДРУГОМ измерении (`coven=prod` / иная trait-пара) НЕ
// покрывает trait-грант — симметрия с exact-ключами, regex, soulprint и state.
func callerHoldsTrait(callerPerms []Permission, resource, action, pair string) bool {
	for _, cp := range callerPerms {
		if cp.IsWildcard {
			return true
		}
		if cp.Resource != resource {
			continue
		}
		if cp.Action != "*" && cp.Action != action {
			continue
		}
		if cp.Selector == nil {
			// bare-permission caller-а — не ограничен по trait, покрывает любую.
			return true
		}
		for _, cpair := range cp.Selector["trait"] {
			if cpair == pair {
				return true
			}
		}
	}
	return false
}

// matchesAny — true, если хотя бы одна permission caller-а матчит конкретную
// (resource, action, context)-точку. Тонкая обёртка над [Permission.Matches]
// для перебора набора.
func matchesAny(callerPerms []Permission, resource, action string, ctx map[string]string) bool {
	for _, cp := range callerPerms {
		if cp.Matches(resource, action, ctx) {
			return true
		}
	}
	return false
}
