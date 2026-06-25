package api

// HUMA-NATIVE reply-DTO PROVIDER-домена (Cloud Provider CRUD, ADR-017). Имя
// структуры = контрактное имя схемы (huma DefaultSchemaNamer берёт
// reflect.Type.Name()). Форма выровнена под существующий MCP-output
// schemaProviderCreateOutput (manifest.go): name/type/region/credentials_ref +
// created_at (date-time) + created_by_aid (nullable). credentials_ref — ПУТЬ
// (`vault:<path>`), НЕ резолвится (секрет-гигиена).

import (
	"time"
)

// Provider — native запись реестра providers (POST 201 / GET 200 / list-element).
// created_by_aid — `*string` С omitempty (NULL у записей, переживших удаление
// оператора); created_at — наносекундный time-wire.
type Provider struct {
	CreatedAt      time.Time `json:"created_at"`
	CreatedByAID   *string   `json:"created_by_aid,omitempty" pattern:"^[a-z0-9][a-z0-9._@-]{1,127}$"` // ← operator.AIDPattern
	CredentialsRef string    `json:"credentials_ref"`
	Name           string    `json:"name"`
	Region         string    `json:"region"`
	Type           string    `json:"type"`
}

// ProviderListReply — native 200-тело GET /v1/providers (offset-envelope:
// items/offset/limit/total). offset/limit/total — int (parity push-provider-envelope).
type ProviderListReply struct {
	Items  []Provider `json:"items"`
	Limit  int        `json:"limit"`
	Offset int        `json:"offset"`
	Total  int        `json:"total"`
}
