// My-permissions-handler Operator API (`GET /v1/me/permissions`) — публикует
// эффективные права ТЕКУЩЕГО Архонта (из JWT-claims), для permission-aware UI:
// показывать/прятать кнопки по «можно ли resource.action [в каком scope]».
//
// Отличие от `GET /v1/permissions` ([PermissionCatalogHandler]): тот отдаёт ВЕСЬ
// каталог возможных прав (источник rbac.catalog.go), этот — ПОДМНОЖЕСТВО,
// реально выданное текущему оператору (распаковка его ролей через
// [rbac.Enforcer.PermissionsOf]).
//
// RBAC — только аутентификация (валидный JWT), БЕЗ отдельной permission: эндпоинт
// само-описывающий «свои права» (любой аутентифицированный видит ИМЕННО СВОИ
// права; чужие не отдаёт — AID берётся из claims, не из query). Симметрично
// каталогу — read-only, без audit (паттерн health/meta / permissions-каталог).
package handlers

import (
	"io"
	"log/slog"

	"github.com/souls-guild/soul-stack/keeper/internal/rbac"
)

// PermissionsLister — узкое подмножество rbac-поверхности, нужное handler-у:
// распаковка эффективных прав AID. Реализуется [rbac.Enforcer] и [rbac.Holder]
// (через RBACProvider в server.go). Узкий интерфейс (а не *rbac.Enforcer) —
// чтобы handler-тест мог подставить лёгкий fake без сборки полного снимка
// (паттерн PermissionChecker в middleware).
type PermissionsLister interface {
	PermissionsOf(aid string) []rbac.EffectivePermission
}

// MyPermissionsHandler — `GET /v1/me/permissions`. Состояние не держит (enforcer
// immutable-снимок); safe for concurrent use.
type MyPermissionsHandler struct {
	enforcer PermissionsLister
	logger   *slog.Logger
}

// NewMyPermissionsHandler создаёт handler. logger nil → io.Discard.
func NewMyPermissionsHandler(enforcer PermissionsLister, logger *slog.Logger) *MyPermissionsHandler {
	if logger == nil {
		logger = slog.New(slog.NewJSONHandler(io.Discard, nil))
	}
	return &MyPermissionsHandler{enforcer: enforcer, logger: logger}
}

// MyScope — ПЛОСКАЯ scope-сводка одного права (handler-native T5d): либо
// unrestricted, либо набор конкретных ограничений по измерениям. Пакет api
// проецирует пустые измерения в nil-указатели (omitempty).
type MyScope struct {
	Unrestricted bool
	Covens       []string
	Regex        []string
	Soulprint    []string
	State        []string
}

// MyPermission — ПЛОСКОЕ доменное эффективное право (handler-native T5d). Wildcard=true
// — cluster-admin (`*`): resource/action пусты, scope не несётся. Пакет api проецирует
// в pointer-optional native-форму.
type MyPermission struct {
	Wildcard bool
	Resource string
	Action   string
	Scope    *MyScope
}

// MyPermissions — ПЛОСКОЕ доменное тело `GET /v1/me/permissions` (handler-native T5d).
type MyPermissions struct {
	Permissions []MyPermission
}

// GetTyped — доменная функция `GET /v1/me/permissions` (handler-native T5d, READ без
// audit): распаковка эффективных прав AID без http.ResponseWriter/*http.Request. aid
// приходит аргументом (извлечение из claims — на вызывающем слое). Ошибок нет →
// возвращает только значение (native-проекция в api строит pointer-optional wire).
// Wire-форма (permissions non-nil, snake_case scope-ключи) сохранена — golden
// фиксирует её байт-в-байт.
func (h *MyPermissionsHandler) GetTyped(aid string) MyPermissions {
	eff := h.enforcer.PermissionsOf(aid)
	perms := make([]MyPermission, 0, len(eff))
	for _, p := range eff {
		perms = append(perms, toMyPermission(p))
	}
	return MyPermissions{Permissions: perms}
}

// toMyPermission конвертирует [rbac.EffectivePermission] в ПЛОСКУЮ доменную форму.
// Wildcard → маркер без scope; иначе resource.action + scope-сводка.
func toMyPermission(p rbac.EffectivePermission) MyPermission {
	if p.Wildcard {
		return MyPermission{Wildcard: true}
	}
	return MyPermission{
		Resource: p.Resource,
		Action:   p.Action,
		Scope: &MyScope{
			Unrestricted: p.Scope.Unrestricted,
			Covens:       p.Scope.Covens,
			Regex:        p.Scope.Regexes,
			Soulprint:    p.Scope.SoulprintExprs,
			State:        p.Scope.StateExprs,
		},
	}
}
