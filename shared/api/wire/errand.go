// Errand domain bodies (ADR-033): the single-Errand exec result and its list
// envelope.
//
//   - Status is the INLINE enum ErrandResultStatus - a string on the wire, with
//     no $ref.
//   - ErrandListReply carries items/limit/offset/total as Go int.
//   - ErrandAccepted is the 202 body of a running errand-get. keeper pre-seeds
//     its schema separately (huma_errand_accepted.go) because the get route
//     serialises it from a flat domain view rather than reflecting it.
//
// The `pattern` tags are for client codegen and OpenAPI documentation: huma does
// not validate a response body, and a pattern does not affect json.Marshal.

package wire

import (
	"time"
)

// ErrandResult — native element of errand-list / the 200 body of a terminal errand-get.
// Shape 1:1 with the former ErrandResult (field ORDER under oapi byte-order): duration_ms/
// error_message/exit_code/finished_at/output/stderr/stderr_truncated/stdout/
// stdout_truncated — optional pointers WITH omitempty (nil → key omitted); status —
// native enum ErrandResultStatus (inline schema, string on the wire); started_at —
// nanosecond time-wire; finished_at — `*time.Time` omitempty (running → omitted).
type ErrandResult struct {
	DurationMs      *int64                  `json:"duration_ms,omitempty"`
	ErrandID        string                  `json:"errand_id" pattern:"^[0-9A-HJKMNP-TV-Z]{26}$"` // ULID (audit.NewULID)
	ErrorMessage    *string                 `json:"error_message,omitempty"`
	ExitCode        *int32                  `json:"exit_code,omitempty"`
	FinishedAt      *time.Time              `json:"finished_at,omitempty"`
	Module          string                  `json:"module"`
	Output          *map[string]interface{} `json:"output,omitempty"`
	SID             string                  `json:"sid" pattern:"^[a-z0-9][a-z0-9.-]{0,253}$"` // ← soul.SIDPattern
	StartedAt       time.Time               `json:"started_at"`
	StartedByAID    string                  `json:"started_by_aid" pattern:"^[a-z0-9][a-z0-9._@-]{1,127}$"` // ← operator.AIDPattern
	Status          ErrandResultStatus      `json:"status"`
	Stderr          *string                 `json:"stderr,omitempty"`
	StderrTruncated *bool                   `json:"stderr_truncated,omitempty"`
	Stdout          *string                 `json:"stdout,omitempty"`
	StdoutTruncated *bool                   `json:"stdout_truncated,omitempty"`
}

// ErrandListReply — native 200 envelope for GET /v1/errands. Shape 1:1 with the former
// oapi shape (items/limit/offset/total; offset/limit/total are Go int, parity with the
// legacy generator). Items — []ErrandResult (native element). The nil-ness of Items is
// projected by the register func (nil→nil, []→[]) byte-exact with the former.
type ErrandListReply struct {
	Items  []ErrandResult `json:"items"`
	Limit  int            `json:"limit"`
	Offset int            `json:"offset"`
	Total  int            `json:"total"`
}

// ErrandAccepted — native 202 body of errand-get-running (errand_id + status). Shape 1:1
// with the former ErrandAccepted; on the wire it is serialized by the get route's register
// function via json.RawMessage (errandGetOutput.Body). The schema in components/schemas is
// emitted by a separate schema-builder pre-seed (errandAccepted, huma_errand_accepted.go) —
// this type does NOT take part in spec emission, only in the wire serialization of the 202 body.
type ErrandAccepted struct {
	ErrandID string `json:"errand_id"`
	Status   string `json:"status"`
}
