package api

// FULL-TYPED форма PROVIDER-домена (Cloud Provider CRUD, ADR-017, код — источник
// OpenAPI). Операции: create (WRITE+AUDIT provider.created), list
// (read-with-typed-query), get (read-with-path), delete (WRITE+AUDIT
// provider.deleted). БЕЗ update: Provider иммутабелен (смена параметров =
// delete+create, защита от частичной мутации live cloud-spec).
//
// credentials_ref — ПУТЬ (`vault:<path>`), pattern на входе `^vault:` (parity
// MCP schemaProviderCreateInput); сам секрет НЕ резолвится.

import (
	"net/http"

	"github.com/danielgtaylor/huma/v2"
)

// === POST /v1/providers (create) — WRITE+AUDIT provider.created ===

type providerCreateInput struct {
	Body ProviderCreateRequest
}

// ProviderCreateRequest — Go-форма тела POST /v1/providers (code-first схема +
// валидация). name/type — kebab (формат CloudDriver-плагина); region —
// произвольная строка; credentials_ref — vault-ref. additionalProperties=false
// (huma-дефолт) → unknown поле → 400. Доменная валидация формата — в CreateTyped (422).
type ProviderCreateRequest struct {
	Name   string `json:"name" required:"true" pattern:"^[a-z0-9-]{1,63}$" doc:"имя Cloud-Provider-а (kebab)"`
	Type   string `json:"type" required:"true" pattern:"^[a-z0-9-]{1,63}$" doc:"имя CloudDriver-плагина (= plugins.cloud_drivers[].name)"`
	Region string `json:"region" required:"true" doc:"регион провайдера"`
	// credentials_ref XOR credentials (dual-mode, ADR-064): ровно одно. ref —
	// vault-путь (значение НЕ резолвится); credentials — plaintext (keeper пишет
	// в Vault сам). Формат/XOR валидирует сервис (422); pattern снят (условная валидация).
	CredentialsRef string         `json:"credentials_ref,omitempty" doc:"vault-ref до credentials (vault:<path>); XOR с credentials. Значение НЕ резолвится"`
	Credentials    map[string]any `json:"credentials,omitempty" doc:"опц. plaintext cloud-credentials (dual-mode, ADR-064): напр. {access_key, secret_key}; keeper пишет их в Vault сам; XOR с credentials_ref. Требует TLS-фронта (secret_ingest.accept_plaintext)"`
	FQDNSuffix     *string        `json:"fqdn_suffix,omitempty" doc:"суффикс FQDN VM (self-onboard: keeper предсказывает FQDN=<name>-<index>.<fqdn_suffix>). Опущено → self-onboard недоступен"`
}

type providerCreateOutput struct {
	Status int `json:"-"`
	Body   Provider
}

func providerCreateOperation() huma.Operation {
	return huma.Operation{
		OperationID:   "createProvider",
		Method:        http.MethodPost,
		Path:          "/",
		Summary:       "Создать Cloud-Provider",
		Description:   "Заносит Cloud-Provider (реестр providers, ADR-017). Permission provider.create. 409 — name занят. credentials_ref хранится как vault-путь, секрет не резолвится.",
		Tags:          []string{"provider"},
		DefaultStatus: http.StatusCreated,
		Errors:        []int{http.StatusBadRequest, http.StatusForbidden, http.StatusConflict, http.StatusUnprocessableEntity, http.StatusInternalServerError},
	}
}

// === GET /v1/providers (list) — READ-with-typed-query (БЕЗ audit) ===

type providerListInput struct {
	Offset int32 `query:"offset" default:"0" doc:"сдвиг от начала набора, ≥0 (out-of-range → 400)"`
	Limit  int32 `query:"limit" default:"50" doc:"размер страницы 1..1000 (out-of-range → 400)"`
}

type providerListOutput struct {
	Body ProviderListReply
}

func providerListOperation() huma.Operation {
	return huma.Operation{
		OperationID:   "listProviders",
		Method:        http.MethodGet,
		Path:          "/",
		Summary:       "Список Cloud-Provider-ов (paged)",
		Description:   "Реестр Cloud-Provider-ов с пагинацией (ADR-017). Permission provider.read. Read-only, без audit.",
		Tags:          []string{"provider"},
		DefaultStatus: http.StatusOK,
		Errors:        []int{http.StatusBadRequest, http.StatusForbidden, http.StatusInternalServerError},
	}
}

// === GET /v1/providers/{name} (get) — READ-with-path (БЕЗ audit) ===

type providerGetInput struct {
	Name string `path:"name" pattern:"^[a-z0-9-]{1,63}$" doc:"имя Cloud-Provider-а"`
}

type providerGetOutput struct {
	Body Provider
}

func providerGetOperation() huma.Operation {
	return huma.Operation{
		OperationID:   "getProvider",
		Method:        http.MethodGet,
		Path:          "/{name}",
		Summary:       "Карточка Cloud-Provider-а",
		Description:   "Метаданные одного Cloud-Provider-а по имени (ADR-017). Permission provider.read. Read-only, без audit. credentials_ref — путь, секрет не резолвится.",
		Tags:          []string{"provider"},
		DefaultStatus: http.StatusOK,
		Errors:        []int{http.StatusForbidden, http.StatusNotFound, http.StatusUnprocessableEntity, http.StatusInternalServerError},
	}
}

// === DELETE /v1/providers/{name} (delete) — WRITE+AUDIT provider.deleted ===

type providerDeleteInput struct {
	Name string `path:"name" pattern:"^[a-z0-9-]{1,63}$" doc:"имя Cloud-Provider-а"`
}

// providerNoContentOutput — 204 No Content (без Body).
type providerNoContentOutput struct {
	Status int `json:"-"`
}

func providerDeleteOperation() huma.Operation {
	return huma.Operation{
		OperationID:   "deleteProvider",
		Method:        http.MethodDelete,
		Path:          "/{name}",
		Summary:       "Удалить Cloud-Provider",
		Description:   "Удаляет запись Cloud-Provider-а (ADR-017). Permission provider.delete. 404 — записи нет; 409 — есть зависимые Profile-и (FK RESTRICT).",
		Tags:          []string{"provider"},
		DefaultStatus: http.StatusNoContent,
		Errors:        []int{http.StatusForbidden, http.StatusNotFound, http.StatusConflict, http.StatusUnprocessableEntity, http.StatusInternalServerError},
	}
}
