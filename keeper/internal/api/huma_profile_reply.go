package api

// HUMA-NATIVE reply-DTO PROFILE-домена (Cloud Profile CRUD, ADR-017). Имя
// структуры = контрактное имя схемы. Форма выровнена под MCP-output
// schemaProfileCreateOutput (manifest.go): name/provider/params + cloud_init
// (опц.) + created_at + created_by_aid (nullable). params нормализован nil→{}
// handler-ом (на wire всегда объект).

import (
	"time"
)

// Profile — native запись реестра profiles (POST 201 / GET 200 / list-element).
// params — `map` БЕЗ omitempty (handler даёт {} при nil); cloud_init /
// created_by_aid — `*string` С omitempty; created_at — наносекундный time-wire.
type Profile struct {
	CloudInit    *string                `json:"cloud_init,omitempty"`
	CreatedAt    time.Time              `json:"created_at"`
	CreatedByAID *string                `json:"created_by_aid,omitempty" pattern:"^[a-z0-9][a-z0-9._@-]{1,127}$"` // ← operator.AIDPattern
	Name         string                 `json:"name"`
	Params       map[string]interface{} `json:"params"`
	Provider     string                 `json:"provider"`
}

// ProfileListReply — native 200-тело GET /v1/profiles (offset-envelope).
type ProfileListReply struct {
	Items  []Profile `json:"items"`
	Limit  int       `json:"limit"`
	Offset int       `json:"offset"`
	Total  int       `json:"total"`
}
