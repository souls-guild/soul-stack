package api

// HUMA-NATIVE wire-DTO of the ERRAND domain (handler-native T5d-2c-full). The Reply/output
// Body of the huma read routes (list + get) are native Go structs in package api, no legacy
// generator. The handler (handlers/errand.go) returns domain results with FLAT fields
// (ErrandResultView / ErrandListPage); the register func (huma_errand.go) projects them
// INTO THESE types directly — there are no legacy-generator → native converters anymore
// (the api↔handlers boundary builds the wire-DTO from domain fields). Key points:
//
//   - SCHEMA NAME = the contract name (ErrandResult / ErrandListReply): huma's
//     DefaultSchemaNamer takes reflect.Type.Name() → schema under the same name
//     (errand-schema-test pins items.$ref → ErrandResult, envelope → ErrandListReply).
//   - ENUM field Status — native ErrandResultStatus (huma_enums.go, INLINE enum): huma
//     inlines the string-named type as `type: string` without a $ref; a string on the wire.
//   - ENVELOPE: the list-schema element is this native ErrandResult; ErrandListReply carries
//     items/limit/offset/total (Go int, parity with the former oapi shape).
//   - The wire SHAPE (json tags/omitempty/date-time/nullable/field ORDER) is 1:1 with the
//     former legacy generator; golden byte-exact is pinned by huma_errand_reply_test.go.
//   - ErrandAccepted (the 202 body of a running errand-get) is typed separately as a
//     schema-builder pre-seed (errandAccepted, huma_errand_accepted.go); on the wire the
//     get-route register func serializes it from the flat handlers.ErrandAcceptedView.
//
// OUTPUT-PATTERN (documentation-only, NOT runtime validation): huma does NOT validate the
// response body (empirically 200, not 500). errand_id is a machine-generated ULID
// (audit.NewULID, dispatcher.go:262); sid ← soul.SIDPattern; started_by_aid ← operator.AIDPattern.
// The format is for client codegen; the pattern does not affect json.Marshal (golden intact).

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
