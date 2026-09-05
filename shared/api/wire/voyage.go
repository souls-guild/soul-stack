// Voyage domain bodies (ADR-043): a Voyage is a unified batch run
// (kind=scenario|command), async by default.
//
// VoyageTarget and VoyageNotify are the nested shapes SHARED with the cadence
// domain, and sharing them is the point: each input domain used to carry its
// own Go type of the same shape, which made huma emit four technical schemas
// (VoyageTargetHumaBody / CadenceTargetHumaBody / ...) instead of the two the
// contract names. VoyageTarget is one type for every input consumer
// (VoyageCreateRequest.Target, CadenceCreateRequest.Target,
// CadencePatchRequest.Target) AND for the output (Voyage.target) - the shapes
// are compatible because every field is optional on both sides.
//
// ★ VoyageTarget's FIELD ORDER IS ALPHABETICAL (coven/incarnations/service/
// sids/where) and must stay so. Once it became an output schema, json.Marshal
// emits keys in Go-field order, and the wire it has to match was produced by a
// generator that sorted them.

package wire

import (
	"time"
)

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

// VoyageCreateRequest — the Go form of the POST /v1/voyages body (code-first source of the schema AND
// validation). Mirrors the domain voyageCreateRequest: the run recipe (kind/scenario_
// name|module/target/input/scheduling/batch*) + notify[]. kind-dependent validation
// (scenario↔scenario_name / command↔module, non-empty target, on_failure/batch_mode enum,
// ranges) is domain (CreateTyped → 422). additionalProperties:false (huma default) →
// unknown field → 400. kind/batch_mode/on_failure — inline enums (the spec does NOT hoist them
// as a standalone schema → the enum-alias mechanism does not apply). The struct name = the contract
// schema name (huma DefaultSchemaNamer takes reflect.Type.Name()) — aligned with the committed
// hand-written spec (rollout N3). The domain VoyageCreateRequest does not reach the spec (the huma input is
// this struct; the oapi type is not used as the huma body).
type VoyageCreateRequest struct {
	Kind         string         `json:"kind" required:"true" enum:"scenario,command" doc:"recipe type of the run"`
	ScenarioName string         `json:"scenario_name,omitempty" doc:"scenario name for kind=scenario"`
	Module       string         `json:"module,omitempty" doc:"module for kind=command"`
	Input        map[string]any `json:"input,omitempty" doc:"recipe parameters"`
	Target       VoyageTarget   `json:"target" required:"true" doc:"declarative target (resolved into a snapshot of units)"`

	Batch                *string    `json:"batch,omitempty" doc:"batch size: N hosts or N%"`
	BatchSize            *int       `json:"batch_size,omitempty" minimum:"1"`
	BatchPercent         *int       `json:"batch_percent,omitempty" minimum:"1" maximum:"100"`
	Concurrency          *int       `json:"concurrency,omitempty" minimum:"1" maximum:"500"`
	BatchMode            string     `json:"batch_mode,omitempty" doc:"barrier (default) | window"`
	DryRun               bool       `json:"dry_run,omitempty" doc:"for kind=command - each per-host Errand asks the module for a Plan instead of Apply; a verb-shell module (core.cmd.shell / core.exec.run) has no pure-read Plan on any host -> 400"`
	ScheduleAt           *time.Time `json:"schedule_at,omitempty" doc:"RFC3339 deferred start"`
	InterBatchIntervalMS *int       `json:"inter_batch_interval_ms,omitempty"`
	InterUnitIntervalMS  *int       `json:"inter_unit_interval_ms,omitempty"`

	MaxFailures   *string `json:"max_failures,omitempty" doc:"failure threshold: N absolute or N%"`
	FailThreshold *int    `json:"fail_threshold,omitempty" minimum:"1"`
	RequireAlive  *bool   `json:"require_alive,omitempty"`
	OnFailure     string  `json:"on_failure,omitempty" doc:"abort | continue (default)"`

	Notify []VoyageNotify `json:"notify,omitempty" doc:"one-time subscriptions for THIS run (ephemeral)"`
}

// VoyageTarget — a declarative run target (CLASS A, shared input↔output). scenario
// mode: incarnations/service; command mode: sids/where; shared coven. All fields optional
// (spec :7455 — no required block). Resolution to a snapshot of units is domain-side (at spawn).
//
// ★ FIELD ORDER = alphabetical (coven/incarnations/service/sids/where), like the generated
// VoyageTarget. Once VoyageTarget became an OUTPUT schema (Voyage.target native, final of T5b
// group 4), json.Marshal emits keys in Go-field order — it MUST match the former
// Voyage.target wire (oapi-codegen sorts fields alphabetically), else golden voyage
// fails on byte-order. For INPUT the order is irrelevant (unmarshal is order-independent).
type VoyageTarget struct {
	Coven        []string `json:"coven,omitempty" doc:"coven labels (scenario env-tag / command host label)"`
	Incarnations []string `json:"incarnations,omitempty" doc:"incarnation names (scenario mode)"`
	Service      string   `json:"service,omitempty" doc:"service name (scenario mode)"`
	SIDs         []string `json:"sids,omitempty" doc:"host SIDs (command mode)"`
	Where        string   `json:"where,omitempty" doc:"CEL predicate as an ADDITION to sids/coven (command mode)"`
}

// VoyageNotify — a one-off subscription to run notifications (CLASS B, shared between input
// bodies). Shape only; runtime validation (herald existence / RBAC herald.read / on-enum) is done
// by the domain prepareNotifyErr. herald is required (spec :7612 — required:[herald]).
type VoyageNotify struct {
	Herald       string         `json:"herald" required:"true" pattern:"^[a-z0-9-]{1,63}$" doc:"herald channel name"`
	On           []string       `json:"on,omitempty" doc:"terminal event types: completed|failed|partial"`
	OnlyFailures *bool          `json:"only_failures,omitempty"`
	OnlyChanges  *bool          `json:"only_changes,omitempty"`
	Annotations  map[string]any `json:"annotations,omitempty"`
	Projection   []string       `json:"projection,omitempty"`
}
