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
	"time"

	"github.com/souls-guild/soul-stack/keeper/internal/api/handlers"
)

// === nested leaves (1:1 shape with legacy-generated) ===

// VoyageSummary — native run aggregates (1:1 shape with VoyageSummary, types.gen.go :3973):
// total/succeeded/failed/cancelled — int WITHOUT omitempty (required, hand-written spec :7710); no_match —
// *int WITH omitempty (0/nil → key omitted, like the handler noMatchPtr). The struct name = the contract
// schema name.
type VoyageSummary struct {
	Cancelled int  `json:"cancelled"`
	Failed    int  `json:"failed"`
	NoMatch   *int `json:"no_match,omitempty"`
	Succeeded int  `json:"succeeded"`
	Total     int  `json:"total"`
}

// VoyageTargetEntry — a native voyage_targets row (All-runs drill; 1:1 shape with
// VoyageTargetEntry, types.gen.go :4001): apply_id/errand_id/finished_at — pointer WITH omitempty
// (mutually exclusive kind=scenario/command back-links → key omitted); status/target_kind —
// inline oapi enum (huma inlines `type: string`); batch_index — int; target_id — string.
type VoyageTargetEntry struct {
	ApplyID    *string                     `json:"apply_id,omitempty"`
	BatchIndex int                         `json:"batch_index"`
	ErrandID   *string                     `json:"errand_id,omitempty"`
	FinishedAt *time.Time                  `json:"finished_at,omitempty"`
	Status     VoyageTargetEntryStatus     `json:"status"`
	TargetID   string                      `json:"target_id"`
	TargetKind VoyageTargetEntryTargetKind `json:"target_kind"`
}

// === Voyage (detailed snapshot) — 1:1 shape with Voyage ===

// Voyage — native snapshot of a Voyage run (GET detail / list item; 1:1 shape with Voyage,
// types.gen.go :3787). Fields required by the hand-written spec (:7789) — without omitempty (attempt/current_
// batch_index/dry_run/scope_size/total_batches/started_by_aid/voyage_id/kind/status/created_at);
// pointer-optional WITH omitempty — all nullable fields (batch_*/concurrency/fail_threshold/finished_
// at/module/on_failure/require_alive/scenario_name/schedule_at/started_at/summary/target). enum
// kind/status/batch_mode/on_failure — inline oapi enum (huma `type: string`). Target — REUSES the
// shared api.VoyageTarget (CLASS A; the same schema as the input). Summary — native VoyageSummary.
// date-time — nanosecond wire precision (the handler assigns a bare time.Time WITHOUT .UTC()/Truncate).
// The struct name = the contract schema name.
type Voyage struct {
	Attempt           int              `json:"attempt"`
	BatchMode         *VoyageBatchMode `json:"batch_mode,omitempty"`
	BatchPercent      *int             `json:"batch_percent,omitempty"`
	BatchSize         *int             `json:"batch_size,omitempty"`
	Concurrency       *int             `json:"concurrency,omitempty"`
	CreatedAt         time.Time        `json:"created_at"`
	CurrentBatchIndex int              `json:"current_batch_index"`
	DryRun            bool             `json:"dry_run"`
	FailThreshold     *int             `json:"fail_threshold,omitempty"`
	FinishedAt        *time.Time       `json:"finished_at,omitempty"`
	Kind              VoyageKind       `json:"kind"`
	Module            *string          `json:"module,omitempty"`
	OnFailure         *VoyageOnFailure `json:"on_failure,omitempty"`
	RequireAlive      *bool            `json:"require_alive,omitempty"`
	ScenarioName      *string          `json:"scenario_name,omitempty"`
	ScheduleAt        *time.Time       `json:"schedule_at,omitempty"`
	ScopeSize         int              `json:"scope_size"`
	StartedAt         *time.Time       `json:"started_at,omitempty"`
	StartedByAID      string           `json:"started_by_aid" pattern:"^[a-z0-9][a-z0-9._@-]{1,127}$"` // ← operator.AIDPattern
	Status            VoyageStatus     `json:"status"`
	Summary           *VoyageSummary   `json:"summary,omitempty"`
	Target            *VoyageTarget    `json:"target,omitempty"`
	TotalBatches      int              `json:"total_batches"`
	VoyageID          string           `json:"voyage_id" pattern:"^[0-9A-HJKMNP-TV-Z]{26}$"` // ULID (audit.NewULID)
}

// === wrappers (1:1 shape with legacy-generated) ===

// VoyageListReply — native 200 envelope for GET /v1/voyages (1:1 shape with VoyageListReply,
// types.gen.go :3923): items/offset/limit/total (offset/limit/total — int, parity with legacy-generated);
// items.$ref to native Voyage. ★ SHARED type for voyage-list AND cadence-runs (the latter — via
// a generic envelope alias → api.VoyageListReply, huma_cadence_envelope.go) → one named schema
// VoyageListReply byte-identical in both domains. The struct name = the contract schema name.
type VoyageListReply struct {
	Items  []Voyage `json:"items"`
	Limit  int      `json:"limit"`
	Offset int      `json:"offset"`
	Total  int      `json:"total"`
}

// VoyageTargetsReply — native 200 body for GET /v1/voyages/{id}/targets (1:1 shape with
// VoyageTargetsReply, types.gen.go :4021): voyage_id + targets[] (native VoyageTargetEntry),
// both required. The struct name = the contract schema name.
type VoyageTargetsReply struct {
	Targets  []VoyageTargetEntry `json:"targets"`
	VoyageID string              `json:"voyage_id" pattern:"^[0-9A-HJKMNP-TV-Z]{26}$"` // ULID (audit.NewULID)
}

// VoyageCreateReply — native 202 body for POST /v1/voyages (1:1 shape with VoyageCreateReply,
// types.gen.go :3839): voyage_id/kind/scope_size/status/location (all required). kind/status —
// inline oapi enum (huma `type: string`). The struct name = the contract schema name.
type VoyageCreateReply struct {
	Kind      VoyageCreateReplyKind   `json:"kind"`
	Location  string                  `json:"location"`
	ScopeSize int                     `json:"scope_size"`
	Status    VoyageCreateReplyStatus `json:"status"`
	VoyageID  string                  `json:"voyage_id" pattern:"^[0-9A-HJKMNP-TV-Z]{26}$"` // ULID (audit.NewULID)
}

// VoyagePreviewReply — native 200 body for POST /v1/voyages/preview (1:1 shape with
// VoyagePreviewReply, types.gen.go :3951): kind/scope_size/total_batches/batch_mode
// (required) + effective_batch_size (*int WITH omitempty). kind/batch_mode — inline oapi enum.
// The struct name = the contract schema name.
type VoyagePreviewReply struct {
	BatchMode          VoyagePreviewReplyBatchMode `json:"batch_mode"`
	EffectiveBatchSize *int                        `json:"effective_batch_size,omitempty"`
	Kind               VoyagePreviewReplyKind      `json:"kind"`
	ScopeSize          int                         `json:"scope_size"`
	TotalBatches       int                         `json:"total_batches"`
}

// VoyageCancelReply — native 202 body for DELETE /v1/voyages/{id} (1:1 shape with
// VoyageCancelReply, types.gen.go :3830): voyage_id + status:cancelled (required).
// status — inline oapi enum. The struct name = the contract schema name.
type VoyageCancelReply struct {
	Status   VoyageCancelReplyStatus `json:"status"`
	VoyageID string                  `json:"voyage_id" pattern:"^[0-9A-HJKMNP-TV-Z]{26}$"` // ULID (audit.NewULID)
}

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
