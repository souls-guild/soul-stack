package api

// HUMA-NATIVE wire-DTO of the ORACLE domain (vigils + decrees; handler-native T5d-2c).
// Reply/output Body of huma oracle operations — native Go structs in package api, no legacy generator.
// Handler (handlers/oracle.go) returns domain results with flat fields;
// register-func (huma_oracle.go) projects them directly INTO THESE types (newVigilView /
// newDecreeView / newVigilListReply / newDecreeListReply) — no more legacy-generator → native
// converters.
//
// INVARIANTS (★ wire byte-exact + ★ schema name is stable): the EXPORTED-struct name =
// the contract (VigilView / DecreeView / VigilListReply / DecreeListReply) → huma
// DefaultSchemaNamer yields the same schema; the shape (json tags/omitempty/json.RawMessage
// nil→`null`/time.Time wire/FIELD-ORDER under oapi byte-order) is byte-for-byte the same —
// golden byte-exact pins it in huma_oracle_reply_test.go. params/action_input —
// json.RawMessage byte-passthrough (ADR-051 category D).
//
// ENVELOPE. VigilListReply/DecreeListReply are NOT a generic alias PagedResponse[X] but
// concrete reply types; the Items element field → native ([]VigilView/[]DecreeView),
// the items/offset/limit/total shape 1:1.

// OUTPUT NAME-PATTERN (documentation-only, NOT runtime validation): huma does NOT validate
// the response body (empirically 200, not 500). name ← oracle.NamePattern (kebab, Vigil/Decree);
// on_beacon — FK to a Vigil name by the same oracle.NamePattern; incarnation_name ←
// oracle.IncarnationPattern (same const as INPUT decree.create incarnation_name). Format
// for client codegen; the pattern does not affect json.Marshal (golden byte-exact intact). Output types
// are not shared with the request Body (create — separate *Request) → no input-422 risk. coven is NOT
// tagged: outside this batch's coven scope (Soul*/Incarnation* View), a separate domain.

import (
	"encoding/json"
	"time"

	"github.com/souls-guild/soul-stack/keeper/internal/api/handlers"
)

// === top-level reply-DTO (shape 1:1 with the former legacy generator) ===

// VigilView — native projection of a vigils registry record (shape 1:1 with the former VigilView).
// coven/created_by_aid/sid — `*[]string`/`*string` WITH omitempty (nil → key omitted);
// params — json.RawMessage WITHOUT omitempty (nil → `null`); created_at/updated_at —
// nanosecond time-wire.
type VigilView struct {
	Check        string          `json:"check"`
	Coven        *[]string       `json:"coven,omitempty"`
	CreatedAt    time.Time       `json:"created_at"`
	CreatedByAID *string         `json:"created_by_aid,omitempty"`
	Enabled      bool            `json:"enabled"`
	Interval     string          `json:"interval"`
	Name         string          `json:"name" pattern:"^[a-z0-9-]{1,63}$"` // ← oracle.NamePattern
	Params       json.RawMessage `json:"params"`
	SID          *string         `json:"sid,omitempty"`
	UpdatedAt    time.Time       `json:"updated_at"`
}

// DecreeView — native projection of a decrees registry record (shape 1:1 with the former DecreeView).
// coven/created_by_aid/sid/where — WITH omitempty (nil → key omitted); action_input —
// json.RawMessage WITHOUT omitempty (nil → `null`); created_at/updated_at — nanosecond
// time-wire.
type DecreeView struct {
	ActionInput     json.RawMessage `json:"action_input"`
	ActionScenario  string          `json:"action_scenario"`
	Cooldown        string          `json:"cooldown"`
	Coven           *[]string       `json:"coven,omitempty"`
	CreatedAt       time.Time       `json:"created_at"`
	CreatedByAID    *string         `json:"created_by_aid,omitempty"`
	Enabled         bool            `json:"enabled"`
	IncarnationName string          `json:"incarnation_name" pattern:"^[a-z0-9][a-z0-9-]{0,62}$"` // ← oracle.IncarnationPattern
	Name            string          `json:"name" pattern:"^[a-z0-9-]{1,63}$"`                     // ← oracle.NamePattern
	OnBeacon        string          `json:"on_beacon" pattern:"^[a-z0-9-]{1,63}$"`                // ← oracle.NamePattern (FK to a Vigil name)
	SID             *string         `json:"sid,omitempty"`
	UpdatedAt       time.Time       `json:"updated_at"`
	Where           *string         `json:"where,omitempty"`
}

// === envelope reply-DTO (element field → native, shape 1:1) ===

// VigilListReply — native 200-envelope GET /v1/vigils (shape 1:1 with the former VigilListReply).
// items — []VigilView (native element); offset/limit/total — int32.
type VigilListReply struct {
	Items  []VigilView `json:"items"`
	Limit  int32       `json:"limit"`
	Offset int32       `json:"offset"`
	Total  int32       `json:"total"`
}

// DecreeListReply — native 200-envelope GET /v1/decrees (shape 1:1 with the former DecreeListReply).
type DecreeListReply struct {
	Items  []DecreeView `json:"items"`
	Limit  int32        `json:"limit"`
	Offset int32        `json:"offset"`
	Total  int32        `json:"total"`
}

// === projection of domain handler results → native wire-DTO ===

// newVigilView projects the domain handlers.VigilView (flat fields) into a native VigilView.
func newVigilView(v handlers.VigilView) VigilView {
	return VigilView{
		Check:        v.Check,
		Coven:        v.Coven,
		CreatedAt:    v.CreatedAt,
		CreatedByAID: v.CreatedByAID,
		Enabled:      v.Enabled,
		Interval:     v.Interval,
		Name:         v.Name,
		Params:       v.Params,
		SID:          v.SID,
		UpdatedAt:    v.UpdatedAt,
	}
}

// newDecreeView projects the domain handlers.DecreeView into a native DecreeView.
func newDecreeView(d handlers.DecreeView) DecreeView {
	return DecreeView{
		ActionInput:     d.ActionInput,
		ActionScenario:  d.ActionScenario,
		Cooldown:        d.Cooldown,
		Coven:           d.Coven,
		CreatedAt:       d.CreatedAt,
		CreatedByAID:    d.CreatedByAID,
		Enabled:         d.Enabled,
		IncarnationName: d.IncarnationName,
		Name:            d.Name,
		OnBeacon:        d.OnBeacon,
		SID:             d.SID,
		UpdatedAt:       d.UpdatedAt,
		Where:           d.Where,
	}
}

// newVigilListReply projects the domain handlers.VigilListPage into a native envelope
// VigilListReply (items non-nil [], offset/limit/total int32).
func newVigilListReply(p handlers.VigilListPage) VigilListReply {
	items := make([]VigilView, 0, len(p.Items))
	for _, v := range p.Items {
		items = append(items, newVigilView(v))
	}
	return VigilListReply{Items: items, Limit: int32(p.Limit), Offset: int32(p.Offset), Total: int32(p.Total)}
}

// newDecreeListReply projects the domain handlers.DecreeListPage into a native envelope
// DecreeListReply.
func newDecreeListReply(p handlers.DecreeListPage) DecreeListReply {
	items := make([]DecreeView, 0, len(p.Items))
	for _, d := range p.Items {
		items = append(items, newDecreeView(d))
	}
	return DecreeListReply{Items: items, Limit: int32(p.Limit), Offset: int32(p.Offset), Total: int32(p.Total)}
}
