package api

// HUMA-NATIVE reply-DTO for the PUSH domain (handler-native T5d-2c-full). The reply/output Body
// of huma operations is a native Go struct in package api, no legacy generator. The register func (huma_push.go)
// projects the handler's flat domain views (PushApplyResultView / PushRunListEntryView)
// directly INTO THESE types — there are no more legacy-generator→native converters. Key points for push:
//
//   - SHAPE byte-for-byte = the former legacy generator (json tags/omitempty/date-time/nullable categories A-D).
//   - SCHEMA NAME = contractual (PushApplyReply / PushApplyView / PushRunListReply /
//     PushRunListEntry / PushSummaryCounts): huma's DefaultSchemaNamer takes
//     reflect.Type.Name() → schema under the same name the former legacy generator produced.
//   - Status enum fields (PushApplyView.Status / PushRunListEntry.Status) — native
//     PushApplyViewStatus / PushRunListEntryStatus (huma_enums.go, INLINE enum): the hand-written
//     code inlines status as `type: string` + enum (no standalone schema), huma inlines a
//     string-named type the same way → schema byte-identical (parity ServiceView.GitRefType).
//   - PushRunListReply — NOT a generic envelope (not sharedapi.PagedResponse) but a plain
//     reply with items[]PushRunListEntry + offset/limit/total (int) → `type: integer`
//     without format (assertOffsetEnvelopeNoFormat). Top-level reply-DTO, not via an alias.
//
// OUTPUT-PATTERN (documentation-only, NOT runtime validation): huma does NOT validate
// the response body (empirically 200, not 500). apply_id — machine ULID (audit.NewULID,
// pushorch/run.go:182); started_by_aid ← operator.AIDPattern. Format for client
// codegen; the pattern does not affect json.Marshal. inventory_sids is NOT tagged:
// a per-element pattern on an array output field is not covered by this batch.

import (
	"time"
)

// === top-level reply-DTO (shape 1:1 with the former legacy generator shape) ===

// PushApplyReply — native 202 body of POST /v1/push/apply (apply_id async). Shape 1:1 with
// PushApplyReply.
type PushApplyReply struct {
	ApplyID string `json:"apply_id" pattern:"^[0-9A-HJKMNP-TV-Z]{26}$"` // ULID (audit.NewULID)
}

// PushApplyView — native 200 body of GET /v1/push/{apply_id}. Shape 1:1 with PushApplyView:
// finished_at/input/ssh_provider/started_by_aid/summary — `*` fields with omitempty (nil → key
// omitted); inventory_sids — array; started_at — nanosecond time-wire; status — enum type
// PushApplyViewStatus (wire string).
type PushApplyView struct {
	ApplyID       string                  `json:"apply_id" pattern:"^[0-9A-HJKMNP-TV-Z]{26}$"` // ULID (audit.NewULID)
	CleanupStale  bool                    `json:"cleanup_stale"`
	DestinyRef    string                  `json:"destiny_ref"`
	FinishedAt    *time.Time              `json:"finished_at,omitempty"`
	Input         *map[string]interface{} `json:"input,omitempty"`
	InventorySids []string                `json:"inventory_sids"`
	SSHProvider   *string                 `json:"ssh_provider,omitempty"`
	StartedAt     time.Time               `json:"started_at"`
	StartedByAID  *string                 `json:"started_by_aid,omitempty" pattern:"^[a-z0-9][a-z0-9._@-]{1,127}$"` // ← operator.AIDPattern
	Status        PushApplyViewStatus     `json:"status"`
	Summary       *map[string]interface{} `json:"summary,omitempty"`
}

// PushRunListReply — native 200 body of GET /v1/push-runs (offset envelope: items/offset/
// limit/total). items — native PushRunListEntry; offset/limit/total — int (parity with the legacy
// generator → `type: integer` without format). Shape 1:1 with PushRunListReply.
type PushRunListReply struct {
	Items  []PushRunListEntry `json:"items"`
	Limit  int                `json:"limit"`
	Offset int                `json:"offset"`
	Total  int                `json:"total"`
}

// === nested reply-DTO ===

// PushRunListEntry — native compact push_runs row (element of PushRunListReply.items).
// Shape 1:1 with PushRunListEntry: finished_at/ssh_provider/started_by_aid/summary_counts —
// `*` fields with omitempty; inventory_sids — array; started_at — nanosecond time-wire;
// status — enum type PushRunListEntryStatus.
type PushRunListEntry struct {
	ApplyID       string                 `json:"apply_id" pattern:"^[0-9A-HJKMNP-TV-Z]{26}$"` // ULID (audit.NewULID)
	CleanupStale  bool                   `json:"cleanup_stale"`
	DestinyRef    string                 `json:"destiny_ref"`
	FinishedAt    *time.Time             `json:"finished_at,omitempty"`
	InventorySids []string               `json:"inventory_sids"`
	SSHProvider   *string                `json:"ssh_provider,omitempty"`
	StartedAt     time.Time              `json:"started_at"`
	StartedByAID  *string                `json:"started_by_aid,omitempty" pattern:"^[a-z0-9][a-z0-9._@-]{1,127}$"` // ← operator.AIDPattern
	Status        PushRunListEntryStatus `json:"status"`
	SummaryCounts *PushSummaryCounts     `json:"summary_counts,omitempty"`
}

// PushSummaryCounts — native counts aggregate (PushRunListEntry.summary_counts). All fields —
// `*int` with omitempty (nil → key omitted). Shape 1:1 with the former PushSummaryCounts.
type PushSummaryCounts struct {
	FailCount    *int `json:"fail_count,omitempty"`
	SuccessCount *int `json:"success_count,omitempty"`
	Total        *int `json:"total,omitempty"`
}
