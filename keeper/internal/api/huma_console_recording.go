package api

// Registration and spec-dump of the CONSOLE-RECORDING domain (ADR-0074(g),
// NIM-148) — the playback half of mandatory console session recording.
//
// The wire DTOs live here rather than being generated: this domain was born
// handler-native, so the native types below ARE the schema, built from the
// handler's flat domain views.

import (
	"context"
	"time"

	"github.com/danielgtaylor/huma/v2"

	"github.com/souls-guild/soul-stack/keeper/internal/api/handlers"
	apimiddleware "github.com/souls-guild/soul-stack/keeper/internal/api/middleware"
	"github.com/souls-guild/soul-stack/keeper/internal/api/problem"
)

// ConsoleRecording — the native wire shape of one recorded session's metadata.
//
// The cast body is NOT a field: it is fetched from the `/cast` sub-route, so a
// list of a hundred sessions does not carry a hundred terminal recordings.
type ConsoleRecording struct {
	ArchonAID   string     `json:"archon_aid" pattern:"^[a-z0-9][a-z0-9._@-]{1,127}$" doc:"the Archon who held the session"`
	ByteCount   int64      `json:"byte_count" doc:"encoded size of the cast body in bytes"`
	CloseReason *string    `json:"close_reason,omitempty"`
	EventCount  int64      `json:"event_count"`
	FinishedAt  *time.Time `json:"finished_at,omitempty" doc:"absent while the session is live, and also when the Keeper instance holding it died mid-recording — the body up to that point is complete and replayable"`
	Height      uint32     `json:"height" doc:"terminal rows from the cast header"`
	Kind        string     `json:"kind" enum:"interactive,command" doc:"interactive = a PTY session over GET /v1/console; command = a one-shot keeper.soul.run-command"`
	RecordingID string     `json:"recording_id" pattern:"^[0-9A-HJKMNP-TV-Z]{26}$"`
	SessionID   string     `json:"session_id"`
	SID         string     `json:"sid" pattern:"^[a-z0-9][a-z0-9.-]{0,253}$" doc:"the host the session ran on"`
	StartedAt   time.Time  `json:"started_at"`
	Truncated   bool       `json:"truncated" doc:"the session hit the per-session recording cap and was closed; the recording ends before the shell did"`
	Width       uint32     `json:"width" doc:"terminal columns from the cast header"`
}

// ConsoleRecordingListReply — the native 200 envelope of the list route.
type ConsoleRecordingListReply struct {
	Items  []ConsoleRecording `json:"items"`
	Offset int                `json:"offset"`
	Limit  int                `json:"limit"`
	Total  int                `json:"total"`
}

// registerHumaConsoleRecordingList mounts GET /v1/console/recordings (READ with
// typed query, no audit). h nil → no-op.
func registerHumaConsoleRecordingList(humaAPI huma.API, h *handlers.ConsoleRecordingHandler) {
	if h == nil {
		return
	}
	huma.Register(humaAPI, consoleRecordingListOperation(), func(ctx context.Context, in *consoleRecordingListInput) (*consoleRecordingListOutput, error) {
		claims, ok := apimiddleware.ClaimsFromContext(ctx)
		if !ok {
			return nil, consoleRecordingMissingClaims()
		}
		page, err := h.ListTyped(ctx, claims, toConsoleRecordingListInput(in))
		if err != nil {
			return nil, consoleRecordingProblem(err)
		}
		return &consoleRecordingListOutput{Body: newConsoleRecordingListReply(page)}, nil
	})
}

// registerHumaConsoleRecordingGet mounts GET /v1/console/recordings/{recording_id}
// (READ with path, no audit). h nil → no-op.
func registerHumaConsoleRecordingGet(humaAPI huma.API, h *handlers.ConsoleRecordingHandler) {
	if h == nil {
		return
	}
	huma.Register(humaAPI, consoleRecordingGetOperation(), func(ctx context.Context, in *consoleRecordingGetInput) (*consoleRecordingGetOutput, error) {
		claims, ok := apimiddleware.ClaimsFromContext(ctx)
		if !ok {
			return nil, consoleRecordingMissingClaims()
		}
		view, err := h.GetTyped(ctx, claims, in.RecordingID)
		if err != nil {
			return nil, consoleRecordingProblem(err)
		}
		return &consoleRecordingGetOutput{Body: newConsoleRecording(view)}, nil
	})
}

// registerHumaConsoleRecordingCast mounts
// GET /v1/console/recordings/{recording_id}/cast (READ + audit, streamed).
// h nil → no-op.
//
// Authorization and the audit write happen HERE, before the handler returns —
// while a status can still be set and before a single byte of the session has
// left. Only the copy itself runs inside the stream callback.
func registerHumaConsoleRecordingCast(humaAPI huma.API, h *handlers.ConsoleRecordingHandler) {
	if h == nil {
		return
	}
	huma.Register(humaAPI, consoleRecordingCastOperation(), func(ctx context.Context, in *consoleRecordingCastInput) (*huma.StreamResponse, error) {
		claims, ok := apimiddleware.ClaimsFromContext(ctx)
		if !ok {
			return nil, consoleRecordingMissingClaims()
		}
		rec, err := h.PrepareCast(ctx, claims, in.RecordingID)
		if err != nil {
			return nil, consoleRecordingProblem(err)
		}
		return &huma.StreamResponse{Body: func(hctx huma.Context) {
			hctx.SetHeader("Content-Type", castContentType)
			// A cast is a file to hand to a player, not a page to render, and
			// it holds whatever the operator typed — an inline disposition
			// would let a browser interpret it in the API's origin.
			hctx.SetHeader("Content-Disposition", `attachment; filename="`+rec.RecordingID+`.cast"`)
			w := hctx.BodyWriter()
			h.StreamCast(hctx.Context(), rec, func(b []byte) error {
				_, werr := w.Write(b)
				return werr
			})
		}}, nil
	})
}

// newConsoleRecording projects the flat handler view onto the wire type.
func newConsoleRecording(v handlers.ConsoleRecordingView) ConsoleRecording {
	return ConsoleRecording{
		ArchonAID:   v.ArchonAID,
		ByteCount:   v.ByteCount,
		CloseReason: v.CloseReason,
		EventCount:  v.EventCount,
		FinishedAt:  v.FinishedAt,
		Height:      v.Height,
		Kind:        v.Kind,
		RecordingID: v.RecordingID,
		SessionID:   v.SessionID,
		SID:         v.SID,
		StartedAt:   v.StartedAt,
		Truncated:   v.Truncated,
		Width:       v.Width,
	}
}

// newConsoleRecordingListReply projects the domain page onto the wire envelope.
// Items is a non-nil empty slice on an empty page, so the JSON carries `[]`
// rather than `null` (the errand-list convention).
func newConsoleRecordingListReply(p handlers.ConsoleRecordingListPage) ConsoleRecordingListReply {
	items := make([]ConsoleRecording, 0, len(p.Items))
	for i := range p.Items {
		items = append(items, newConsoleRecording(p.Items[i]))
	}
	return ConsoleRecordingListReply{Items: items, Limit: p.Limit, Offset: p.Offset, Total: p.Total}
}

// consoleRecordingMissingClaims — defensive: RequireJWT puts claims in before
// huma, so this is unreachable through the router.
func consoleRecordingMissingClaims() huma.StatusError {
	return humaProblemError{Details: problem.New(problem.TypeInternalError, "", "missing claims")}
}

// consoleRecordingProblem delivers a domain error through huma as problem+json.
func consoleRecordingProblem(err error) huma.StatusError {
	if d, ok := handlers.AsProblemDetails(err); ok {
		return humaProblemError{Details: d}
	}
	return humaProblemError{Details: problem.New(problem.TypeInternalError, "", "internal error")}
}

// HumaConsoleRecordingSpecYAML assembles the OpenAPI fragment of the domain for
// the spec merge target and the guard tests.
func HumaConsoleRecordingSpecYAML() (string, error) {
	return humaDumpSpec(func(api huma.API) error {
		stub := handlers.ConsoleRecordingSpecStub()
		registerHumaConsoleRecordingList(api, stub)
		registerHumaConsoleRecordingGet(api, stub)
		registerHumaConsoleRecordingCast(api, stub)
		return nil
	})
}
