package middleware

import (
	"errors"
	"net/http"

	"github.com/souls-guild/soul-stack/keeper/internal/api/problem"
	"github.com/souls-guild/soul-stack/keeper/internal/rbac"
)

// SelectorExtractor — функция извлечения runtime-context-а из request-а
// для permission-проверки.
//
// Endpoint, не требующий контекста (например, `POST /v1/operators` —
// permission `operator.create` без селектора), передаёт NoSelector.
// Endpoint c контекстом (например, `POST /v1/incarnations`) — closure,
// читающий name из body / path и возвращающий `{"incarnation": name}`.
//
// nil-возврат и пустая map эквивалентны: permission с селектором не
// сматчит, но bare-permission и full-wildcard — да.
type SelectorExtractor func(r *http.Request) map[string]string

// NoSelector — empty-extractor для endpoints без контекстных фильтров.
// Используется на operator endpoints (`POST /v1/operators*`) — permission-ы
// для них (`operator.create` / `operator.revoke` / `operator.issue-token`)
// в rbac.md селекторов не имеют, контекст всегда пуст.
func NoSelector(_ *http.Request) map[string]string { return nil }

// MultiSelectorExtractor — функция извлечения НАБОРА runtime-context-ов из
// request-а для permission-проверки с OR-семантикой по контекстам.
//
// Нужна там, где один ключ селектора имеет МНОЖЕСТВО кандидатов-значений, а
// [rbac.Permission.Matches] оперирует single-value context-ом (`coven=` для
// incarnation = `incarnation.covens ∪ {name}`, ADR-008 amendment a). Каждый
// возвращённый context описывает один кандидат (одна coven-метка плюс
// неизменные `incarnation`/`service`); permission допускается, если матчит
// ХОТЯ БЫ ОДИН из контекстов (см. [RequirePermissionMulti]).
//
// Контракт возвратов:
//   - nil / пустой slice → доступ только bare-/`*`-permission-ам (как
//     nil-возврат у [SelectorExtractor]): ни один coven-/service-scoped не
//     сматчит. Используется, когда экстрактор не смог прочитать данные
//     (incarnation не найдена / битый body) — fail-closed для scoped-ролей.
//   - непустой slice → permission проходит при матче любого контекста.
//
// Внутри одного context-а сохраняется AND-семантика [rbac.Permission.Matches]
// (например, permission `... on coven=prod` И неявно по service не нарушается —
// service-only-permission матчит при любой coven-итерации, coven-only — при
// своей метке).
type MultiSelectorExtractor func(r *http.Request) []map[string]string

// PermissionChecker — узкое подмножество rbac-поверхности, нужное
// middleware-у. Реализуется и [rbac.Enforcer], и [rbac.Holder] (последний
// обёртывает Enforcer с pointer-cmp invalidation для hot-reload-а
// `rbac:`-блока через [config.Store], см. ADR-021 + docs/keeper/config.md).
//
// Сужение нужно для двух целей: (1) unit-тесты могут передать прямой
// Enforcer без подъёма Store; (2) production wire-up в `keeper run` передаёт
// Holder, который пересобирает Enforcer на каждый Reload-swap.
type PermissionChecker interface {
	Check(aid, resource, action string, context map[string]string) error
}

// ActionHolder — узкая поверхность existence-gate read-эндпоинтов (ADR-047 §г
// amendment 2026-06-04), нужная [RequireAction]. Реализуется и [rbac.Enforcer],
// и [rbac.Holder] (как [PermissionChecker]).
//
// Отдельный интерфейс, а не расширение [PermissionChecker]: вопрос другой
// (existence без scope-контекста, без error — bool), сигнатура иная. Узкий
// контракт держит зависимость middleware минимальной и позволяет unit-тесту
// передать прямой Enforcer без Store.
type ActionHolder interface {
	HoldsAction(aid, resource, action string) bool
}

// RequirePermission — middleware-фабрика. Должен использоваться после
// [RequireJWT] (иначе ClaimsFromContext вернёт ok=false → 500-логика, не
// 401: missing JWT — конфиг-ошибка сервера, не пользователя).
//
// При deny возвращает 403 problem+json. При misconfiguration (нет
// claims в context) — 500 (логирование делает caller через server-wide
// recovery; здесь только короткий generic-detail).
func RequirePermission(e PermissionChecker, resource, action string, extractor SelectorExtractor) func(http.Handler) http.Handler {
	if extractor == nil {
		extractor = NoSelector
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			claims, ok := ClaimsFromContext(r.Context())
			if !ok {
				// JWT-middleware не отработал — это ошибка конфигурации
				// chain-а, а не клиента. 500 без подробностей.
				WriteInternal(w, r)
				return
			}
			ctx := extractor(r)
			if err := e.Check(claims.Subject, resource, action, ctx); err != nil {
				// ErrOperatorRevoked → 401 (ADR-014 Amendment 2026-05-27,
				// parity с expired JWT — токен больше не доверенный).
				// ErrPermissionDenied и всё остальное → 403 (внутреннюю
				// диагностику не leak-аем, подробности — в audit-middleware).
				if errors.Is(err, rbac.ErrOperatorRevoked) {
					problem.Write(w, problem.New(problem.TypeOperatorRevokedToken, r.URL.Path,
						"archon "+claims.Subject+" has been revoked"))
					return
				}
				detail := "operator lacks required permission"
				if errors.Is(err, rbac.ErrPermissionDenied) {
					detail = "operator lacks required permission " + resource + "." + action
				}
				problem.Write(w, problem.New(problem.TypeForbidden, r.URL.Path, detail))
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// RequireAction — middleware-фабрика existence-gate read-эндпоинтов (ADR-047 §г
// amendment 2026-06-04). Должен использоваться после [RequireJWT] (как
// [RequirePermission]).
//
// Отличие от [RequirePermission]: тот зовёт scope-aware [PermissionChecker.Check]
// со scope-контекстом из [SelectorExtractor] и режет scoped-оператора, чей
// контекст не сматчил селектор. Read-эндпоинт на этапе middleware ещё НЕ знает
// scope-контекста (host/coven/state резолвятся из строк БД, которых нет до
// фетча), поэтому `Check(...,nil)` дал бы ложный deny для scoped-permission.
// RequireAction спрашивает другое — держит ли оператор действие В ПРИНЦИПЕ
// ([ActionHolder.HoldsAction]); сужение по scope делает handler после фетча
// (per-resource резолверы soulpurview/statepredicate).
//
// Не держит действие → 403 (тот же problem+json-стиль, что [RequirePermission]).
// Missing claims → 500 (паритет: JWT-middleware не отработал = конфиг-ошибка
// chain-а, не клиента).
func RequireAction(h ActionHolder, resource, action string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			claims, ok := ClaimsFromContext(r.Context())
			if !ok {
				WriteInternal(w, r)
				return
			}
			if !h.HoldsAction(claims.Subject, resource, action) {
				problem.Write(w, problem.New(problem.TypeForbidden, r.URL.Path,
					"operator lacks required permission "+resource+"."+action))
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// RequireAnyPermission — middleware-фабрика с OR-семантикой по НАБОРУ
// `<action>` одного resource в едином контексте [SelectorExtractor]. Должен
// использоваться после [RequireJWT] (как [RequirePermission]).
//
// Отличие от [RequirePermissionMulti]: тот OR-ит один `<resource>.<action>` по
// набору *контекстов* (multi-value `coven=`); этот OR-ит несколько
// *permission-имён* `<resource>.<action_i>` в одном контексте. Нужен там, где
// эндпоинт допускает любое из нескольких прав — например, гранулярное
// `cadence.enable` ИЛИ backcompat-грант `cadence.update` на
// `POST /v1/cadences/{id}/enable` (роли со старым `cadence.update` не теряют
// доступ при вводе гранулярных enable/disable, ADR-046 amendment 2026-06-02).
//
// Допуск, если [PermissionChecker.Check] вернул nil ХОТЯ БЫ для одного action.
// При deny возвращает 403 с упоминанием ПЕРВОГО action набора (канонического
// для эндпоинта); при missing claims — 500 (паритет [RequirePermission]).
//
// Side-effect метрики: Check вызывается по разу на каждый action до первого
// allow, поэтому при матче не-первого права счётчик rbac_checks_total{result=
// "deny"} над-считан (паритет over-count у [RequirePermissionMulti]). Набор
// actions короткий (2 для cadence toggle), эффект минорный.
func RequireAnyPermission(e PermissionChecker, resource string, actions []string, extractor SelectorExtractor) func(http.Handler) http.Handler {
	if extractor == nil {
		extractor = NoSelector
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			claims, ok := ClaimsFromContext(r.Context())
			if !ok {
				WriteInternal(w, r)
				return
			}
			ctx := extractor(r)
			var lastErr error
			for _, action := range actions {
				err := e.Check(claims.Subject, resource, action, ctx)
				if err == nil {
					next.ServeHTTP(w, r)
					return
				}
				lastErr = err
				// revoked-AID — короткое замыкание в 401 на любом action
				// (паритет [RequirePermissionMulti], ADR-014 Amendment).
				if errors.Is(err, rbac.ErrOperatorRevoked) {
					problem.Write(w, problem.New(problem.TypeOperatorRevokedToken, r.URL.Path,
						"archon "+claims.Subject+" has been revoked"))
					return
				}
			}
			detail := "operator lacks required permission"
			if errors.Is(lastErr, rbac.ErrPermissionDenied) && len(actions) > 0 {
				detail = "operator lacks required permission " + resource + "." + actions[0]
			}
			problem.Write(w, problem.New(problem.TypeForbidden, r.URL.Path, detail))
		})
	}
}

// RequirePermissionMulti — middleware-фабрика с OR-семантикой по набору
// контекстов из [MultiSelectorExtractor]. Должен использоваться после
// [RequireJWT] (как [RequirePermission]).
//
// Допуск, если [PermissionChecker.Check] вернул nil ХОТЯ БЫ для одного
// контекста. Это приземляет multi-value `coven=` без правки
// [rbac.Permission.Matches] (single-value context): экстрактор разворачивает
// `incarnation.covens ∪ {name}` в по-кандидатный набор контекстов, OR — здесь.
// Пустой набор (extractor вернул nil/[]) → пробуем единственный пустой
// context: проходят только bare-/`*`-permission-ы (fail-closed для
// coven-/service-scoped, как и у nil-возврата [SelectorExtractor]).
//
// При deny возвращает 403; при missing claims — 500 (паритет
// [RequirePermission]).
//
// Side-effect метрики: [PermissionChecker.Check] вызывается по разу на каждый
// контекст набора, а keeper_rbac_checks_total наблюдается ВНУТРИ Check-а
// (Holder-обёртка, keeper/internal/rbac/metrics.go). Поэтому один логический
// incarnation-гейт даёт до N инкрементов счётчика, и при матче не-первого
// контекста — N-1 ложных `deny` перед итоговым `allow`. Это осознанный minor:
// убрать его без no-observe-варианта Check-а нельзя (у middleware нет
// метрик-поверхности — он работает через узкий [PermissionChecker]), а вводить
// её ради нита избыточно. Учитывать при алертинге на rbac_checks_total{result="deny"}:
// для coven-scoped incarnation-эндпоинтов deny над-считан на размер набора контекстов.
func RequirePermissionMulti(e PermissionChecker, resource, action string, extractor MultiSelectorExtractor) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			claims, ok := ClaimsFromContext(r.Context())
			if !ok {
				WriteInternal(w, r)
				return
			}

			var contexts []map[string]string
			if extractor != nil {
				contexts = extractor(r)
			}
			// Пустой набор → одна попытка с пустым context-ом (bare/`*` пройдут,
			// scoped — нет). Это сохраняет fail-closed для scoped-ролей, когда
			// экстрактор не смог приземлить данные (404 / битый body).
			if len(contexts) == 0 {
				contexts = []map[string]string{nil}
			}

			var lastErr error
			for _, ctx := range contexts {
				err := e.Check(claims.Subject, resource, action, ctx)
				if err == nil {
					next.ServeHTTP(w, r)
					return
				}
				lastErr = err
				// ErrOperatorRevoked — короткое замыкание: revoked-AID не
				// «не подходит к одному из context-ов», он deny на любой
				// context. Маппим в 401 сразу (ADR-014 Amendment 2026-05-27).
				if errors.Is(err, rbac.ErrOperatorRevoked) {
					problem.Write(w, problem.New(problem.TypeOperatorRevokedToken, r.URL.Path,
						"archon "+claims.Subject+" has been revoked"))
					return
				}
			}

			detail := "operator lacks required permission"
			if errors.Is(lastErr, rbac.ErrPermissionDenied) {
				detail = "operator lacks required permission " + resource + "." + action
			}
			problem.Write(w, problem.New(problem.TypeForbidden, r.URL.Path, detail))
		})
	}
}
