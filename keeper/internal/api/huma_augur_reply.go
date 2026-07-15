package api

// HUMA-NATIVE wire-DTO of the AUGUR domain (omens + rites; handler-native T5d-2c). The
// Reply/output Body of the augur huma ops are native Go structs in package api, no legacy generator.
// The handler (handlers/augur.go) returns domain results with flat fields;
// the register func (huma_augur.go) projects them INTO these types directly (newOmenView /
// newRiteView / newOmenListReply / newRiteListReply) — there are no more
// legacy-generator → native converters.
//
// COMPOSITION. omen create/get → OmenView; omen list → OmenListReply (envelope,
// items[]→OmenView); rite create → RiteView; rite list → RiteListReply (items-only,
// no pagination). delete routes carry no body (204).
//
// NAME/SHAPE 1:1 with the former legacy generator (TestSchemaNames_Augur expects OmenView/RiteView/
// OmenListReply/RiteListReply): the native structs are named EXACTLY per contract, and the shape
// (json tags/omitempty/date-time/nullable/FIELD-ORDER under oapi byte-order)
// is byte-for-byte the same — golden byte-exact is pinned by huma_augur_reply_test.go. The enum
// source_type is native OmenViewSourceType (huma_enums.go, INLINE string-enum, no
// $ref). allow is json.RawMessage byte-passthrough (ADR-051 category D).

// OUTPUT-PATTERN NAMES (documentation-only, NOT runtime validation): huma does NOT validate
// the response body (empirically 200, not 500). name ← augur.NamePattern (kebab); RiteView.omen is
// an FK reference to the same augur.NamePattern (the Omen name). Format is for client codegen; the
// pattern doesn't affect json.Marshal (golden byte-exact intact). Output types aren't shared with
// request Body (create/grant use separate *Request) → no input-422 risk.

import (
	"encoding/json"
	"time"
)

// OmenView — native projection of an omens registry row (create/get + element OmenListReply.items[]).
// Shape 1:1 with the former OmenView: created_by_aid — *string WITH omitempty (nil → key omitted);
// source_type — OmenViewSourceType (inline string-enum, no $ref); created_at —
// nanosecond time-wire.
type OmenView struct {
	AuthRef      string             `json:"auth_ref"`
	CreatedAt    time.Time          `json:"created_at"`
	CreatedByAID *string            `json:"created_by_aid,omitempty"`
	Endpoint     string             `json:"endpoint"`
	Name         string             `json:"name" pattern:"^[a-z0-9-]{1,63}$"` // ← augur.NamePattern
	SourceType   OmenViewSourceType `json:"source_type"`
}

// OmenListReply — native envelope of GET /v1/augur/omens (4-field offset). Shape 1:1 with
// the former OmenListReply: items[]→OmenView; offset/limit/total — int32. A concrete
// named struct (NOT generic PagedResponse), used as the Body directly.
type OmenListReply struct {
	Items  []OmenView `json:"items"`
	Limit  int32      `json:"limit"`
	Offset int32      `json:"offset"`
	Total  int32      `json:"total"`
}

// RiteView — native projection of a rites registry row (create + element RiteListReply.items[]).
// Shape 1:1 with the former RiteView: allow — json.RawMessage (byte-passthrough JSONB); coven/sid/
// created_by_aid/token_num_uses/token_ttl — *-optional WITH omitempty; created_at — nanosecond
// time-wire; id — int64.
type RiteView struct {
	Allow        json.RawMessage `json:"allow"`
	Coven        *string         `json:"coven,omitempty"`
	CreatedAt    time.Time       `json:"created_at"`
	CreatedByAID *string         `json:"created_by_aid,omitempty"`
	Delegate     bool            `json:"delegate"`
	ID           int64           `json:"id"`
	Omen         string          `json:"omen" pattern:"^[a-z0-9-]{1,63}$"` // ← augur.NamePattern (FK to omens.name)
	SID          *string         `json:"sid,omitempty"`
	TokenNumUses *int            `json:"token_num_uses,omitempty"`
	TokenTTL     *string         `json:"token_ttl,omitempty"`
}

// RiteListReply — native body of GET /v1/augur/rites (items-only, list-by-omen without pagination).
// Shape 1:1 with the former RiteListReply: EXACTLY one field items[]→RiteView.
type RiteListReply struct {
	Items []RiteView `json:"items"`
}
