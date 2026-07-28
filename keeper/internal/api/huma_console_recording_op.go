package api

// FULL-TYPED form of the CONSOLE-RECORDING domain (list + get + cast), the
// playback surface of ADR-0074(g) — NIM-148. Go types are the single source of
// the OpenAPI (ADR-054 §Pattern).
//
// All three routes are READ-only. Two carry no audit; the cast route does, and
// writes it in the handler rather than through the variant-B middleware —
// see [consoleRecordingCastOperation].

import (
	"net/http"
	"time"

	"github.com/danielgtaylor/huma/v2"
)

// castContentType — the media type of an asciicast v2 file.
//
// Not `application/json`: the artifact is a JSON header line followed by one
// JSON array per line, which is not a JSON document. Not
// `application/octet-stream` either — this is the type asciinema, `agg` and the
// xterm.js players already recognize, and naming it is what lets a browser hand
// the file to a player instead of downloading an opaque blob.
const castContentType = "application/x-asciicast"

// === GET /v1/console/recordings (list) — READ with typed query (no audit) ===

// consoleRecordingListInput — huma-input of the list route. offset/limit follow
// the errand convention (int32 + default; out-of-range → 400 via
// CheckPageBounds, not huma minimum/maximum). started_after / started_before are
// date-time — a malformed value is rejected at bind (400) before the handler.
type consoleRecordingListInput struct {
	SID           string    `query:"sid" doc:"filter by recorded host (FQDN); malformed -> 422"`
	ArchonAID     string    `query:"archon_aid" doc:"filter by the Archon whose session was recorded"`
	Kind          string    `query:"kind" enum:"interactive,command" doc:"interactive = a PTY session; command = a one-shot keeper.soul.run-command"`
	StartedAfter  time.Time `query:"started_after" doc:"filter by start (started_at > value, RFC3339); bad value -> 400"`
	StartedBefore time.Time `query:"started_before" doc:"filter by start (started_at < value, RFC3339); bad value -> 400"`
	Offset        int32     `query:"offset" default:"0" doc:"offset from start of set, ≥0 (out-of-range → 400)"`
	Limit         int32     `query:"limit" default:"50" doc:"page size 1..1000 (out-of-range → 400)"`
}

// consoleRecordingListOutput — huma-output of the list route.
type consoleRecordingListOutput struct {
	Body ConsoleRecordingListReply
}

// consoleRecordingListOperation — metadata for GET /v1/console/recordings.
//
// Permission `soul.console`. The route gate is the EXISTENCE gate (router.go),
// because a listing names no host; the per-row scope is applied in SQL from the
// caller's `soul.console` purview, so the page and its total only ever describe
// hosts that caller could open a console on.
func consoleRecordingListOperation() huma.Operation {
	return huma.Operation{
		OperationID: "listConsoleRecordings",
		Method:      http.MethodGet,
		Path:        "/console/recordings",
		Summary:     "List of recorded console sessions (paged)",
		Description: "Recorded console sessions with filters and pagination (ADR-0074(g), NIM-145/NIM-148). " +
			"Permission soul.console — the same right and the same selectors as opening the console itself; " +
			"rows are narrowed to the hosts the caller may console into. Read-only, no audit.",
		Tags:          []string{"console"},
		DefaultStatus: http.StatusOK,
		Errors:        []int{http.StatusBadRequest, http.StatusForbidden, http.StatusUnprocessableEntity, http.StatusInternalServerError},
	}
}

// === GET /v1/console/recordings/{recording_id} (get) — READ with path ===

// consoleRecordingGetInput — huma-input of the metadata route.
type consoleRecordingGetInput struct {
	RecordingID string `path:"recording_id" doc:"ULID of the recording"`
}

// consoleRecordingGetOutput — huma-output of the metadata route.
type consoleRecordingGetOutput struct {
	Body ConsoleRecording
}

// consoleRecordingGetOperation — metadata for GET /v1/console/recordings/{id}.
//
// 404 covers both "no such recording" and "outside your scope": a 403 would
// confirm the recording exists, turning the route into an oracle for which
// hosts have been consoled into.
func consoleRecordingGetOperation() huma.Operation {
	return huma.Operation{
		OperationID: "getConsoleRecording",
		Method:      http.MethodGet,
		Path:        "/console/recordings/{recording_id}",
		Summary:     "Recorded console session (metadata)",
		Description: "Metadata of one recorded session (ADR-0074(g)). Permission soul.console with the recording's host in scope. " +
			"404 covers both an unknown id and one outside the caller's scope. Read-only, no audit.",
		Tags:          []string{"console"},
		DefaultStatus: http.StatusOK,
		Errors:        []int{http.StatusForbidden, http.StatusNotFound, http.StatusUnprocessableEntity, http.StatusInternalServerError},
	}
}

// === GET /v1/console/recordings/{recording_id}/cast — READ + AUDIT ===

// consoleRecordingCastInput — huma-input of the cast route.
type consoleRecordingCastInput struct {
	RecordingID string `path:"recording_id" doc:"ULID of the recording"`
}

// consoleRecordingCastOperation — metadata for the cast route.
//
// The response is streamed ([huma.StreamResponse]), so the body has no Go type
// to derive a schema from and the response is declared by hand. That is honest
// rather than a workaround: an asciicast is a line-delimited stream whose length
// is bounded only by the 256 MiB per-session recording cap, and buffering one
// per concurrent viewer is exactly the memory failure the cap exists to bound.
//
// Audit `console.recording-read` is written by the HANDLER, before the first
// byte, NOT by the variant-B middleware. The middleware writes after `next`
// returns, reading the final status — which for a stream is decided before the
// body exists, so an instance dying mid-stream would disclose the session with
// no record of it. The ordering mirrors the record-before-deliver rule of the
// recording itself (NIM-145).
func consoleRecordingCastOperation() huma.Operation {
	return huma.Operation{
		OperationID: "getConsoleRecordingCast",
		Method:      http.MethodGet,
		Path:        "/console/recordings/{recording_id}/cast",
		Summary:     "Recorded console session (asciicast v2)",
		Description: "The recorded session as an asciicast v2 file — a JSON header line followed by one JSON array per event " +
			"(ADR-0074(g), NIM-145). Replays with asciinema, agg and xterm.js. Permission soul.console with the recording's " +
			"host in scope; audited as console.recording-read. Vault references were masked when the session was recorded and " +
			"are served as masked — there is no un-masked form of this body.",
		Tags:          []string{"console"},
		DefaultStatus: http.StatusOK,
		Responses: map[string]*huma.Response{
			"200": {
				Description: "The asciicast v2 file.",
				Content: map[string]*huma.MediaType{
					castContentType: {Schema: &huma.Schema{Type: huma.TypeString}},
				},
			},
		},
		Errors: []int{http.StatusForbidden, http.StatusNotFound, http.StatusUnprocessableEntity, http.StatusInternalServerError},
	}
}
