package api

// HUMA-NATIVE reply DTOs for the SIGIL-KEY domain (Sigil signing trust-anchor
// key rotation; Teardown T5b following the T5a huma_incarnation_reply.go
// template). Reply/output Body of huma operations — native Go structs in
// package api, NOT generated legacy-generata. Teardown removes oapi/ + the
// handwritten layer: reply Body must become code-first native.
//
// INVARIANTS (★ wire byte-exact + ★ schema name stable): the form is
// byte-for-byte the old legacy-generata (same json tags; introduced_at is a
// nanosecond time-wire value from the handler layer (.UTC().Truncate(Second))).
// EXPORTED struct names are the contractual ones (SigilKeyIntroduceReply /
// SigilKeyListReply / SigilKeyView) → huma DefaultSchemaNamer produces the same
// schema. SigilKeyListReply is NOT a paged envelope (items[] only). Projection
// of domain handlers.SigilKey* results into these types is the register-func
// (huma_sigil_key.go).
//
// STATUS FIELDS — native enum types SigilKeyIntroduceReplyStatus /
// SigilKeyViewStatus (huma_enums.go; per-field string enum, handwritten
// string+enum inline). The handler returns status as a plain string, the
// register-func casts it to the native enum (same underlying string).
//
// OUTPUT-PATTERN (documentational, NOT runtime validation): huma does NOT
// validate the response body (empirically 200, not 500). key_id is
// mechanically hex(sha256) of the public key's SPKI DER (keyIDFromPublic,
// keyservice.go:287; hex.EncodeToString → lowercase 64 chars). The format is
// for client codegen; the pattern doesn't affect json.Marshal (golden intact).

import (
	"time"

	"github.com/souls-guild/soul-stack/keeper/internal/api/handlers"
)

// === top-level reply DTOs (form 1:1 with the old legacy-generata) ===

// SigilKeyIntroduceReply — native 201 body of POST /v1/sigil/keys (form 1:1
// with the old SigilKeyIntroduceReply). No private key. status is the native
// enum SigilKeyIntroduceReplyStatus (wire is a string); introduced_at is a
// nanosecond time-wire value.
type SigilKeyIntroduceReply struct {
	IntroducedAt time.Time                    `json:"introduced_at"`
	IsPrimary    bool                         `json:"is_primary"`
	KeyID        string                       `json:"key_id" pattern:"^[0-9a-f]{64}$"` // hex(sha256) SPKI-DER
	PubkeyPEM    string                       `json:"pubkey_pem"`
	Status       SigilKeyIntroduceReplyStatus `json:"status"`
}

// SigilKeyListReply — native 200 body of GET /v1/sigil/keys (form 1:1 with
// the old SigilKeyListReply). items[] only — NOT a paged envelope.
type SigilKeyListReply struct {
	Items []SigilKeyView `json:"items"`
}

// === nested reply-DTO ===

// SigilKeyView — native projection of an active key (form 1:1 with the old
// SigilKeyView). No vault_ref. status is the native enum SigilKeyViewStatus
// (wire is a string); introduced_at is a nanosecond time-wire value (the
// handler layer truncates it to seconds).
type SigilKeyView struct {
	IntroducedAt time.Time          `json:"introduced_at"`
	IsPrimary    bool               `json:"is_primary"`
	KeyID        string             `json:"key_id" pattern:"^[0-9a-f]{64}$"` // hex(sha256) SPKI-DER
	Status       SigilKeyViewStatus `json:"status"`
}

// === projection of domain handlers.SigilKey* results → native wire DTOs ===

// newSigilKeyIntroduceReply projects the flat domain handlers.SigilKeyIntroduceView
// into native. Status is a native enum cast (same underlying string); the
// handler returns introduced_at as-is (byte-exact with the legacy wire).
func newSigilKeyIntroduceReply(v handlers.SigilKeyIntroduceView) SigilKeyIntroduceReply {
	return SigilKeyIntroduceReply{
		IntroducedAt: v.IntroducedAt,
		IsPrimary:    v.IsPrimary,
		KeyID:        v.KeyID,
		PubkeyPEM:    v.PubkeyPEM,
		Status:       SigilKeyIntroduceReplyStatus(v.Status),
	}
}

// newSigilKeyView projects the flat domain handlers.SigilKeyView into native.
// The handler already truncated introduced_at to seconds (byte-exact with the
// legacy wire).
func newSigilKeyView(v handlers.SigilKeyView) SigilKeyView {
	return SigilKeyView{
		IntroducedAt: v.IntroducedAt,
		IsPrimary:    v.IsPrimary,
		KeyID:        v.KeyID,
		Status:       SigilKeyViewStatus(v.Status),
	}
}

// newSigilKeyListReply projects the domain handlers.SigilKeyListPage into
// native. Items preserve nil-vs-empty 1:1 (nil → null, [] → []) for
// byte-exactness — ListTyped yields a non-nil [] (empty registry → `[]`).
func newSigilKeyListReply(p handlers.SigilKeyListPage) SigilKeyListReply {
	var items []SigilKeyView
	if p.Items != nil {
		items = make([]SigilKeyView, len(p.Items))
		for i := range p.Items {
			items[i] = newSigilKeyView(p.Items[i])
		}
	}
	return SigilKeyListReply{Items: items}
}
