// Permission-catalog-handler Operator API (`GET /v1/permissions`) — публикует
// машиночитаемый каталог RBAC-permissions (источник правды — rbac.catalog.go,
// closed enum `<resource>.<action>`). Назначение — UI назначения прав роли
// (`PATCH /v1/roles/{name}/permissions`): UI фетчит реальные имена из каталога
// вместо хардкода (фикс бага «unknown_permission» при guessed-имени).
//
// RBAC — только аутентификация (валидный JWT), БЕЗ отдельной permission: каталог
// самоописывающий, требование права на чтение списка прав даёт «курицу-яйцо»
// (оператор не узнает, какое право ему назначить, не имея уже какого-то права).
// Симметрично health/meta нет audit-записи (read-самоописание API).
//
// selector_keys — ОБЩИЙ список допустимых ключей скоупа ([rbac.SelectorKeys]):
// per-permission-метаданных скоупа в каталоге MVP нет, поэтому не выдумываем
// per-permission, отдаём один и тот же общий список для каждого action.
package handlers

import (
	"io"
	"log/slog"
	"sort"
	"strings"

	"github.com/souls-guild/soul-stack/keeper/internal/rbac"
)

// PermissionCatalogHandler — `GET /v1/permissions`. Состояние не держит (каталог
// read-only-статика из пакета rbac); safe for concurrent use.
type PermissionCatalogHandler struct {
	logger *slog.Logger
}

// NewPermissionCatalogHandler создаёт handler. logger nil → io.Discard.
func NewPermissionCatalogHandler(logger *slog.Logger) *PermissionCatalogHandler {
	if logger == nil {
		logger = slog.New(slog.NewJSONHandler(io.Discard, nil))
	}
	return &PermissionCatalogHandler{logger: logger}
}

// Тела ответа — ПЛОСКИЕ доменные типы (handler-native T5d). Пакет api проецирует
// их в native-схему PermissionCatalogReply (register-func). selector_keys — общий
// список допустимых ключей скоупа (см. doc-комментарий пакета).
type (
	// permissionAction — один action в составе resource.
	permissionAction = PermissionActionView
	// permissionResource — группа actions одного resource.
	permissionResource = PermissionCatalogItemView
)

// PermissionActionView — ПЛОСКАЯ доменная запись action (action + selector_keys).
type PermissionActionView struct {
	Action       string
	SelectorKeys []string
}

// PermissionCatalogItemView — ПЛОСКАЯ группа actions одного resource.
type PermissionCatalogItemView struct {
	Resource string
	Actions  []PermissionActionView
}

// PermissionCatalog — ПЛОСКОЕ доменное тело `GET /v1/permissions` (handler-native T5d).
type PermissionCatalog struct {
	Items []PermissionCatalogItemView
}

// ListTyped — доменная функция `GET /v1/permissions` (handler-native T5d, READ без
// audit): собирает каталог без http.ResponseWriter/*http.Request. Каталог — read-only-
// статика из пакета rbac, ошибки невозможны → возвращает только значение (native-
// проекция в api строит wire). Wire-форма (items non-nil, сортировка resource/action)
// сохранена — golden фиксирует её байт-в-байт.
func (h *PermissionCatalogHandler) ListTyped() PermissionCatalog {
	return PermissionCatalog{Items: buildPermissionCatalog()}
}

// buildPermissionCatalog разбирает rbac.AllowedPermissions (ключи
// `<resource>.<action>`) в сгруппированную по resource форму. selector_keys —
// общий rbac.SelectorKeys() для каждого action. Порядок детерминирован.
func buildPermissionCatalog() []permissionResource {
	selectorKeys := rbac.SelectorKeys()

	byResource := make(map[string][]string)
	for name := range rbac.AllowedPermissions {
		// Грамматика каталога — ровно `<resource>.<action>` (action может
		// содержать дефис: `soul.ssh-target-update`). Делим по ПЕРВОЙ точке —
		// resource не содержит точки в MVP-каталоге.
		dot := strings.IndexByte(name, '.')
		if dot < 0 {
			continue // защитно: каталог гарантирует точку, но не падаем на drift
		}
		resource, action := name[:dot], name[dot+1:]
		byResource[resource] = append(byResource[resource], action)
	}

	resources := make([]string, 0, len(byResource))
	for res := range byResource {
		resources = append(resources, res)
	}
	sort.Strings(resources)

	items := make([]permissionResource, 0, len(resources))
	for _, res := range resources {
		actions := byResource[res]
		sort.Strings(actions)
		acts := make([]permissionAction, 0, len(actions))
		for _, a := range actions {
			acts = append(acts, permissionAction{Action: a, SelectorKeys: selectorKeys})
		}
		items = append(items, permissionResource{Resource: res, Actions: acts})
	}
	return items
}
