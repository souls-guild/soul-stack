package api

// HUMA-NATIVE wire-DTO of the HERALD domain (heralds + tidings; handler-native T5d-2c).
// The reply/output Body of the herald/tiding huma operations — native Go structs in the api package, WITHOUT
// the legacy generator. The handler (handlers/herald.go) returns domain results with flat
// fields; the register-func (huma_herald.go) projects them INTO THESE types directly
// (newHerald / newTiding / newHeraldListReply / newTidingListReply) — there are no more
// legacy-generator → native converters.
//
// INVARIANTS (★ wire byte-exact + ★ schema name is stable): byte-for-byte shape = the former
// legacy generator (same json tags/omitempty/time.Time); the exported struct name = the contract
// schema name (Herald / Tiding / HeraldListReply / TidingListReply — huma
// DefaultSchemaNamer takes reflect.Type.Name()). The ENUM field Type — native HeraldType
// (huma_enums.go, INLINE string-enum, wire — a byte-exact string).
//
// ENVELOPE. HeraldListReply/TidingListReply — NOT a generic PagedResponse[T], but direct
// named structs with Items []Herald / []Tiding (native element). Fields items/offset/limit/
// total → wire byte-exact (offset/limit/total — Go-int parity with the former legacy generator).

// OUTPUT-PATTERN of NAMES (documentation-only, NOT runtime validation): huma does NOT validate
// the response body (empirically 200, not 500). name ← herald.NamePattern (kebab, Herald/Tiding);
// Tiding.herald — an FK to the Herald name by the same herald.NamePattern. A format for client codegen;
// the pattern does not affect json.Marshal (golden byte-exact intact). Output types are not shared with
// the request Body (create/update — separate *Request) → no input-422 risk.

import (
	"time"

	"github.com/souls-guild/soul-stack/keeper/internal/api/handlers"
)

// === top-level reply-DTO (shape 1:1 with the former legacy generator) ===

// Herald — native body for herald create (201) / get (200) / update (200). Shape 1:1 with
// the former Herald: config — a map WITHOUT omitempty; created_by_aid/secret_ref —
// `*string` WITH omitempty (nil → key omitted); enabled — bool; type — HeraldType
// (an enum field, inline schema, wire — a string); created_at/updated_at — nanosecond
// time-wire.
type Herald struct {
	Config       map[string]interface{} `json:"config"`
	CreatedAt    time.Time              `json:"created_at"`
	CreatedByAID *string                `json:"created_by_aid,omitempty"`
	Enabled      bool                   `json:"enabled"`
	Name         string                 `json:"name" pattern:"^[a-z0-9-]{1,63}$"` // ← herald.NamePattern
	SecretRef    *string                `json:"secret_ref,omitempty"`
	Type         HeraldType             `json:"type"`
	UpdatedAt    time.Time              `json:"updated_at"`
}

// Tiding — native body for tiding create (201) / get (200) / update (200). Shape 1:1 with
// the former Tiding: annotations — `*map` WITH omitempty; cadence/created_by_aid/
// ephemeral/incarnation/projection/task/voyage_id — optional pointers WITH omitempty;
// event_types — []string WITHOUT omitempty; only_changes/only_failures/enabled — bool;
// created_at/updated_at — nanosecond time-wire.
type Tiding struct {
	Annotations  *map[string]interface{} `json:"annotations,omitempty"`
	Cadence      *string                 `json:"cadence,omitempty"`
	CreatedAt    time.Time               `json:"created_at"`
	CreatedByAID *string                 `json:"created_by_aid,omitempty"`
	Enabled      bool                    `json:"enabled"`
	Ephemeral    *bool                   `json:"ephemeral,omitempty"`
	EventTypes   []string                `json:"event_types"`
	Herald       string                  `json:"herald" pattern:"^[a-z0-9-]{1,63}$"` // ← herald.NamePattern (FK to heralds.name)
	Incarnation  *string                 `json:"incarnation,omitempty"`
	Name         string                  `json:"name" pattern:"^[a-z0-9-]{1,63}$"` // ← herald.NamePattern
	OnlyChanges  bool                    `json:"only_changes"`
	OnlyFailures bool                    `json:"only_failures"`
	Projection   *[]string               `json:"projection,omitempty"`
	Task         *string                 `json:"task,omitempty"`
	UpdatedAt    time.Time               `json:"updated_at"`
	VoyageID     *string                 `json:"voyage_id,omitempty"`
}

// === envelope reply-DTO (shape 1:1 with the former legacy generator; element → native) ===

// HeraldListReply — native 200 envelope for GET /v1/heralds. Shape 1:1 with the former
// HeraldListReply (items/limit/offset/total; offset/limit/total — Go-int). Items —
// []Herald (native element).
type HeraldListReply struct {
	Items  []Herald `json:"items"`
	Limit  int      `json:"limit"`
	Offset int      `json:"offset"`
	Total  int      `json:"total"`
}

// TidingListReply — native 200 envelope for GET /v1/tidings. Shape 1:1 with the former
// TidingListReply. Items — []Tiding (native element).
type TidingListReply struct {
	Items  []Tiding `json:"items"`
	Limit  int      `json:"limit"`
	Offset int      `json:"offset"`
	Total  int      `json:"total"`
}

// === projection of the handler's domain results → native wire-DTO ===

// newHerald projects the domain handlers.HeraldView into a native Herald. type — native
// enum HeraldType (inline string-enum).
func newHerald(v handlers.HeraldView) Herald {
	return Herald{
		Config:       v.Config,
		CreatedAt:    v.CreatedAt,
		CreatedByAID: v.CreatedByAID,
		Enabled:      v.Enabled,
		Name:         v.Name,
		SecretRef:    v.SecretRef,
		Type:         HeraldType(v.Type),
		UpdatedAt:    v.UpdatedAt,
	}
}

// newTiding projects the domain handlers.TidingView into a native Tiding.
func newTiding(v handlers.TidingView) Tiding {
	return Tiding{
		Annotations:  v.Annotations,
		Cadence:      v.Cadence,
		CreatedAt:    v.CreatedAt,
		CreatedByAID: v.CreatedByAID,
		Enabled:      v.Enabled,
		Ephemeral:    v.Ephemeral,
		EventTypes:   v.EventTypes,
		Herald:       v.Herald,
		Incarnation:  v.Incarnation,
		Name:         v.Name,
		OnlyChanges:  v.OnlyChanges,
		OnlyFailures: v.OnlyFailures,
		Projection:   v.Projection,
		Task:         v.Task,
		UpdatedAt:    v.UpdatedAt,
		VoyageID:     v.VoyageID,
	}
}

// newHeraldListReply projects the domain handlers.HeraldListPage into the native envelope
// HeraldListReply (items non-nil []).
func newHeraldListReply(p handlers.HeraldListPage) HeraldListReply {
	items := make([]Herald, 0, len(p.Items))
	for _, v := range p.Items {
		items = append(items, newHerald(v))
	}
	return HeraldListReply{Items: items, Limit: p.Limit, Offset: p.Offset, Total: p.Total}
}

// newTidingListReply projects the domain handlers.TidingListPage into the native envelope
// TidingListReply (items non-nil []).
func newTidingListReply(p handlers.TidingListPage) TidingListReply {
	items := make([]Tiding, 0, len(p.Items))
	for _, v := range p.Items {
		items = append(items, newTiding(v))
	}
	return TidingListReply{Items: items, Limit: p.Limit, Offset: p.Offset, Total: p.Total}
}
