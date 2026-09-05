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
	"github.com/souls-guild/soul-stack/keeper/internal/api/handlers"
)

// === top-level reply-DTO (shape 1:1 with the legacy-generated type) ===

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

// newSoulForgetReply projects the domain handlers.SoulForgetView into native.
// warnings is passed through as-is — the domain already coalesces nil to `[]`
// (soulForgetView), and re-coalescing here would hide a regression there.
func newSoulForgetReply(v handlers.SoulForgetView) SoulForgetReply {
	return SoulForgetReply{
		Broadcast:          v.Broadcast,
		CacheKeysPurged:    v.CacheKeysPurged,
		ChoirVoicesRemoved: v.ChoirVoicesRemoved,
		LocalStreamClosed:  v.LocalStreamClosed,
		MembershipsSevered: v.MembershipsSevered,
		SeedsRevoked:       v.SeedsRevoked,
		SID:                v.SID,
		StatusBefore:       v.StatusBefore,
		BootstrapsBurned:   v.BootstrapsBurned,
		Warnings:           v.Warnings,
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
