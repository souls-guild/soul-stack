// Package rbac — runtime-проверка permissions Архонтов по [docs/keeper/rbac.md].
//
// Каталог [AllowedPermissions] — closed enum имён, валидируемых
// [NewEnforcer] при load `keeper.yml`. Unknown-имя → fatal-ошибка
// (PM-decision M0.6b #7).
//
// Селекторы (`on key=v1,v2`) парсятся отдельно; grammar — `<resource>.<action>`
// + optional `key=values` где key ∈ {service, coven, incarnation, host}.
package rbac

import "sort"

// AllowedPermissions — каталог permission-имён из rbac.md → §Каталог
// permissions. 67 имён MVP:
//
//   - operator (5): create / revoke / issue-token / list / read;
//   - role (6): create / delete / list / update / grant-operator / revoke-operator;
//   - synod (8): create / update / delete / list / add-operator / remove-operator / grant-role / revoke-role (ADR-049);
//   - incarnation (12): create / create-rerun / run / get / list / history / unlock / upgrade / destroy / check-drift / update-hosts / update (deprecated-alias);
//   - soul (6): list / create / issue-token / coven-assign / traits-assign / ssh-target-update;
//   - plugin (3): allow / revoke / list;
//   - sigil (4): key-introduce / key-retire / key-list / key-set-primary;
//   - service (4): register / update / list / deregister;
//   - omen (3): create / list / delete;
//   - rite (3): create / list / delete;
//   - vigil (3): create / list / delete;
//   - decree (3): create / list / delete;
//   - push (3): apply / cleanup / read;
//   - push-provider (5): create / update / delete / list / read (ADR-032 amendment S7-2);
//   - errand (3): run / cancel / list (ADR-033);
//   - choir (5): create / delete / list / add-voice / remove-voice (ADR-044, S-T3);
//   - cadence (6): create / list / update / delete / enable / disable (ADR-046, S4; enable/disable — amendment 2026-06-02);
//   - herald (5): create / read / list / update / delete (ADR-052, S4);
//   - tiding (5): create / read / list / update / delete (ADR-052, S4);
//   - provisioning (2): read / update (ADR-058 Часть B — политика способов создания операторов);
//   - audit (1): read;
//   - provider/profile (2): create / create.
//
// Wildcard `*` в `<action>` (`incarnation.*`) разворачивается на этапе
// resolve и матчит любое известное `<action>` для данного `<resource>`.
// Wildcard в `<resource>` не поддерживается в MVP.
//
// Расширение каталога — обычный PR с дублирующим обновлением rbac.md.
// Removed-имена — never (operator-роли в `keeper.yml` могут содержать
// исторические имена; remove ломает существующие installations).
var AllowedPermissions = map[string]struct{}{
	// operator.*
	"operator.create":      {},
	"operator.revoke":      {},
	"operator.issue-token": {},
	// operator.list / operator.read — read-only-доступ к реестру Архонтов
	// (`GET /v1/operators`, `GET /v1/operators/{aid}`). Селектор — NoSelector
	// (нет per-resource scope, как и у operator.create/revoke); per-AID-scope —
	// отдельный slice при появлении мульти-тенант-RBAC. `operator.read` отделён
	// от `operator.list` симметрично push.read↔push.apply: read одной записи
	// концептуально шире `list`, но в MVP оба покрываются одним правом —
	// drift-test и rbac.md фиксируют наличие в каталоге, route монтирует
	// `operator.list` на оба эндпоинта.
	"operator.list": {},
	"operator.read": {},

	// role.* — управление RBAC (роли / permissions / membership) через
	// OpenAPI/MCP (ADR-028(e), rbac.md → §Каталог permissions → Role).
	"role.create":          {},
	"role.delete":          {},
	"role.list":            {},
	"role.update":          {},
	"role.grant-operator":  {},
	"role.revoke-operator": {},

	// synod.* — управление Synod-группами (ADR-049): промежуточный уровень
	// Архон → Synod → Роли. 8 permissions. Селектор — NoSelector (управление
	// группами — кластер-уровневая операция, без scope по coven/host, как role.*
	// / operator.*; group-scope ADR-049 НЕ вводит). grant-role/add-operator
	// под least-privilege subset, delete/remove-operator/revoke-role под
	// self-lockout (ADR-049(f)). synod.update меняет ТОЛЬКО description (косметика,
	// прав не выдаёт/не отнимает) — без subset/self-lockout; name (PK) immutable.
	"synod.create":          {},
	"synod.update":          {},
	"synod.delete":          {},
	"synod.list":            {},
	"synod.add-operator":    {},
	"synod.remove-operator": {},
	"synod.grant-role":      {},
	"synod.revoke-role":     {},

	// incarnation.*
	"incarnation.create": {},
	// incarnation.create-rerun — перезапуск scenario `create` из error_locked
	// (`POST /v1/incarnations/{name}/rerun-create`, architecture.md →
	// «Атомарность и error_locked»). Отдельное право от `incarnation.create`
	// (создание новой инкарнации) и `incarnation.unlock` (снятие блока без
	// перезапуска): rerun = снятие error_locked + rerun bootstrap-а одним
	// действием, требует явного подтверждения reason. Scope-селектор тот же
	// (incarnation/coven/service по path-{name}).
	"incarnation.create-rerun": {},
	"incarnation.run":          {},
	"incarnation.get":          {},
	"incarnation.list":         {},
	"incarnation.history":      {},
	"incarnation.unlock":       {},
	"incarnation.upgrade":      {},
	"incarnation.destroy":      {},
	"incarnation.check-drift":  {},
	// incarnation.update-hosts — изменение declared `spec.hosts[]` записи
	// incarnation через Operator API (`PATCH /v1/incarnations/{name}/hosts`,
	// UI Hosts editing): тот же scope-селектор incarnation/coven/service, что
	// у других incarnation-мутаций (run/unlock/upgrade). Имя сужено с прежнего
	// `incarnation.update` (PM-decision 2026-06-02) под задел будущих
	// update-covens/update-spec — каждая операция получит свою permission.
	"incarnation.update-hosts": {},
	// incarnation.update — DEPRECATED-alias `incarnation.update-hosts`. Оставлен
	// в каталоге (closed enum, removed-имена — never): роли/operators в
	// keeper.yml / БД с историческим именем НЕ должны фейлить load снимка.
	// [ParsePermission] канонизирует его в `incarnation.update-hosts` (см.
	// [deprecatedActionAliases]), поэтому такие роли сохраняют доступ к /hosts.
	"incarnation.update": {},

	// soul.*
	"soul.list":         {},
	"soul.create":       {},
	"soul.issue-token":  {},
	"soul.coven-assign": {},
	// soul.traits-assign — bulk-мутация operator-set trait-меток (jsonb-колонка
	// `souls.traits`, ADR-060) массово по селектору: merge/replace/remove.
	// Action — hyphenated (`traits-assign`), т.к. permission-грамматика — ровно
	// `<resource>.<action>` (паттерн soul.coven-assign / soul.ssh-target-update).
	// Селектор тот же, что у soul.coven-assign (`coven=` / `host=` / bare): bulk
	// trait-assign гейтится тем же coven-scope (гейт a, целевые хосты ⊆ scope) —
	// least-privilege не ослаблен. trait-КЛЮЧ НЕ является scope-измерением (в
	// отличие от Coven-метки), поэтому гейта (b) на ключи нет.
	"soul.traits-assign": {},
	// soul.ssh-target-update — изменение per-host SSH-реквизитов push-flow
	// (ADR-032 amendment 2026-05-26, S7-1). Action — hyphenated (`ssh-target-update`),
	// т.к. permission-грамматика — ровно `<resource>.<action>` (3-сегментный
	// `soul.ssh-target.update` — это MCP-tool, не permission; паттерн
	// `sigil.key-introduce` ↔ `keeper.sigil.key.introduce`).
	"soul.ssh-target-update": {},

	// plugin.* — управление allow-list-ом целостности плагинов Sigil
	// (ADR-026, rbac.md → §Каталог permissions → Plugin Sigil).
	"plugin.allow":  {},
	"plugin.revoke": {},
	"plugin.list":   {},

	// sigil.* — ротация trust-anchor-ключей ПОДПИСИ Sigil (ADR-026(h), R3-S7).
	// Отдельный resource от plugin.* (тот про допуски бинарей, этот — про ключи
	// их подписи). Action — hyphenated (`key-introduce`), т.к. permission-
	// грамматика — ровно `<resource>.<action>` (3-сегментный `sigil.key.introduce`
	// — это MCP-tool, не permission); MCP-tool keeper.sigil.key.<verb> ↔
	// permission sigil.key-<verb>.
	"sigil.key-introduce":   {},
	"sigil.key-retire":      {},
	"sigil.key-list":        {},
	"sigil.key-set-primary": {},

	// service.* — управление реестром Service-ов `service_registry`
	// (ADR-028-паттерн RBAC-storage, naming-rules.md → service_registry).
	"service.register":   {},
	"service.update":     {},
	"service.list":       {},
	"service.deregister": {},

	// omen.* / rite.* — operator-facing CRUD реестров Augur (omens / rites,
	// ADR-025, rbac.md §Augur). resource — omen/rite (НЕ augur.*); 2-сегментная
	// permission <resource>.<action> с verbs create/list/delete. Live-fetch от
	// Soul (AugurRequest) RBAC-permission НЕ контролируется — это машинный gRPC-
	// запрос, не операторская операция (rbac.md §Augur).
	"omen.create": {},
	"omen.list":   {},
	"omen.delete": {},
	"rite.create": {},
	"rite.list":   {},
	"rite.delete": {},

	// vigil.* / decree.* — operator-facing CRUD реестров Oracle (vigils /
	// decrees, ADR-030 beacons, rbac.md §Oracle). resource — vigil/decree;
	// 2-сегментная permission <resource>.<action> с verbs create/list/delete
	// (паттерн omen/rite). Reactor-флоу (Portent → match Decree → enqueue)
	// RBAC-permission НЕ контролируется — это машинный Soul-инициированный путь,
	// не операторская операция (security — субъектная привязка Decree, ADR-030(b)).
	"vigil.create":  {},
	"vigil.list":    {},
	"vigil.delete":  {},
	"decree.create": {},
	"decree.list":   {},
	"decree.delete": {},

	// push.*
	"push.apply":   {},
	"push.cleanup": {},
	// push.read — чтение состояния push-прогона (`GET /v1/push/{apply_id}`,
	// Variant C orchestrator). Отдельно от push.apply: read-операция не требует
	// mutate-прав (паттерн service.list / role.list — read без audit, бывают
	// сужены под отдельный role для observability-операторов).
	"push.read": {},

	// push-provider.* — CRUD реестра Push-Provider-ов (per-provider env-payload
	// params SSH-плагинов push-flow, ADR-032 amendment 2026-05-26, S7-2). resource
	// — `push-provider` (kebab-single-section с дефисом — корректная форма для
	// двухсловной области, симметрично прецеденту `ssh-target` в action). 5
	// permissions: create / update / delete / list / read. Селектор —
	// NoSelector в MVP: CRUD оперирует самим реестром (как provider.* / service.*
	// / operator.*); per-name scope — отдельный slice при появлении мульти-
	// тенант-RBAC.
	"push-provider.create": {},
	"push-provider.update": {},
	"push-provider.delete": {},
	"push-provider.list":   {},
	"push-provider.read":   {},

	// errand.* — pull-ad-hoc exec одиночного модуля (ADR-033, rbac.md §Errand).
	// Селекторы — `host=<sid>` / `coven=<label>` (как у soul.list / soul.issue-
	// token); bare — unrestricted. errand.cancel — slice E5 (DELETE-endpoint
	// ещё не реализован), permission зарегистрирован forward-only чтобы
	// конфиги ролей не ломались при появлении endpoint-а.
	"errand.run":    {},
	"errand.cancel": {},
	"errand.list":   {},

	// choir.* — operator-facing CRUD топологии хостов внутри инкарнации
	// (Choir/Voice, ADR-044, S-T3). Choir принадлежит инкарнации, поэтому
	// селектор тот же, что у incarnation.* — `incarnation=` / `service=` /
	// `coven=` (приземляется [IncarnationScopeSelector] по path-{name}); bare
	// — unrestricted. resource — `choir`; actions — create / delete / list +
	// add-voice / remove-voice (управление Voice-членством). Voice-actions —
	// hyphenated (`add-voice`/`remove-voice`), т.к. permission-грамматика —
	// ровно `<resource>.<action>` (паттерн soul.ssh-target-update /
	// sigil.key-introduce). Mutating-CRUD аудируется (choir.created /
	// choir.deleted / choir.voice_added / choir.voice_removed).
	"choir.create":       {},
	"choir.delete":       {},
	"choir.list":         {},
	"choir.add-voice":    {},
	"choir.remove-voice": {},

	// cadence.* — operator-facing CRUD реестра Cadence-расписаний (`cadences`,
	// ADR-046 §7). resource — `cadence`; actions — create / list / update /
	// delete + гранулярные enable / disable (паттерн omen/rite/vigil/decree).
	// Селектор — NoSelector в MVP: CRUD оперирует самим реестром расписаний (как
	// push-provider.* / operator.*); per-name scope — отдельный slice при
	// появлении мульти-тенант-RBAC.
	// ДВУХУРОВНЕВЫЙ guard (security-критичный, ADR-046 §7): право `cadence.*`
	// управляет расписанием, но рецепт спавнит Voyage, поэтому при СОЗДАНИИ
	// создатель обязан иметь и Voyage-permission по `kind` рецепта
	// (`incarnation.run` для scenario / `errand.run` для command, ADR-043 §6) —
	// иначе Cadence стала бы privilege-escalation-обходом RBAC. Проверка живёт
	// внутри CadenceHandler.Create (kind виден только из тела, parity Voyage).
	//
	// cadence.enable / cadence.disable — гранулярные права на toggle расписания
	// (`POST /v1/cadences/{id}/enable` / `.../disable`), отделены от
	// `cadence.update` (PATCH рецепта), ADR-046 amendment 2026-06-02. BACKCOMPAT:
	// `cadence.update` остаётся валидным грантом и для toggle — роли со старым
	// правом не теряют enable/disable. Роуты используют OR-гейт
	// [middleware.RequireAnyPermission]: enable допускает `cadence.enable` ИЛИ
	// `cadence.update`, disable — `cadence.disable` ИЛИ `cadence.update`.
	"cadence.create":  {},
	"cadence.list":    {},
	"cadence.update":  {},
	"cadence.delete":  {},
	"cadence.enable":  {},
	"cadence.disable": {},

	// herald.* / tiding.* — operator-facing CRUD реестров уведомлений Herald
	// (каналы доставки) / Tiding (правила подписки) о событиях прогонов (ADR-052,
	// S4). resource — `herald` / `tiding`; actions — create / read / list /
	// update / delete (паттерн omen.* / push-provider.*). Селектор — NoSelector:
	// управление каналами/правилами кластер-уровневое (как role.* / synod.* /
	// omen.*); per-name scope — отдельный slice при появлении мульти-тенант-RBAC.
	// Mutating-CRUD аудируется (herald.created/updated/deleted + tiding.*).
	"herald.create": {},
	"herald.read":   {},
	"herald.list":   {},
	"herald.update": {},
	"herald.delete": {},
	"tiding.create": {},
	"tiding.read":   {},
	"tiding.list":   {},
	"tiding.update": {},
	"tiding.delete": {},

	// provisioning.* — runtime-управление политикой способов СОЗДАНИЯ операторов
	// (`provisioning_allowed_methods` в keeper_settings, ADR-058 Часть B). resource
	// — `provisioning`; actions — read (`GET /v1/provisioning-policy`) / update
	// (`PUT /v1/provisioning-policy`). Селектор — NoSelector: политика кластер-
	// уровневая (как operator.* / role.*). update аудируется
	// (`provisioning.policy_changed`), read — нет.
	"provisioning.read":   {},
	"provisioning.update": {},

	// audit.* — read-only-доступ к `audit_log` (`GET /v1/audit`). Селектор —
	// NoSelector в MVP: фильтрация по archon_aid делается query-param-ом, а
	// per-AID/coven-scope под audit-trail пока не вводится. read audit-событий
	// сам в audit НЕ пишется (избегаем рекурсии: иначе каждый GET /v1/audit
	// удваивает таблицу).
	"audit.read": {},

	// cloud
	"provider.create": {},
	"profile.create":  {},
}

// deprecatedActionAliases — DEPRECATED permission-имена → каноническое.
// Ключ/значение — полная форма `<resource>.<action>`. [ParsePermission]
// канонизирует ключ в значение на load снимка, чтобы [Permission.Matches]
// остался чистым строковым сравнением, а роутер монтировал только
// каноническое имя. Оба имени остаются валидными в [AllowedPermissions]
// (closed enum, removed-имена — never): роли в keeper.yml / БД со старым
// именем НЕ фейлят load и сохраняют доступ.
//
//   - `incarnation.update` → `incarnation.update-hosts` (PM-decision
//     2026-06-02): прежнее имя покрывало только `PATCH /hosts`, сужено под
//     задел будущих update-covens/update-spec.
var deprecatedActionAliases = map[string]string{
	"incarnation.update": "incarnation.update-hosts",
}

// IsAllowedPermission — проверка строки `<resource>.<action>` против
// каталога. Для wildcard-permission (`<resource>.*`) проверяется
// наличие хотя бы одного `<resource>.*`-имени в каталоге (т.е. resource
// known).
func IsAllowedPermission(resource, action string) bool {
	if action == "*" {
		// `<resource>.*` валиден, если для resource есть хотя бы одна
		// permission в каталоге. Любой full-wildcard `*` (без resource)
		// валидируется отдельно в parsePermission.
		for name := range AllowedPermissions {
			if len(name) > len(resource)+1 &&
				name[:len(resource)] == resource && name[len(resource)] == '.' {
				return true
			}
		}
		return false
	}
	_, ok := AllowedPermissions[resource+"."+action]
	return ok
}

// allowedSelectorKeys — closed enum ключей селектора (rbac.md §
// Грамматика селектора). Расширение — отдельный PR.
//
// `regex` (ADR-047 S2a) — RE2-паттерн по SID/имени хоста, quoted-форма
// `regex='^web-.*'`. В отличие от exact-ключей матчинг — regexp.MatchString
// против host/sid-контекста ([Permission.Matches]).
//
// `soulprint` (ADR-047 S2b) — CEL-предикат по фактам хоста (`soulprint.self.*`,
// ADR-018), quoted-форма `soulprint='soulprint.self.os.family == "debian"'`.
// Компиляция валидируется shared/cel на load; реальный CEL-eval против фактов —
// слайсы S3/S4 ([Permission.Matches] для soulprint в S2b fail-closed: context
// map[string]string не несёт nested facts).
//
// `state` (ADR-047 S2c) — CEL-предикат по incarnation.state, quoted-форма
// `state='state.redis_version == "8.0"'`. Компиляция валидируется через
// keeper/internal/statepredicate (migration-sandbox корень `state`) на load;
// реальный CEL-eval против state — слайс S3b ([Permission.Matches] для state
// fail-closed без incarnation.state в context).
var allowedSelectorKeys = map[string]struct{}{
	"service":     {},
	"coven":       {},
	"incarnation": {},
	"host":        {},
	"regex":       {},
	"soulprint":   {},
	"state":       {},
}

// IsAllowedSelectorKey — проверка ключа селектора против closed enum.
func IsAllowedSelectorKey(key string) bool {
	_, ok := allowedSelectorKeys[key]
	return ok
}

// SelectorKeys возвращает отсортированный список допустимых ключей селектора
// (closed enum [allowedSelectorKeys]). Per-permission-метаданных в каталоге MVP
// нет — это общий список допустимых ключей скоупа, применимый к permission-ам,
// поддерживающим селектор. Используется каталог-эндпоинтом `GET /v1/permissions`
// (selector_keys в выдаче — этот общий список).
func SelectorKeys() []string {
	keys := make([]string, 0, len(allowedSelectorKeys))
	for k := range allowedSelectorKeys {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
