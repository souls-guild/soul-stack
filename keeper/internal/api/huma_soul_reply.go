package api

// HUMA-NATIVE reply-DTO of the SOUL domain (Teardown T5b, pattern pilot T5a —
// huma_incarnation_reply.go). Reply/output Body of the huma soul operations — a native Go struct in
// the api package, NOT the legacy-generated one. Boundary/invariants — see the header of huma_incarnation_reply.go.
//
// SCOPE T5b (final, 4 archetypes of the architect pattern). Reply Bodies of the soul domain have been
// migrated to huma-native:
//   - SoulCreateReply (POST /v1/souls)            — flat, the enum status/transport stays legacy-generated;
//   - SoulIssueTokenReply (POST .../issue-token)  — flat scalars;
//   - SoulSshTargetReply (PUT .../ssh-target)     — class-A reuse of nested SoulSshTarget (the same
//     native type as the input PUT body); the rename-alias SoulSSHTargetReply is removed (the native Body
//     provides the schema itself);
//   - SoulListEntry (GET /v1/souls/{sid})         — shared get-Body + envelope-element: one
//     native type, get-Body is native; the envelope alias key PagedResponse[SoulListEntry]
//     is UNTOUCHED (resolves the handler's wire type into the native envelope → native element);
//   - SoulHistoryReply + SoulHistoryItem (GET .../history) — a nested envelope (NOT generic).
//
// NAME/SHAPE 1:1: enum fields — NATIVE SoulStatus/SoulTransport (huma_enums.go, T5d-2c-full
// Phase 1) — the alias aliasSoulStatusTransport exposes them as a named schema with $ref (the native types
// implement SchemaProvider themselves, huma_soul_status.go). SoulHistoryItem.type — native SoulHistoryItemType
// (the reference declares the enum INLINE inside SoulHistoryItem, NOT as a standalone schema → no alias needed,
// huma inlines it as `type: string`). The projection of the domain handlers.Soul*View → native casts
// status/transport plain-string → native enum (byte-exact: the same string). covens/bootstrap_token/
// expires_at — *-optional WITH omitempty (nil → key omitted).
//
// OUTPUT-PATTERN (documentation-only, NOT runtime validation): huma does NOT validate the
// response body (empirically 200, not 500). sid ← soul.SIDPattern; created_by_aid ←
// operator.AIDPattern; SoulHistoryItem.id / voyage_id — machine-generated ULID (id = apply_id|
// errand_id, voyage_id = audit.NewULID); covens[] ← soul.CovenPattern (per-element,
// batch 5 — output covens on Soul* View/Reply). The format is for client codegen; the pattern does not
// affect json.Marshal (the golden byte-exact stays intact). Reply types are output-only (create/
// issue-token have separate *Request/*Input types) → no input-422 risk.

import (
	"time"

	"github.com/souls-guild/soul-stack/keeper/internal/api/handlers"
)

// === top-level reply-DTO (shape 1:1 with the legacy-generated type) ===

// SoulCreateReply — the native 201 body of POST /v1/souls. Shape 1:1 with SoulCreateReply:
// bootstrap_token/covens/expires_at — *-optional WITH omitempty (present only for
// transport=agent; nil → key omitted); status/transport — oapi-enum ($ref via alias);
// registered_at — nanosecond time-wire.
type SoulCreateReply struct {
	BootstrapToken *string       `json:"bootstrap_token,omitempty"`
	Covens         *[]string     `json:"covens,omitempty" pattern:"^[a-z][a-z0-9]*(-[a-z0-9]+)*$"` // ← soul.CovenPattern (per-element)
	CreatedByAID   string        `json:"created_by_aid" pattern:"^[a-z0-9][a-z0-9._@-]{1,127}$"`   // ← operator.AIDPattern
	ExpiresAt      *time.Time    `json:"expires_at,omitempty"`
	RegisteredAt   time.Time     `json:"registered_at"`
	SID            string        `json:"sid" pattern:"^[a-z0-9][a-z0-9.-]{0,253}$"` // ← soul.SIDPattern
	Status         SoulStatus    `json:"status"`
	Transport      SoulTransport `json:"transport"`
}

// SoulIssueTokenReply — the native 200 body of POST /v1/souls/{sid}/issue-token. Shape 1:1 with
// SoulIssueTokenReply: bootstrap_token/expires_at/sid (all required); expires_at —
// nanosecond time-wire.
type SoulIssueTokenReply struct {
	BootstrapToken string    `json:"bootstrap_token"`
	ExpiresAt      time.Time `json:"expires_at"`
	SID            string    `json:"sid" pattern:"^[a-z0-9][a-z0-9.-]{0,253}$"` // ← soul.SIDPattern
}

// SoulSshTargetReply — the native 200 body of PUT /v1/souls/{sid}/ssh-target (CLASS A, reuse). Shape
// 1:1 with SoulSSHTargetReply (the reference :6399): sid + ssh_target (a snapshot of the saved
// target), both required. ssh_target — REUSES the existing native SoulSshTarget (the same type as the
// input PUT body, huma_soul_op.go) → one valid SoulSshTarget schema for input↔output.
// Struct name = the contract schema name (huma DefaultSchemaNamer → "SoulSshTargetReply");
// the native Body emits the schema itself — the rename-alias SoulSSHTargetReply → soulSshTargetReply is removed.
type SoulSshTargetReply struct {
	SID       string        `json:"sid" pattern:"^[a-z0-9][a-z0-9.-]{0,253}$"` // ← soul.SIDPattern
	SSHTarget SoulSshTarget `json:"ssh_target"`
}

// SoulListEntry — the native projection of the souls registry (shared get-Body GET /v1/souls/{sid} +
// list-envelope element). covens — []string WITHOUT omitempty (always an array); traits — map WITHOUT
// omitempty (always an object; bare-soul → `{}` via coalesceTraits, ADR-060 read-path);
// created_by_aid/last_seen_at/last_seen_by_kid/requested_at — *-WITHOUT-omitempty (nil → `null`);
// status/transport — oapi-enum ($ref via aliasSoulStatusTransport); registered_at —
// nanosecond time-wire. Struct name = the contract schema name. ★ The envelope element
// references this same schema through the alias key PagedResponse[SoulListEntry] → the get-Body
// and envelope-element schemas are identical (TestFullSpec_NoSchemaCollision).
type SoulListEntry struct {
	Covens        []string       `json:"covens" pattern:"^[a-z][a-z0-9]*(-[a-z0-9]+)*$"` // ← soul.CovenPattern (per-element)
	Traits        map[string]any `json:"traits" doc:"operator-set key→value метки (ADR-060); зonчение — scalar or list of scalars; bare-soul → {}"`
	CreatedByAID  *string        `json:"created_by_aid" pattern:"^[a-z0-9][a-z0-9._@-]{1,127}$"` // ← operator.AIDPattern
	LastSeenAt    *time.Time     `json:"last_seen_at"`
	LastSeenByKid *string        `json:"last_seen_by_kid"`
	RegisteredAt  time.Time      `json:"registered_at"`
	RequestedAt   *time.Time     `json:"requested_at"`
	SID           string         `json:"sid" pattern:"^[a-z0-9][a-z0-9.-]{0,253}$"` // ← soul.SIDPattern
	Status        SoulStatus     `json:"status"`
	Transport     SoulTransport  `json:"transport"`
}

// SoulHistoryReply — the native 200 envelope of GET /v1/souls/{sid}/history (a standalone envelope,
// NOT the generic PagedResponse). Shape 1:1 with SoulHistoryReply (types.gen.go :3302):
// items/limit/offset/total (limit/offset/total — int, parity with the legacy-generated type) + top-level sid (host
// echo). Struct name = the contract schema name.
type SoulHistoryReply struct {
	Items  []SoulHistoryItem `json:"items"`
	Limit  int               `json:"limit"`
	Offset int               `json:"offset"`
	SID    string            `json:"sid" pattern:"^[a-z0-9][a-z0-9.-]{0,253}$"` // ← soul.SIDPattern
	Total  int               `json:"total"`
}

// SoulHistoryItem — the native element of history.items (shape 1:1 with SoulHistoryItem, types.gen.go
// :3276): finished_at/incarnation/module/scenario/voyage_id — *-WITH omitempty (mutually exclusive
// nil fields for type=scenario/errand → key omitted); id/status — string; started_at — nanosecond
// time-wire; type — SoulHistoryItemType (enum INLINE in the reference, huma inlines it as `type: string`,
// no alias needed).
type SoulHistoryItem struct {
	FinishedAt  *time.Time          `json:"finished_at,omitempty"`
	ID          string              `json:"id" pattern:"^[0-9A-HJKMNP-TV-Z]{26}$"` // ULID (apply_id|errand_id)
	Incarnation *string             `json:"incarnation,omitempty"`
	Module      *string             `json:"module,omitempty"`
	Scenario    *string             `json:"scenario,omitempty"`
	StartedAt   time.Time           `json:"started_at"`
	Status      string              `json:"status"`
	Type        SoulHistoryItemType `json:"type"`
	VoyageID    *string             `json:"voyage_id,omitempty" pattern:"^[0-9A-HJKMNP-TV-Z]{26}$"` // ULID (audit.NewULID)
}

// === projection of the domain handlers.Soul*View → native wire-DTO (byte-exact passthrough of the shape) ===

// newSoulCreateReply projects the flat domain handlers.SoulCreateView into native. Covens —
// the handler's non-nil slice → `&covens` (the key is always present, omitempty does not trigger).
// status/transport — a native enum cast (the same underlying string).
func newSoulCreateReply(v handlers.SoulCreateView) SoulCreateReply {
	covens := v.Covens
	return SoulCreateReply{
		BootstrapToken: v.BootstrapToken,
		Covens:         &covens,
		CreatedByAID:   v.CreatedByAID,
		ExpiresAt:      v.ExpiresAt,
		RegisteredAt:   v.RegisteredAt,
		SID:            v.SID,
		Status:         SoulStatus(v.Status),
		Transport:      SoulTransport(v.Transport),
	}
}

func newSoulIssueTokenReply(v handlers.SoulIssueTokenView) SoulIssueTokenReply {
	return SoulIssueTokenReply{
		BootstrapToken: v.BootstrapToken,
		ExpiresAt:      v.ExpiresAt,
		SID:            v.SID,
	}
}

// newSoulSshTargetReply projects the domain handlers.SoulSshTargetView into native (class-A reuse:
// nested ssh_target — the same native SoulSshTarget as the input PUT body; ssh_provider empty →
// omitempty drops the key).
func newSoulSshTargetReply(v handlers.SoulSshTargetView) SoulSshTargetReply {
	return SoulSshTargetReply{
		SID: v.SID,
		SSHTarget: SoulSshTarget{
			SoulPath:    v.SoulPath,
			SSHPort:     v.SSHPort,
			SSHProvider: v.SSHProvider,
			SSHUser:     v.SSHUser,
		},
	}
}

// newSoulListEntry projects the flat domain handlers.SoulListView into the native SoulListEntry
// (shared get-Body + envelope-element). Covens — slice as-is (non-nullable, the handler yields `[]`);
// Traits — map as-is (non-nullable, the handler yields `{}` via coalesceTraits); nullable pointers
// as-is; status/transport — a native enum cast.
func newSoulListEntry(v handlers.SoulListView) SoulListEntry {
	return SoulListEntry{
		Covens:        v.Covens,
		Traits:        v.Traits,
		CreatedByAID:  v.CreatedByAID,
		LastSeenAt:    v.LastSeenAt,
		LastSeenByKid: v.LastSeenByKid,
		RegisteredAt:  v.RegisteredAt,
		RequestedAt:   v.RequestedAt,
		SID:           v.SID,
		Status:        SoulStatus(v.Status),
		Transport:     SoulTransport(v.Transport),
	}
}

func newSoulHistoryItem(v handlers.SoulHistoryItemView) SoulHistoryItem {
	return SoulHistoryItem{
		FinishedAt:  v.FinishedAt,
		ID:          v.ID,
		Incarnation: v.Incarnation,
		Module:      v.Module,
		Scenario:    v.Scenario,
		StartedAt:   v.StartedAt,
		Status:      v.Status,
		Type:        SoulHistoryItemType(v.Type),
		VoyageID:    v.VoyageID,
	}
}

// newSoulHistoryReply projects the domain handlers.SoulHistoryView into native. Items preserve
// nil-vs-empty 1:1 (nil → `null`, [] → `[]`) for the sake of byte-exact output (the handler yields a non-nil []).
func newSoulHistoryReply(v handlers.SoulHistoryView) SoulHistoryReply {
	var items []SoulHistoryItem
	if v.Items != nil {
		items = make([]SoulHistoryItem, len(v.Items))
		for i := range v.Items {
			items[i] = newSoulHistoryItem(v.Items[i])
		}
	}
	return SoulHistoryReply{
		Items:  items,
		Limit:  v.Limit,
		Offset: v.Offset,
		SID:    v.SID,
		Total:  v.Total,
	}
}

// soulStatsReply — the native 200 body of GET /v1/souls/stats (the Souls Overview aggregate).
// The axes by_status/by_transport/by_coven — map string→int (huma inlines them as an object with
// additionalProperties:integer); all fields required (the aggregate is always complete, an empty
// axis → an empty object {}). The by_transport keys — agent/ssh (the domain), the UI
// maps them to pull/push labels.
type soulStatsReply struct {
	ByStatus    map[string]int `json:"by_status"`
	ByTransport map[string]int `json:"by_transport"`
	ByCoven     map[string]int `json:"by_coven"`
	Total       int            `json:"total"`
	StaleCount  int            `json:"stale_count"`
}

// newSoulStatsReply projects the domain handlers.SoulStatsView into native. The maps
// are passed through as-is (the handler guarantees non-nil → the wire carries `{}`, not `null`).
func newSoulStatsReply(v handlers.SoulStatsView) soulStatsReply {
	return soulStatsReply{
		ByStatus:    v.ByStatus,
		ByTransport: v.ByTransport,
		ByCoven:     v.ByCoven,
		Total:       v.Total,
		StaleCount:  v.StaleCount,
	}
}
