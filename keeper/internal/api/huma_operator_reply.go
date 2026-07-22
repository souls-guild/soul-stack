package api

// HUMA-NATIVE wire-DTO for the OPERATOR domain (handler-native PILOT T5d). The reply/output
// Body of huma operations is a native Go struct in package api, no legacy generator. The handler
// (handlers/operator.go) returns domain results with flat fields; the
// register func (huma_operator.go) projects them INTO THESE types directly — there are no more
// legacy-generator → native converters (the api↔handlers boundary builds the wire-DTO from domain
// fields). Key points:
//
//   - SCHEMA NAME = contractual (OperatorCreateReply / Operator / IssueTokenReply):
//     huma's DefaultSchemaNamer takes reflect.Type.Name() → schema under the same name.
//   - AuthMethod enum field — native OperatorAuthMethod (huma_enums.go, INLINE enum):
//     huma inlines the string-named type as `type: string` without $ref.
//   - ENVELOPE: the list schema element (operatorListReply, huma_operator_envelope.go)
//     references this native Operator; alias to PagedResponse[Operator].
//   - wire SHAPE (json tags/omitempty/date-time/nullable) — categories A-D ADR-051,
//     golden byte-exact pinned by huma_operator_reply_test.go.
//
// OUTPUT-PATTERN (documentation-only, NOT runtime validation): huma does NOT validate
// the response body against the schema (empirically 200, not 500). aid/*_by_aid ←
// operator.AIDPattern — format for client codegen. Migration 058 relaxed AID
// to the current pattern (a superset of the old `archon-*`) → legacy AIDs still
// match, there is no output validation → no 500 risk. The pattern tag does not affect
// json.Marshal (golden byte-exact intact).

import (
	"time"
)

// OperatorCreateReply — native 201 body of POST /v1/operators. SENSITIVE: jwt is returned
// once; the secret-masking middleware strips it from logs/OTel/audit. roles — `*[]string`
// with omitempty (nil → key omitted). created_at — nanosecond time-wire.
type OperatorCreateReply struct {
	AID          string    `json:"aid" pattern:"^[a-z0-9][a-z0-9._@-]{1,127}$"` // ← operator.AIDPattern
	CreatedAt    time.Time `json:"created_at"`
	CreatedByAID string    `json:"created_by_aid" pattern:"^[a-z0-9][a-z0-9._@-]{1,127}$"` // ← operator.AIDPattern
	DisplayName  string    `json:"display_name"`
	JWT          string    `json:"jwt"`
	Roles        *[]string `json:"roles,omitempty"`
}

// Operator — native body of GET /v1/operators/{aid} (and list-envelope element). Shape 1:1
// with the former wire: auth_method — native enum OperatorAuthMethod (schema `type: string`);
// created_by_aid/metadata/revoked_at — with omitempty (nil → key omitted). created_at —
// nanosecond time-wire. created_via — ALWAYS present (NOT NULL DEFAULT 'user'
// in the schema, ADR-058(d)); a flat string with an enum tag (the domain matches the SQL CHECK
// `created_via_valid` and the domain constants operator.CreatedVia*) — for client
// codegen, without a named type (distinguishes the creation source from auth_method).
type Operator struct {
	AID              string                  `json:"aid" pattern:"^[a-z0-9][a-z0-9._@-]{1,127}$"` // ← operator.AIDPattern
	AuthMethod       OperatorAuthMethod      `json:"auth_method"`
	BootstrapInitial bool                    `json:"bootstrap_initial"`
	CreatedAt        time.Time               `json:"created_at"`
	CreatedByAID     *string                 `json:"created_by_aid,omitempty" pattern:"^[a-z0-9][a-z0-9._@-]{1,127}$"` // ← operator.AIDPattern
	CreatedVia       string                  `json:"created_via" enum:"bootstrap,user,ldap,oidc,system"`
	DisplayName      string                  `json:"display_name"`
	Metadata         *map[string]interface{} `json:"metadata,omitempty"`
	RevokedAt        *time.Time              `json:"revoked_at,omitempty"`
}

// IssueTokenReply — native 200 body of POST /v1/operators/{aid}/issue-token. SENSITIVE:
// jwt — never log. expires_at — nanosecond time-wire.
type IssueTokenReply struct {
	AID       string    `json:"aid" pattern:"^[a-z0-9][a-z0-9._@-]{1,127}$"` // ← operator.AIDPattern
	ExpiresAt time.Time `json:"expires_at"`
	JWT       string    `json:"jwt"`
}
