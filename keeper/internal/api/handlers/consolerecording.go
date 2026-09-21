package handlers

// Playback of recorded console sessions (ADR-0074 amendment, NIM-148) —
// the read side of the mandatory recording NIM-145 made:
//
//	GET /v1/console/recordings                     — paged list (scope-filtered)
//	GET /v1/console/recordings/{recording_id}      — metadata of one
//	GET /v1/console/recordings/{recording_id}/cast — the asciicast v2 file
//
// The right is `soul.console` with the SAME selectors the live console uses, and
// no lighter one is minted. Watching a recording is reading, verbatim, what an
// operator typed into a root shell and what it printed back — that is the live
// session's content, moved in time. A weaker right for it would make the
// stronger one decorative: an operator refused `soul.console on host=db-01`
// could read every session anyone ever held on db-01. The reasoning is the one
// NIM-147 already applied to `run-command`: dropping the tty (here, dropping the
// live-ness) removes the interactivity, not the privilege.
//
// The check is split exactly as ADR-0074(c) splits it for the WebSocket, and for
// the same reason — the list carries no target host, so the route gate can only
// ask the EXISTENCE question (`RequireAction`, in router.go) and the scope
// question is answered here, where a SID exists. A scope-aware check with an
// absent host dimension would fail closed and deny precisely the `host=`-scoped
// roles this exists to serve (ADR-047 §g G1).
//
// Nothing here touches the body. Masking ran once, at record time, carried
// across chunk boundaries (NIM-145); the cast is served byte-for-byte from
// Postgres. There is no un-masking path because there is nothing to un-mask —
// the plaintext was never written — and there is no re-masking path because a
// second implementation of the same guarantee is the one that drifts.

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"time"

	"github.com/souls-guild/soul-stack/keeper/internal/api/problem"
	"github.com/souls-guild/soul-stack/keeper/internal/consolepg"
	keeperjwt "github.com/souls-guild/soul-stack/keeper/internal/jwt"
	"github.com/souls-guild/soul-stack/keeper/internal/rbac"
	"github.com/souls-guild/soul-stack/keeper/internal/soul"
	"github.com/souls-guild/soul-stack/keeper/internal/soulpurview"
	"github.com/souls-guild/soul-stack/shared/api"
	"github.com/souls-guild/soul-stack/shared/audit"
)

// ConsoleRecordingReader is the read surface of the recording store. Narrow on
// purpose: this handler must not be able to write a recording, and the
// interface is what says so (keeper/internal/consolepg splits Reader from Store
// for the same reason).
type ConsoleRecordingReader interface {
	List(ctx context.Context, f consolepg.ListFilter, offset, limit int) ([]consolepg.Recording, int, error)
	Get(ctx context.Context, recordingID string) (consolepg.Recording, error)
	WriteCast(ctx context.Context, rec consolepg.Recording, write func([]byte) error) error
}

// ConsoleRecordingHandler serves recorded sessions back to an operator.
//
// A nil reader / scoper is fail-closed rather than permissive: an unconfigured
// scoper hides everything (the [SoulHandler] rule), and an unconfigured reader
// is a 500 rather than an empty list, because "no recordings" and "no store" are
// different answers and only one of them is safe to believe.
type ConsoleRecordingHandler struct {
	reader ConsoleRecordingReader
	scoper PurviewResolver
	audit  audit.Writer
	logger *slog.Logger
}

// NewConsoleRecordingHandler creates the handler. auditWriter may be nil (dev
// without audit); reader/scoper are required for production calls.
func NewConsoleRecordingHandler(reader ConsoleRecordingReader, scoper PurviewResolver, auditWriter audit.Writer, logger *slog.Logger) *ConsoleRecordingHandler {
	if logger == nil {
		logger = slog.New(slog.NewJSONHandler(io.Discard, nil))
	}
	return &ConsoleRecordingHandler{reader: reader, scoper: scoper, audit: auditWriter, logger: logger}
}

// ConsoleRecordingSpecStub — a non-empty handler for the OpenAPI fragment dump
// (the [ErrandSpecStub] pattern): huma.Register needs non-nil, and no method is
// called in spec mode.
func ConsoleRecordingSpecStub() *ConsoleRecordingHandler {
	return &ConsoleRecordingHandler{logger: slog.New(slog.NewJSONHandler(io.Discard, nil))}
}

// ConsoleRecordingView — the flat wire shape of one recording's metadata.
//
// Width/Height are lifted out of the cast header so a UI can size its player
// from the list, without fetching a body that may be hundreds of megabytes.
// A nil FinishedAt means the recording has no terminal stamp — the session is
// still live, or the Keeper instance holding it died before it could say
// goodbye (NIM-145). The body up to that point is complete and replayable.
type ConsoleRecordingView struct {
	RecordingID string
	SessionID   string
	Kind        string
	SID         string
	ArchonAID   string
	StartedAt   time.Time
	FinishedAt  *time.Time
	CloseReason *string
	EventCount  int64
	ByteCount   int64
	Truncated   bool
	Width       uint32
	Height      uint32
}

// ConsoleRecordingListPage — domain paged result of the list route.
type ConsoleRecordingListPage struct {
	Items  []ConsoleRecordingView
	Offset int
	Limit  int
	Total  int
}

// ConsoleRecordingListInput — validated input of GET /v1/console/recordings.
type ConsoleRecordingListInput struct {
	SID           string
	ArchonAID     string
	Kind          string
	StartedAfter  time.Time
	StartedBefore time.Time
	Offset        int
	Limit         int
}

// ListTyped returns the recordings the caller may open, newest first.
//
// "May open" is the SAME boundary the live console draws: the operator's
// `soul.console` purview, pushed into SQL so pagination and the total stay exact
// (the souls-list pattern). An operator entitled to no host gets an empty page,
// never the whole table — the purview renders to `FALSE`, and there is
// deliberately no branch around it that could drift the other way.
func (h *ConsoleRecordingHandler) ListTyped(ctx context.Context, claims *keeperjwt.Claims, in ConsoleRecordingListInput) (ConsoleRecordingListPage, error) {
	var zero ConsoleRecordingListPage
	if h.reader == nil {
		return zero, &problemError{problem.New(problem.TypeInternalError, "", "console recording store is not configured")}
	}
	if err := api.CheckPageBounds(in.Offset, in.Limit); err != nil {
		return zero, &problemError{problem.New(problem.TypeMalformedRequest, "", err.Error())}
	}
	filter, err := in.filter()
	if err != nil {
		return zero, err
	}

	scope := h.scopeFor(claims)
	filter.Scope = func(startIdx int) (string, []any, int) {
		return scope.WhereSQL(consoleRecordingScopeColumns, startIdx)
	}

	rows, total, err := h.reader.List(ctx, filter, in.Offset, in.Limit)
	if err != nil {
		h.logger.Error("console.recording.list: store failed", slog.Any("error", err))
		return zero, &problemError{problem.New(problem.TypeInternalError, "", "list console recordings failed")}
	}

	items := make([]ConsoleRecordingView, 0, len(rows))
	for i := range rows {
		items = append(items, newConsoleRecordingView(rows[i]))
	}
	return ConsoleRecordingListPage{Items: items, Offset: in.Offset, Limit: in.Limit, Total: total}, nil
}

// GetTyped returns one recording's metadata.
func (h *ConsoleRecordingHandler) GetTyped(ctx context.Context, claims *keeperjwt.Claims, recordingID string) (ConsoleRecordingView, error) {
	rec, err := h.load(ctx, claims, recordingID)
	if err != nil {
		return ConsoleRecordingView{}, err
	}
	return newConsoleRecordingView(rec), nil
}

// PrepareCast authorizes a cast fetch and records it, returning the recording
// the caller may then stream with [ConsoleRecordingHandler.StreamCast].
//
// The two halves are separate because the body is streamed: everything that can
// still become an HTTP status — the scope refusal above all — must happen while
// a status can still be set, and the audit event must be written BEFORE the
// first byte leaves. That order mirrors the record-before-deliver rule the Hub
// holds on the live session (NIM-145): a disclosure that happened must not
// depend on the handler surviving long enough to report it.
//
// The audit write is best-effort in the same sense `console.opened` is — a
// failure is logged, not turned into a refusal — because the audit path is not
// the control that makes this safe; the right and its scope are.
func (h *ConsoleRecordingHandler) PrepareCast(ctx context.Context, claims *keeperjwt.Claims, recordingID string) (consolepg.Recording, error) {
	rec, err := h.load(ctx, claims, recordingID)
	if err != nil {
		return consolepg.Recording{}, err
	}
	h.writeAudit(ctx, claims, rec)
	return rec, nil
}

// StreamCast writes the asciicast v2 file of an already-authorized recording.
//
// An error here cannot become a status — the headers are long gone — so it is
// logged and the stream ends short. A truncated cast still replays up to where
// it stops, which is the same degradation a recording whose Keeper instance
// died already has.
func (h *ConsoleRecordingHandler) StreamCast(ctx context.Context, rec consolepg.Recording, write func([]byte) error) {
	if err := h.reader.WriteCast(ctx, rec, write); err != nil {
		h.logger.Error("console.recording.cast: stream failed",
			slog.String("recording_id", rec.RecordingID), slog.Any("error", err))
	}
}

// load fetches one recording and applies the scope boundary.
//
// Out of scope is the SAME 404 an unknown id gets, deliberately: a 403 would
// confirm that a recording with that id exists, turning the route into an
// oracle for which hosts have been consoled into and by whom. The souls
// single-object reads answer the same way for the same reason.
func (h *ConsoleRecordingHandler) load(ctx context.Context, claims *keeperjwt.Claims, recordingID string) (consolepg.Recording, error) {
	var zero consolepg.Recording
	if h.reader == nil {
		return zero, &problemError{problem.New(problem.TypeInternalError, "", "console recording store is not configured")}
	}
	if recordingID == "" {
		return zero, &problemError{problem.New(problem.TypeValidationFailed, "", "path 'recording_id' is required")}
	}

	rec, err := h.reader.Get(ctx, recordingID)
	if err != nil {
		if errors.Is(err, consolepg.ErrNotFound) {
			return zero, consoleRecordingNotFound(recordingID)
		}
		h.logger.Error("console.recording.get: store failed",
			slog.String("recording_id", recordingID), slog.Any("error", err))
		return zero, &problemError{problem.New(problem.TypeInternalError, "", "get console recording failed")}
	}

	scope := h.scopeFor(claims)
	if !soulpurview.InScope(scope, rec.SID, rec.Covens, soulpurview.TraitsFromJSON(rec.TraitsRaw)) {
		return zero, consoleRecordingNotFound(recordingID)
	}
	return rec, nil
}

// scopeFor resolves the caller's `soul.console` purview.
//
// The action is `console`, not `list`: the boundary that decides which sessions
// may be watched must be the boundary that decided which hosts may be consoled
// into, or the two would drift and the weaker one would win. No claims / no
// scoper → a zero Purview, which renders FALSE and hides everything.
//
// Purview, never [rbac.Enforcer.Check]: Check takes a context map and looks like
// it honours it, but a role's `default_scope` lives outside the permission it
// matches, so a bare `soul.console` passes Check for ANY host (ADR-047 — Check
// is the route gate, scope is resolved here). Every scope decision on this
// surface therefore goes through ResolvePurview.
func (h *ConsoleRecordingHandler) scopeFor(claims *keeperjwt.Claims) soulpurview.Scope {
	if claims == nil || h.scoper == nil {
		return soulpurview.Scope{}
	}
	return soulpurview.Resolve(h.scoper.ResolvePurview(claims.Subject, "soul", "console"))
}

// writeAudit records that a recording's CONTENT was read. Best-effort, matching
// the Hub's own `console.opened` write.
func (h *ConsoleRecordingHandler) writeAudit(ctx context.Context, claims *keeperjwt.Claims, rec consolepg.Recording) {
	if h.audit == nil || claims == nil {
		return
	}
	if err := h.audit.Write(ctx, &audit.Event{
		EventType:     audit.EventConsoleRecordingRead,
		Source:        audit.SourceAPI,
		ArchonAID:     claims.Subject,
		CorrelationID: rec.RecordingID,
		Payload: map[string]any{
			"recording_id": rec.RecordingID,
			"session_id":   rec.SessionID,
			"sid":          rec.SID,
			"kind":         rec.Kind,
			// The Archon whose session this was — the fact that makes the event
			// worth having. Reading your own shell back is routine; reading
			// someone else's is the thing an investigator asks about later.
			"recorded_archon_aid": rec.ArchonAID,
		},
	}); err != nil {
		h.logger.Warn("console.recording.cast: audit write failed",
			slog.String("recording_id", rec.RecordingID), slog.Any("error", err))
	}
}

// filter validates the query filters into a store filter.
func (in ConsoleRecordingListInput) filter() (consolepg.ListFilter, error) {
	var f consolepg.ListFilter
	if in.SID != "" {
		if !soul.ValidSID(in.SID) {
			return f, &problemError{problem.New(problem.TypeValidationFailed, "",
				"query 'sid' must match "+soul.SIDPattern)}
		}
		f.SID = in.SID
	}
	if in.Kind != "" && !validConsoleRecordingKind(in.Kind) {
		return f, &problemError{problem.New(problem.TypeValidationFailed, "",
			"query 'kind' must be one of interactive/command")}
	}
	f.ArchonAID = in.ArchonAID
	f.Kind = in.Kind
	f.StartedAfter = in.StartedAfter
	f.StartedBefore = in.StartedBefore
	return f, nil
}

// consoleRecordingScopeColumns maps a recording onto the RBAC scope dimensions
// — [soulpurview.Columns] for a different table. The aliases come from the
// reader rather than being repeated here: a predicate naming a column the query
// does not have is a runtime SQL error on a security path, and two copies of
// the same strings is how that happens.
//
// Service and incarnation stay empty — a recording carries neither, so a
// condition on them renders FALSE (fail-closed).
var consoleRecordingScopeColumns = rbac.ScopeColumns{
	Coven:  consolepg.ScopeCovenColumn,
	Host:   consolepg.ScopeHostColumn,
	Traits: consolepg.ScopeTraitsColumn,
}

// validConsoleRecordingKind — the closed enum of `console_recordings.kind`,
// matching the CHECK constraint of migration 104.
func validConsoleRecordingKind(kind string) bool {
	return kind == "interactive" || kind == "command"
}

// consoleRecordingNotFound — the single refusal for "absent" and "out of scope".
func consoleRecordingNotFound(recordingID string) error {
	return &problemError{problem.New(problem.TypeNotFound, "", "console recording "+recordingID+" not found")}
}

// newConsoleRecordingView projects a store row onto the wire shape. Timestamps
// are UTC second-precision, the convention of every other read route here.
func newConsoleRecordingView(rec consolepg.Recording) ConsoleRecordingView {
	v := ConsoleRecordingView{
		RecordingID: rec.RecordingID,
		SessionID:   rec.SessionID,
		Kind:        rec.Kind,
		SID:         rec.SID,
		ArchonAID:   rec.ArchonAID,
		StartedAt:   rec.StartedAt.UTC().Truncate(time.Second),
		CloseReason: ptrIfNotEmpty(rec.CloseReason),
		EventCount:  rec.EventCount,
		ByteCount:   rec.ByteCount,
		Truncated:   rec.Truncated,
		Width:       rec.Header.Width,
		Height:      rec.Header.Height,
	}
	if rec.FinishedAt != nil {
		fin := rec.FinishedAt.UTC().Truncate(time.Second)
		v.FinishedAt = &fin
	}
	return v
}
