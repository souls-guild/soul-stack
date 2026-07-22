package api

// HUMA-NATIVE wire-DTO of the CHOIR/VOICE domain (handler-native T5d-2c). The Reply/
// output Body of the choir/voice huma operations is a native Go struct in the api
// package, WITHOUT the legacy generator. The handler (handlers/choir.go) returns domain
// results with flat fields; the register func (huma_choir.go) projects them INTO THESE
// types directly (newChoir / newVoice / newChoirListReply / newVoiceListReply) —
// legacy-generator → native converters no longer exist.
//
// INVARIANTS (★ wire byte-exact + ★ stable schema name): the shape is byte-for-byte the
// former legacy generator's (the same json tags; created_by_aid/description/min_size/
// max_size — `*` WITHOUT omitempty → `null` when nil, category D; time.Time wire;
// envelope int — NOT int32). The EXPORTED-struct name = the contract name (Choir / Voice
// / ChoirListReply / VoiceListReply) → huma DefaultSchemaNamer produces the same schema.
//
// OUTPUT-PATTERN (documentation only, NOT runtime validation): huma does NOT validate
// the response body (empirically 200, not 500). created_by_aid/added_by_aid ←
// operator.AIDPattern; Voice.sid ← soul.SIDPattern; incarnation_name ←
// incarnation.NamePattern (batch 5, FK to incarnation; path-{name} on write). The
// format is for client codegen; the pattern does not affect json.Marshal (golden
// byte-exact stays intact). choir_name is NOT tagged: it has its own grammar
// choir.choirNamePattern (kebab+`_`), outside this batch's name scope.

import (
	"time"

	"github.com/souls-guild/soul-stack/keeper/internal/api/handlers"
)

// === top-level reply DTO (1:1 with the former legacy generator's shape) ===

// Choir — a native projection of a Choir record (1:1 with the former Choir).
// created_by_aid/description/min_size/max_size — `*` WITHOUT omitempty (nil → `null`);
// created_at — nanosecond time wire.
type Choir struct {
	ChoirName       string    `json:"choir_name"`
	CreatedAt       time.Time `json:"created_at"`
	CreatedByAID    *string   `json:"created_by_aid" pattern:"^[a-z0-9][a-z0-9._@-]{1,127}$"` // ← operator.AIDPattern
	Description     *string   `json:"description"`
	IncarnationName string    `json:"incarnation_name" pattern:"^[a-z0-9][a-z0-9-]{0,62}$"` // ← incarnation.NamePattern
	MaxSize         *int      `json:"max_size"`
	MinSize         *int      `json:"min_size"`
}

// Voice — a native projection of Voice membership (1:1 with the former Voice).
// added_by_aid/position/role — `*` WITHOUT omitempty (nil → `null`); added_at —
// nanosecond time wire.
type Voice struct {
	AddedAt         time.Time `json:"added_at"`
	AddedByAID      *string   `json:"added_by_aid" pattern:"^[a-z0-9][a-z0-9._@-]{1,127}$"` // ← operator.AIDPattern
	ChoirName       string    `json:"choir_name"`
	IncarnationName string    `json:"incarnation_name" pattern:"^[a-z0-9][a-z0-9-]{0,62}$"` // ← incarnation.NamePattern
	Position        *int      `json:"position"`
	Role            *string   `json:"role"`
	SID             string    `json:"sid" pattern:"^[a-z0-9][a-z0-9.-]{0,253}$"` // ← soul.SIDPattern
}

// === envelope reply DTO (element field → native, 1:1 shape) ===

// ChoirListReply — the native 200 envelope for GET .../choirs (1:1 with the former
// ChoirListReply). items — []Choir; limit/offset/total — int (NOT int32).
type ChoirListReply struct {
	Items  []Choir `json:"items"`
	Limit  int     `json:"limit"`
	Offset int     `json:"offset"`
	Total  int     `json:"total"`
}

// VoiceListReply — the native 200 envelope for GET .../voices (1:1 with the former
// VoiceListReply).
type VoiceListReply struct {
	Items  []Voice `json:"items"`
	Limit  int     `json:"limit"`
	Offset int     `json:"offset"`
	Total  int     `json:"total"`
}

// === projection of the handler's domain results → native wire DTO ===

// newChoir projects the domain handlers.ChoirView into a native Choir.
func newChoir(v handlers.ChoirView) Choir {
	return Choir{
		ChoirName:       v.ChoirName,
		CreatedAt:       v.CreatedAt,
		CreatedByAID:    v.CreatedByAID,
		Description:     v.Description,
		IncarnationName: v.IncarnationName,
		MaxSize:         v.MaxSize,
		MinSize:         v.MinSize,
	}
}

// newVoice projects the domain handlers.VoiceView into a native Voice.
func newVoice(v handlers.VoiceView) Voice {
	return Voice{
		AddedAt:         v.AddedAt,
		AddedByAID:      v.AddedByAID,
		ChoirName:       v.ChoirName,
		IncarnationName: v.IncarnationName,
		Position:        v.Position,
		Role:            v.Role,
		SID:             v.SID,
	}
}

// newChoirListReply projects the domain handlers.ChoirListPage into the native envelope
// ChoirListReply (items non-nil []; offset 0, limit/total = len — a full list with no
// server-side pagination, parity with the legacy ListChoirs).
func newChoirListReply(p handlers.ChoirListPage) ChoirListReply {
	items := make([]Choir, 0, len(p.Items))
	for _, v := range p.Items {
		items = append(items, newChoir(v))
	}
	return ChoirListReply{Items: items, Offset: 0, Limit: len(items), Total: len(items)}
}

// newVoiceListReply projects the domain handlers.VoiceListPage into the native envelope
// VoiceListReply (items non-nil []; offset 0, limit/total = len).
func newVoiceListReply(p handlers.VoiceListPage) VoiceListReply {
	items := make([]Voice, 0, len(p.Items))
	for _, v := range p.Items {
		items = append(items, newVoice(v))
	}
	return VoiceListReply{Items: items, Offset: 0, Limit: len(items), Total: len(items)}
}
