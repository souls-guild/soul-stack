package api

// HUMA-NATIVE reply-DTO of the PROFILE domain (Cloud Profile CRUD, ADR-017). The struct
// name = the contract schema name. The shape is aligned with the MCP output
// schemaProfileCreateOutput (manifest.go): name/provider/params + cloud_init
// (optional) + created_at + created_by_aid (nullable). params is normalized nil→{} by the
// handler (always an object on the wire).

import (
	"time"
)

// Profile — native profiles-registry record (POST 201 / GET 200 / list-element).
// params — `map` with NO omitempty (handler gives {} when nil); cloud_init /
// created_by_aid — `*string` WITH omitempty; created_at — nanosecond time-wire.
type Profile struct {
	CloudInit    *string                `json:"cloud_init,omitempty"`
	CreatedAt    time.Time              `json:"created_at"`
	CreatedByAID *string                `json:"created_by_aid,omitempty" pattern:"^[a-z0-9][a-z0-9._@-]{1,127}$"` // ← operator.AIDPattern
	Name         string                 `json:"name"`
	Params       map[string]interface{} `json:"params"`
	Provider     string                 `json:"provider"`
}

// ProfileListReply — native 200 body for GET /v1/profiles (offset-envelope).
type ProfileListReply struct {
	Items  []Profile `json:"items"`
	Limit  int       `json:"limit"`
	Offset int       `json:"offset"`
	Total  int       `json:"total"`
}
