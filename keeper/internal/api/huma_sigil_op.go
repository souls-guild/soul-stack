package api

// FULL-TYPED форма SIGIL-домена (plugins/sigils allow-list; code-first источник
// OpenAPI, ADR-054 §Pattern). ТИРАЖ-БАТЧ-2a: allow (WRITE+AUDIT plugin.allowed),
// list (read-bare, БЕЗ audit), revoke (WRITE+AUDIT plugin.revoked, тройка path-
// сегментов). Go-типы — единственный источник правды (JSON Schema + валидация +
// typed-output).

import (
	"net/http"

	"github.com/danielgtaylor/huma/v2"
)

// === POST /v1/plugins/sigils (allow) — WRITE+AUDIT plugin.allowed ===

// sigilAllowInput — huma-input POST /v1/plugins/sigils (FULL-TYPED). Body —
// типизированное тело (тройка namespace/name/ref).
type sigilAllowInput struct {
	Body PluginSigilAllowRequest
}

// PluginSigilAllowRequest — Go-форма тела POST /v1/plugins/sigils (code-first источник
// схемы И валидации). Повторяет доменный PluginSigilAllowRequest: тройка
// supply-chain-допуска (namespace/name/ref). Формат сегментов (reSigilSegment) —
// доменная валидация в AllowTyped (422), не huma-схема. required:"true" —
// missing→422; additionalProperties:false → unknown→400. Имя структуры = контрактное
// имя схемы (huma DefaultSchemaNamer; рукопись PluginSigilAllowRequest, N4).
type PluginSigilAllowRequest struct {
	Namespace string `json:"namespace" required:"true" pattern:"^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$" doc:"namespace плагина (тип — cloud/ssh/mod)"`
	Name      string `json:"name" required:"true" pattern:"^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$" doc:"имя плагина (как в manifest.name)"`
	Ref       string `json:"ref" required:"true" pattern:"^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$" doc:"git-tag-ref допуска (стабильная метка, без слешей)"`
}

// sigilAllowOutput — huma-output POST /v1/plugins/sigils (FULL-TYPED). Status=201;
// Body — native 201-тело (PluginSigilAllowReply: namespace/name/ref + sha256
// допущенного бинаря, посчитанный Keeper-ом).
type sigilAllowOutput struct {
	Status int `json:"-"`
	Body   PluginSigilAllowReply
}

// sigilAllowOperation — метаданные POST /v1/plugins/sigils. Path = "/" относительно
// chi-группы /v1/plugins/sigils. DefaultStatus=201. Permission plugin.allow + audit
// plugin.allowed. Errors: 400 unknown/malformed, 403 RBAC, 404 plugin-not-in-cache,
// 409 sigil-already-active, 422 валидация тройки, 500.
func sigilAllowOperation() huma.Operation {
	return huma.Operation{
		OperationID:   "allowPluginSigil",
		Method:        http.MethodPost,
		Path:          "/",
		Summary:       "Допустить плагин (Sigil)",
		Description:   "Заносит (namespace,name,ref) в allow-list целостности плагинов с подписью SHA-256 (ADR-026 S4a). Permission plugin.allow. 404 — плагина нет в кеше host-а. 409 — допуск уже активен.",
		Tags:          []string{"plugin"},
		DefaultStatus: http.StatusCreated,
		Errors:        []int{http.StatusBadRequest, http.StatusForbidden, http.StatusNotFound, http.StatusConflict, http.StatusUnprocessableEntity, http.StatusInternalServerError},
	}
}

// === GET /v1/plugins/sigils (list) — READ-bare (БЕЗ audit) ===

// sigilListInput — huma-input GET /v1/plugins/sigils. Параметров нет (лента без
// фильтров) — пустая структура (parity roleListInput).
type sigilListInput struct{}

// sigilListOutput — huma-output GET /v1/plugins/sigils (FULL-TYPED). Body — native
// 200-тело (PluginSigilListReply: items[] активных допусков без signature/manifest).
// Wire-форма (items non-nil [], RevokedAt nil→пропуск, allowed_at секундной точности)
// зафиксирована golden-JSON snapshot-тестом.
type sigilListOutput struct {
	Body PluginSigilListReply
}

// sigilListOperation — метаданные GET /v1/plugins/sigils. Path = "/" относительно
// chi-группы /v1/plugins/sigils. DefaultStatus=200. READ-роут: audit НЕ навешан.
func sigilListOperation() huma.Operation {
	return huma.Operation{
		OperationID:   "listPluginSigils",
		Method:        http.MethodGet,
		Path:          "/",
		Summary:       "Список активных Sigil-ов",
		Description:   "Лента активных допусков плагинов (без signature/manifest, ADR-026 S4a). Permission plugin.list. Read-only, без audit.",
		Tags:          []string{"plugin"},
		DefaultStatus: http.StatusOK,
		Errors:        []int{http.StatusForbidden, http.StatusInternalServerError},
	}
}

// === DELETE /v1/plugins/sigils/{namespace}/{name}/{ref} (revoke) — WRITE+AUDIT plugin.revoked ===

// sigilRevokeInput — huma-input DELETE /v1/plugins/sigils/{namespace}/{name}/{ref}.
// Три path-сегмента (huma извлекает по `path:"…"`). Формат сегментов
// (reSigilSegment, слеш в ref → 422) — доменная валидация в RevokeTyped. Body нет.
type sigilRevokeInput struct {
	Namespace string `path:"namespace" pattern:"^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$" doc:"namespace плагина"`
	Name      string `path:"name" pattern:"^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$" doc:"имя плагина"`
	Ref       string `path:"ref" pattern:"^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$" doc:"git-tag-ref допуска"`
}

// sigilNoContentOutput — huma-output 204-write-роута revoke. БЕЗ Body (легаси-
// контракт: 204 No Content). huma на output без Body → SetStatus(204) → пустое тело.
type sigilNoContentOutput struct {
	Status int `json:"-"`
}

// sigilRevokeOperation — метаданные DELETE /v1/plugins/sigils/{namespace}/{name}/{ref}.
// DefaultStatus=204. Permission plugin.revoke + audit plugin.revoked. Errors: 403
// RBAC, 404 sigil-not-found, 422 невалидный path-сегмент, 500.
func sigilRevokeOperation() huma.Operation {
	return huma.Operation{
		OperationID:   "revokePluginSigil",
		Method:        http.MethodDelete,
		Path:          "/{namespace}/{name}/{ref}",
		Summary:       "Отозвать Sigil",
		Description:   "Снимает активный допуск (namespace,name,ref) из allow-list (ADR-026 S4a). Permission plugin.revoke. 404 — активной записи нет.",
		Tags:          []string{"plugin"},
		DefaultStatus: http.StatusNoContent,
		Errors:        []int{http.StatusForbidden, http.StatusNotFound, http.StatusUnprocessableEntity, http.StatusInternalServerError},
	}
}
