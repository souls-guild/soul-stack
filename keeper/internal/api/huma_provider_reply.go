package api

// HUMA-NATIVE reply-DTO of the PROVIDER domain (Cloud Provider CRUD, ADR-017). The struct
// name = the contract schema name (huma DefaultSchemaNamer takes
// reflect.Type.Name()). The shape is aligned with the existing MCP output
// schemaProviderCreateOutput (manifest.go): name/type/region/credentials_ref +
// created_at (date-time) + created_by_aid (nullable). credentials_ref — a PATH
// (`vault:<path>`), NOT resolved (secret hygiene).

import (
	"time"
)

// Provider — a native providers-registry record (POST 201 / GET 200 / list element).
// created_by_aid — `*string` WITH omitempty (NULL for records that outlived operator
// deletion); created_at — nanosecond time-wire.
type Provider struct {
	CreatedAt      time.Time `json:"created_at"`
	CreatedByAID   *string   `json:"created_by_aid,omitempty" pattern:"^[a-z0-9][a-z0-9._@-]{1,127}$"` // ← operator.AIDPattern
	CredentialsRef string    `json:"credentials_ref"`
	FQDNSuffix     *string   `json:"fqdn_suffix,omitempty"`
	Name           string    `json:"name"`
	Region         string    `json:"region"`
	Type           string    `json:"type"`
}

// ProviderListReply — the native 200 body of GET /v1/providers (offset envelope:
// items/offset/limit/total). offset/limit/total — int (parity with the push-provider envelope).
type ProviderListReply struct {
	Items  []Provider `json:"items"`
	Limit  int        `json:"limit"`
	Offset int        `json:"offset"`
	Total  int        `json:"total"`
}
