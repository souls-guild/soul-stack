package api

// FULL-TYPED форма READ-каталогов Operator API (code-first источник OpenAPI,
// ADR-054 §Pattern, READ-вариант pilot-1 БЕЗ audit). БАТЧ-1 read-tier тиража:
// три bare-GET-каталога без входных параметров — `GET /v1/permissions`,
// `GET /v1/event-types`, `GET /v1/me/permissions`. Go-типы — единственный источник
// правды: huma строит из них И JSON Schema OpenAPI-фрагмента, И typed-output.
//
// Все три — READ без фильтров: input — пустая структура (huma не требует Body/
// Path/Query-полей для bare-GET, как roleListInput). Output — typed Body = alias на
// сгенерированный oapi-reply (тот же тип, что отдавал legacy writeJSON), поэтому
// wire-байты идентичны; huma лишь сериализует уже собранный handler-ом срез.
// omitempty/[]-vs-null держат сами oapi-типы — golden-JSON snapshot фиксирует это
// байт-в-байт (главный guard read-tier).

import (
	"net/http"

	"github.com/danielgtaylor/huma/v2"
)

// === GET /v1/permissions — каталог RBAC-permissions ===

// permissionsListInput — huma-input GET /v1/permissions. Параметров нет (каталог
// без фильтров) — пустая структура (parity roleListInput).
type permissionsListInput struct{}

// permissionsListOutput — huma-output GET /v1/permissions (FULL-TYPED). Body —
// typed 200-тело (huma-native api.PermissionCatalogReply, T5b — конверт legacy-генерата→native
// в register-func). Wire-форма (items non-nil, сортировка resource/action,
// selector_keys) зафиксирована golden-JSON snapshot-тестом.
type permissionsListOutput struct {
	Body PermissionCatalogReply
}

// permissionsListOperation — метаданные GET /v1/permissions. Path = "/permissions"
// относительно chi-группы /v1 (huma.API смонтирован на ней; chi.Walk видит роут
// /v1/permissions, drift-test зелёный). Абсолютный (а не "/") — три каталога живут
// на ОДНОЙ huma.API/спека-дампе, distinct-path исключает коллизию операций (в
// отличие от cadence/role, где distinct-path даёт сама форма `/`+`/{name}`).
// DefaultStatus=200. READ-роут: audit НЕ навешан.
func permissionsListOperation() huma.Operation {
	return huma.Operation{
		OperationID:   "listPermissions",
		Method:        http.MethodGet,
		Path:          "/permissions",
		Summary:       "Каталог RBAC-permissions",
		Description:   "Машиночитаемый каталог `<resource>.<action>` (источник rbac.catalog.go), сгруппированный по resource. Auth-only, без отдельной permission (само-описывающий). Read-only, без audit.",
		Tags:          []string{"permission"},
		DefaultStatus: http.StatusOK,
		Errors:        []int{http.StatusInternalServerError},
	}
}

// === GET /v1/event-types — каталог event-types для Tiding-подписки ===

// eventTypesListInput — huma-input GET /v1/event-types. Параметров нет — пустая
// структура (parity roleListInput).
type eventTypesListInput struct{}

// eventTypesListOutput — huma-output GET /v1/event-types (FULL-TYPED). Body —
// typed 200-тело (huma-native api.EventTypeCatalogReply). Wire-форма (areas/
// point_events non-nil, area-glob `<name>.*`) зафиксирована golden-JSON snapshot-
// тестом.
type eventTypesListOutput struct {
	Body EventTypeCatalogReply
}

// eventTypesListOperation — метаданные GET /v1/event-types. Path = "/event-types"
// относительно chi-группы /v1 (абсолютный — distinct-path на общей huma.API/дампе,
// см. permissionsListOperation). DefaultStatus=200. READ-роут: audit НЕ навешан.
func eventTypesListOperation() huma.Operation {
	return huma.Operation{
		OperationID:   "listEventTypes",
		Method:        http.MethodGet,
		Path:          "/event-types",
		Summary:       "Каталог event-types для Tiding-подписки",
		Description:   "Допустимые для подписки Tiding типы: areas (area-glob `<name>.*`) + точечные point_events (источник herald/eventtypes.go). Auth-only, без отдельной permission (само-описывающий). Read-only, без audit.",
		Tags:          []string{"event-type"},
		DefaultStatus: http.StatusOK,
		Errors:        []int{http.StatusInternalServerError},
	}
}

// === GET /v1/herald-types — каталог типов Herald-канала ===

// heraldTypesListInput — huma-input GET /v1/herald-types. Параметров нет — пустая
// структура (parity roleListInput).
type heraldTypesListInput struct{}

// heraldTypesListOutput — huma-output GET /v1/herald-types (FULL-TYPED). Body —
// typed 200-тело (huma-native api.HeraldTypeCatalogReply). Wire-форма (types/fields
// non-nil, сортировка типов как AllHeraldTypes) зафиксирована golden-JSON snapshot-
// тестом.
type heraldTypesListOutput struct {
	Body HeraldTypeCatalogReply
}

// heraldTypesListOperation — метаданные GET /v1/herald-types. Path = "/herald-types"
// относительно chi-группы /v1 (абсолютный — distinct-path на общей huma.API/дампе,
// см. permissionsListOperation). DefaultStatus=200. READ-роут: audit НЕ навешан.
func heraldTypesListOperation() huma.Operation {
	return huma.Operation{
		OperationID:   "listHeraldTypes",
		Method:        http.MethodGet,
		Path:          "/herald-types",
		Summary:       "Каталог типов Herald-канала",
		Description:   "Типы канала уведомлений и их config-поля (webhook/telegram/slack/mattermost/discord/custom/email): name/label/required/secret/kind. Источник — herald.TypeCatalog (тот же, что валидирует CRUD). Auth-only, без отдельной permission (само-описывающий). Read-only, без audit.",
		Tags:          []string{"herald"},
		DefaultStatus: http.StatusOK,
		Errors:        []int{http.StatusInternalServerError},
	}
}

// === GET /v1/me/permissions — эффективные права текущего Архонта ===

// myPermissionsListInput — huma-input GET /v1/me/permissions. Параметров нет (AID
// берётся из claims, НЕ из query) — пустая структура (parity roleListInput).
type myPermissionsListInput struct{}

// myPermissionsListOutput — huma-output GET /v1/me/permissions (FULL-TYPED). Body —
// typed 200-тело (huma-native api.MyPermissionsReply). Wire-форма (permissions
// non-nil, pointer-optional, snake_case scope-ключи) зафиксирована golden-JSON
// snapshot-тестом.
type myPermissionsListOutput struct {
	Body MyPermissionsReply
}

// myPermissionsListOperation — метаданные GET /v1/me/permissions. Path =
// "/me/permissions" относительно chi-группы /v1 (абсолютный — distinct-path на общей
// huma.API/дампе, см. permissionsListOperation). DefaultStatus=200. READ-роут: audit
// НЕ навешан. 500 — claims нет в context (auth-chain не собрана, серверная ошибка).
func myPermissionsListOperation() huma.Operation {
	return huma.Operation{
		OperationID:   "listMyPermissions",
		Method:        http.MethodGet,
		Path:          "/me/permissions",
		Summary:       "Эффективные права текущего Архонта",
		Description:   "Подмножество каталога, реально выданное текущему оператору (AID из JWT-claims). Auth-only (свои права видит любой аутентифицированный; чужие не отдаёт). Read-only, без audit.",
		Tags:          []string{"permission"},
		DefaultStatus: http.StatusOK,
		Errors:        []int{http.StatusInternalServerError},
	}
}
