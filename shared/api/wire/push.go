// Push-flow bodies (ADR-032): the apply request, its 202 reply, and the run
// list read back from the push registry.

package wire

import (
	"time"
)

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

// PushApplyRequest — the Go form of the POST /v1/push/apply body (code-first source of the schema AND
// validation). inventory (SID[] target hosts) + destiny (<name>@<ref>) + optional
// input/ssh_provider/cleanup_stale_versions. Empty inventory / empty destiny is
// domain validation (422 in ApplyTyped). additionalProperties:false (huma default) →
// unknown body field → 400. The struct name = the contract schema name in OpenAPI (huma
// DefaultSchemaNamer takes reflect.Type.Name() directly) — aligned with the committed
// hand-written spec (rollout N3). The register func projects into native handlers.PushApplyInput
// (toPushApplyInput).
type PushApplyRequest struct {
	Inventory            []string       `json:"inventory" required:"true" doc:"list of target SID (FQDN) hosts (transport: ssh)"`
	Destiny              string         `json:"destiny" required:"true" doc:"reference to Destiny in the form <name>@<ref>"`
	Input                map[string]any `json:"input,omitempty" doc:"input for destiny"`
	SSHProvider          string         `json:"ssh_provider,omitempty" doc:"SshProvider name; defaults to the first registered one"`
	CleanupStaleVersions bool           `json:"cleanup_stale_versions,omitempty" doc:"remove stale soul-binary/module versions in the same SSH session"`
}
