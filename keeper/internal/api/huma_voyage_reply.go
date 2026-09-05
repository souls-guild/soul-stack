package api

// HUMA-NATIVE reply-DTO of the VOYAGE domain (Teardown T5b, final — batch 4, pilot pattern T5a
// huma_incarnation_reply.go). Reply/output Body of the voyage huma operations — a native Go struct in
// package api, NOT legacy-generated. Boundary/invariants — see the huma_incarnation_reply.go header.
//
// ★ VOYAGE+CADENCE DEDUP IN SYNC. voyage schemas (Voyage/VoyageSummary/VoyageListReply +
// shared VoyageTarget) are duplicated between the voyage domain and cadence-runs (GET /v1/cadences/
// {id}/runs returns the SAME types). TestFullSpec_NoSchemaCollision deduplicates same-named
// schemas ONLY when the body is byte-identical. So voyage AND cadence-runs MUST reference
// ONE native set api.Voyage/api.VoyageListReply: the voyage domain — a direct native Body,
// cadence-runs — a generic envelope alias over the native api.VoyageListReply (huma_cadence_
// envelope.go, registerCadenceEnvelopes). The Voyage body is identical by construction → no collision.
//
// ENUM (architect minor — checked against meta/openapi.yaml :7654-7851). ALL voyage reply enums
// are DECLARED INLINE (`type: string` + `enum:` directly on the property, with NO standalone $ref schema):
//   - Voyage.kind/status/batch_mode/on_failure       → VoyageKind/Status/BatchMode/OnFailure;
//   - VoyageTargetEntry.target_kind/status           → VoyageTargetEntryTargetKind/Status;
//   - VoyageCreateReply.kind/status                  → VoyageCreateReplyKind/Status;
//   - VoyagePreviewReply.kind/batch_mode             → VoyagePreviewReplyKind/BatchMode;
//   - VoyageCancelReply.status                       → VoyageCancelReplyStatus.
// The hand-written spec does NOT declare standalone schemas for them → the enum-alias mechanism
// (like aliasIncarnationStatus) does NOT apply. Fields are a NATIVE enum type (huma_enums.go, T5d-2c-full
// Phase 1) — huma inlines them as `type: string` with an enum set (byte-exact with the former
// legacy-generated Body; native value identical to the oapi value). The converter casts legacy-generated → native
// (value directly, pointer via a helper).
//
// SHARED VoyageTarget (CLASS A). Voyage.target — REUSES the existing native api.VoyageTarget
// (huma_voyage_target.go), the same schema as the voyage/cadence input. We do NOT duplicate the type.
//
// OUTPUT PATTERN (documentation only, NOT runtime validation): huma does NOT validate the
// response body (empirically 200, not 500). voyage_id — machine ULID (audit.NewULID,
// handlers/voyage.go:988); started_by_aid ← operator.AIDPattern. Purely a format for
// client codegen; the pattern does not affect json.Marshal (golden stays intact). VoyageTarget.sids
// is NOT tagged: the type is shared input↔output, and a pattern on INPUT would become a runtime 422.

import (
	"github.com/souls-guild/soul-stack/keeper/internal/api/handlers"
)

// === nested leaves (1:1 shape with legacy-generated) ===

// === Voyage (detailed snapshot) — 1:1 shape with Voyage ===

// === wrappers (1:1 shape with legacy-generated) ===

// === projectors handlers.X → api-native (api↔handlers boundary, byte-exact form passthrough) ===
//
// Handler-native (T5d): the extracted *Typed functions return flat handlers DTOs (plain-string
// enum fields, target pointer-slice — handlers/voyage.go). These projectors convert them INTO api-native
// wire-DTO (named-enum fields → huma schema, value-slice target). There are no more legacy-generated converters —
// the boundary builds the wire-DTO directly from the domain handler fields (pattern from huma_operator_reply.go).

// ptrVoyageBatchMode / ptrVoyageOnFailure — casts a plain-string pointer → native enum WITHOUT losing
// nil-ness (nil → nil, for byte-exact omitempty). The underlying string is identical.
func ptrVoyageBatchMode(s *string) *VoyageBatchMode {
	if s == nil {
		return nil
	}
	v := VoyageBatchMode(*s)
	return &v
}

func ptrVoyageOnFailure(s *string) *VoyageOnFailure {
	if s == nil {
		return nil
	}
	v := VoyageOnFailure(*s)
	return &v
}

func toVoyageSummary(o *handlers.VoyageSummaryDTO) *VoyageSummary {
	if o == nil {
		return nil
	}
	return &VoyageSummary{
		Cancelled: o.Cancelled,
		Failed:    o.Failed,
		NoMatch:   o.NoMatch,
		Succeeded: o.Succeeded,
		Total:     o.Total,
	}
}

// toVoyageTarget — projects the handler-DTO target → native api.VoyageTarget (CLASS A reuse).
// handlers.VoyageTargetDTO carries pointer-slice/pointer-string (*[]string/*string); native —
// value-slice/value-string with omitempty. Dereferencing preserves byte-exactness: nil pointer →
// nil-slice/"" → (omitempty) key omitted. Voyage.target is omitted entirely (gracefully) when the
// domain has no target_origin.
func toVoyageTarget(o *handlers.VoyageTargetDTO) *VoyageTarget {
	if o == nil {
		return nil
	}
	out := &VoyageTarget{}
	if o.Incarnations != nil {
		out.Incarnations = *o.Incarnations
	}
	if o.Service != nil {
		out.Service = *o.Service
	}
	if o.Sids != nil {
		out.SIDs = *o.Sids
	}
	if o.Where != nil {
		out.Where = *o.Where
	}
	if o.Coven != nil {
		out.Coven = *o.Coven
	}
	return out
}

func toVoyage(o handlers.VoyageDTO) Voyage {
	return Voyage{
		Attempt:           o.Attempt,
		BatchMode:         ptrVoyageBatchMode(o.BatchMode),
		BatchPercent:      o.BatchPercent,
		BatchSize:         o.BatchSize,
		Concurrency:       o.Concurrency,
		CreatedAt:         o.CreatedAt,
		CurrentBatchIndex: o.CurrentBatchIndex,
		DryRun:            o.DryRun,
		FailThreshold:     o.FailThreshold,
		FinishedAt:        o.FinishedAt,
		Kind:              VoyageKind(o.Kind),
		Module:            o.Module,
		OnFailure:         ptrVoyageOnFailure(o.OnFailure),
		RequireAlive:      o.RequireAlive,
		ScenarioName:      o.ScenarioName,
		ScheduleAt:        o.ScheduleAt,
		ScopeSize:         o.ScopeSize,
		StartedAt:         o.StartedAt,
		StartedByAID:      o.StartedByAID,
		Status:            VoyageStatus(o.Status),
		Summary:           toVoyageSummary(o.Summary),
		Target:            toVoyageTarget(o.Target),
		TotalBatches:      o.TotalBatches,
		VoyageID:          o.VoyageID,
	}
}

func toVoyageTargetEntry(o handlers.VoyageTargetEntryDTO) VoyageTargetEntry {
	return VoyageTargetEntry{
		ApplyID:    o.ApplyID,
		BatchIndex: o.BatchIndex,
		ErrandID:   o.ErrandID,
		FinishedAt: o.FinishedAt,
		Status:     VoyageTargetEntryStatus(o.Status),
		TargetID:   o.TargetID,
		TargetKind: VoyageTargetEntryTargetKind(o.TargetKind),
	}
}

func toVoyageListReply(o handlers.VoyageListReply) VoyageListReply {
	// Preserve slice nil-ness (nil → wire `null`, [] → `[]`) for byte-exactness.
	var items []Voyage
	if o.Items != nil {
		items = make([]Voyage, len(o.Items))
		for i := range o.Items {
			items[i] = toVoyage(o.Items[i])
		}
	}
	return VoyageListReply{Items: items, Limit: o.Limit, Offset: o.Offset, Total: o.Total}
}

func toVoyageTargetsReply(o handlers.VoyageTargetsReply) VoyageTargetsReply {
	var targets []VoyageTargetEntry
	if o.Targets != nil {
		targets = make([]VoyageTargetEntry, len(o.Targets))
		for i := range o.Targets {
			targets[i] = toVoyageTargetEntry(o.Targets[i])
		}
	}
	return VoyageTargetsReply{Targets: targets, VoyageID: o.VoyageID}
}

func toVoyageCreateReply(o handlers.VoyageCreateReply) VoyageCreateReply {
	return VoyageCreateReply{
		Kind:      VoyageCreateReplyKind(o.Kind),
		Location:  o.Location,
		ScopeSize: o.ScopeSize,
		Status:    VoyageCreateReplyStatus(o.Status),
		VoyageID:  o.VoyageID,
	}
}

func toVoyagePreviewReply(o handlers.VoyagePreviewReply) VoyagePreviewReply {
	return VoyagePreviewReply{
		BatchMode:          VoyagePreviewReplyBatchMode(o.BatchMode),
		EffectiveBatchSize: o.EffectiveBatchSize,
		Kind:               VoyagePreviewReplyKind(o.Kind),
		ScopeSize:          o.ScopeSize,
		TotalBatches:       o.TotalBatches,
	}
}

func toVoyageCancelReply(o handlers.VoyageCancelReply) VoyageCancelReply {
	return VoyageCancelReply{Status: VoyageCancelReplyStatus(o.Status), VoyageID: o.VoyageID}
}
