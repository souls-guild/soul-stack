package handlers

// Guard tests for console recording playback (ADR-0074 amendment, NIM-148).
//
// What they pin, in the order the ticket's acceptance names it:
//   - no right / an empty purview → nothing is served, and the SQL says FALSE
//     rather than the list quietly widening;
//   - a right scoped to ANOTHER host → refused, and refused as 404 so the route
//     cannot be used to enumerate who consoled into what;
//   - what the recorder masked stays masked when read back — end-to-end through
//     the real recorder, because a pass-through that only LOOKS like a
//     pass-through is exactly what a hand-written fixture would prove;
//   - reading a cast is audited, and reading a refused one is not.

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/souls-guild/soul-stack/keeper/internal/console"
	"github.com/souls-guild/soul-stack/keeper/internal/consolepg"
	"github.com/souls-guild/soul-stack/keeper/internal/jwt"
	"github.com/souls-guild/soul-stack/keeper/internal/rbac"
	"github.com/souls-guild/soul-stack/shared/audit"
)

const (
	testRecordingID = "01J0000000000000000000CAST"
	testRecordedSID = "db-01.example.com"
	testOtherSID    = "web-01.example.com"
)

// fakeRecordingReader is both halves the tests need: it captures the filter the
// handler builds (so the scope pushdown can be asserted on the rendered SQL) and
// serves a fixed recording back.
type fakeRecordingReader struct {
	mu sync.Mutex

	rec      consolepg.Recording
	notFound bool
	body     string

	lastScopeSQL  string
	lastScopeArgs []any
	casts         int
}

func (f *fakeRecordingReader) List(_ context.Context, filter consolepg.ListFilter, _, _ int) ([]consolepg.Recording, int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if filter.Scope != nil {
		f.lastScopeSQL, f.lastScopeArgs, _ = filter.Scope(1)
	}
	return []consolepg.Recording{f.rec}, 1, nil
}

func (f *fakeRecordingReader) Get(_ context.Context, id string) (consolepg.Recording, error) {
	if f.notFound || id != f.rec.RecordingID {
		return consolepg.Recording{}, consolepg.ErrNotFound
	}
	return f.rec, nil
}

func (f *fakeRecordingReader) WriteCast(_ context.Context, _ consolepg.Recording, write func([]byte) error) error {
	f.mu.Lock()
	f.casts++
	f.mu.Unlock()
	return write([]byte(f.body))
}

func (f *fakeRecordingReader) castCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.casts
}

func newFakeRecordingReader() *fakeRecordingReader {
	return &fakeRecordingReader{
		rec: consolepg.Recording{
			RecordingID: testRecordingID,
			SessionID:   "01J0000000000000000000SESS",
			Kind:        "interactive",
			SID:         testRecordedSID,
			ArchonAID:   "archon-victim",
			StartedAt:   time.Unix(1_700_000_000, 0).UTC(),
			Covens:      []string{"prod"},
		},
		body: "{\"version\":2}\n[0.1,\"o\",\"hi\"]\n",
	}
}

func testClaims(aid string) *jwt.Claims { return &jwt.Claims{Subject: aid} }

// collectCast runs the two-step cast path and returns what was streamed.
func collectCast(t *testing.T, h *ConsoleRecordingHandler, claims *jwt.Claims, id string) (string, error) {
	t.Helper()
	rec, err := h.PrepareCast(context.Background(), claims, id)
	if err != nil {
		return "", err
	}
	var sb strings.Builder
	h.StreamCast(context.Background(), rec, func(b []byte) error {
		sb.Write(b)
		return nil
	})
	return sb.String(), nil
}

// TestConsoleRecordingList_NoRightServesNothing — an operator with no
// `soul.console` at all gets a purview that renders FALSE. The assertion is on
// the SQL rather than on the (faked) rows: an empty result set proves nothing
// about a store that was asked for everything.
func TestConsoleRecordingList_NoRightServesNothing(t *testing.T) {
	reader := newFakeRecordingReader()
	h := NewConsoleRecordingHandler(reader, fakeScoper{empty: true}, nil, nil)

	if _, err := h.ListTyped(context.Background(), testClaims("archon-nobody"),
		ConsoleRecordingListInput{Limit: 50}); err != nil {
		t.Fatalf("ListTyped: %v", err)
	}
	if reader.lastScopeSQL != "FALSE" {
		t.Fatalf("scope SQL = %q, want FALSE (fail-closed: no right must hide every recording)", reader.lastScopeSQL)
	}
}

// TestConsoleRecordingList_NilScoperIsFailClosed — an unconfigured scoper must
// hide everything, not show everything. The souls-list rule; the opposite
// default is a wiring mistake that reads as working.
func TestConsoleRecordingList_NilScoperIsFailClosed(t *testing.T) {
	reader := newFakeRecordingReader()
	h := NewConsoleRecordingHandler(reader, nil, nil, nil)

	if _, err := h.ListTyped(context.Background(), testClaims("archon-alice"),
		ConsoleRecordingListInput{Limit: 50}); err != nil {
		t.Fatalf("ListTyped: %v", err)
	}
	if reader.lastScopeSQL != "FALSE" {
		t.Fatalf("scope SQL = %q, want FALSE with a nil scoper", reader.lastScopeSQL)
	}
}

// TestConsoleRecordingList_ScopeIsPushedIntoSQL — a `host=`-scoped operator gets
// a predicate over the RECORDING's own sid, so pagination and the total stay
// exact and a recording of a since-deleted host stays reachable by the operator
// scoped to it.
func TestConsoleRecordingList_ScopeIsPushedIntoSQL(t *testing.T) {
	reader := newFakeRecordingReader()
	h := NewConsoleRecordingHandler(reader, fakeScoper{exprs: []string{"host=" + testRecordedSID}}, nil, nil)

	if _, err := h.ListTyped(context.Background(), testClaims("archon-alice"),
		ConsoleRecordingListInput{Limit: 50}); err != nil {
		t.Fatalf("ListTyped: %v", err)
	}
	if !strings.Contains(reader.lastScopeSQL, "r.sid") {
		t.Fatalf("scope SQL = %q, want a predicate over the recording's own sid", reader.lastScopeSQL)
	}
	if reader.lastScopeSQL == "TRUE" {
		t.Fatal("a host-scoped operator must not get an unrestricted listing")
	}
}

// TestConsoleRecordingList_UnrestrictedSeesEverything — the other end of the
// same switch, so a fail-closed bug cannot hide behind "it denies everything".
func TestConsoleRecordingList_UnrestrictedSeesEverything(t *testing.T) {
	reader := newFakeRecordingReader()
	h := NewConsoleRecordingHandler(reader, fakeScoper{unrestricted: true}, nil, nil)

	if _, err := h.ListTyped(context.Background(), testClaims("archon-admin"),
		ConsoleRecordingListInput{Limit: 50}); err != nil {
		t.Fatalf("ListTyped: %v", err)
	}
	if reader.lastScopeSQL != "TRUE" {
		t.Fatalf("scope SQL = %q, want TRUE for an unrestricted operator", reader.lastScopeSQL)
	}
}

// TestConsoleRecordingScope_ResolvesTheConsoleRight — the boundary that decides
// which sessions may be WATCHED must be the one that decided which hosts may be
// consoled into. Resolving `soul.list` here would let anyone who can see a host
// read every shell ever held on it.
func TestConsoleRecordingScope_ResolvesTheConsoleRight(t *testing.T) {
	scoper := &recordingConsoleScoper{}
	h := NewConsoleRecordingHandler(newFakeRecordingReader(), scoper, nil, nil)

	if _, err := h.ListTyped(context.Background(), testClaims("archon-alice"),
		ConsoleRecordingListInput{Limit: 50}); err != nil {
		t.Fatalf("ListTyped: %v", err)
	}
	if scoper.resource != "soul" || scoper.action != "console" {
		t.Fatalf("purview resolved for %q/%q, want soul/console", scoper.resource, scoper.action)
	}
}

// TestConsoleRecordingGet_ScopedToAnotherHostIs404 — the core refusal. An
// operator holding `soul.console on host=web-01` must not read a session held on
// db-01, and the refusal must be indistinguishable from an unknown id.
func TestConsoleRecordingGet_ScopedToAnotherHostIs404(t *testing.T) {
	reader := newFakeRecordingReader()
	h := NewConsoleRecordingHandler(reader, fakeScoper{exprs: []string{"host=" + testOtherSID}}, nil, nil)

	_, err := h.GetTyped(context.Background(), testClaims("archon-alice"), testRecordingID)
	assertConsoleRecordingNotFound(t, err)
}

// TestConsoleRecordingGet_OutOfScopeMatchesUnknownID — the two refusals are the
// same bytes. A 403 here would confirm the recording exists, turning the route
// into an oracle for which hosts have been consoled into and by whom.
func TestConsoleRecordingGet_OutOfScopeMatchesUnknownID(t *testing.T) {
	scoped := NewConsoleRecordingHandler(newFakeRecordingReader(),
		fakeScoper{exprs: []string{"host=" + testOtherSID}}, nil, nil)
	_, outOfScope := scoped.GetTyped(context.Background(), testClaims("archon-alice"), testRecordingID)

	absent := newFakeRecordingReader()
	absent.notFound = true
	unrestricted := NewConsoleRecordingHandler(absent, fakeScoper{unrestricted: true}, nil, nil)
	_, unknown := unrestricted.GetTyped(context.Background(), testClaims("archon-admin"), testRecordingID)

	if outOfScope == nil || unknown == nil {
		t.Fatal("both reads must be refused")
	}
	if outOfScope.Error() != unknown.Error() {
		t.Fatalf("out-of-scope refusal %q differs from unknown-id refusal %q — the route leaks existence",
			outOfScope, unknown)
	}
}

// TestConsoleRecordingCast_ScopedToAnotherHostServesNothing — the refusal holds
// on the route that actually discloses content: no bytes, and no audit event
// either (nothing was disclosed to record).
func TestConsoleRecordingCast_ScopedToAnotherHostServesNothing(t *testing.T) {
	reader := newFakeRecordingReader()
	aw := &recordingAuditWriter{}
	h := NewConsoleRecordingHandler(reader, fakeScoper{exprs: []string{"host=" + testOtherSID}}, aw, nil)

	body, err := collectCast(t, h, testClaims("archon-alice"), testRecordingID)
	assertConsoleRecordingNotFound(t, err)
	if body != "" {
		t.Fatalf("a refused cast streamed %d bytes", len(body))
	}
	if reader.castCount() != 0 {
		t.Fatal("a refused cast reached the store")
	}
	if len(aw.events) != 0 {
		t.Fatalf("a refused cast wrote %d audit events; nothing was disclosed", len(aw.events))
	}
}

// TestConsoleRecordingCast_IsAudited — reading a session's content lands in the
// audit log, naming the recording and the Archon whose session it was.
func TestConsoleRecordingCast_IsAudited(t *testing.T) {
	aw := &recordingAuditWriter{}
	h := NewConsoleRecordingHandler(newFakeRecordingReader(), fakeScoper{unrestricted: true}, aw, nil)

	if _, err := collectCast(t, h, testClaims("archon-auditor"), testRecordingID); err != nil {
		t.Fatalf("cast: %v", err)
	}
	if len(aw.events) != 1 {
		t.Fatalf("audit events = %d, want exactly one console.recording-read", len(aw.events))
	}
	ev := aw.events[0]
	if ev.EventType != audit.EventConsoleRecordingRead {
		t.Fatalf("event type = %q, want %q", ev.EventType, audit.EventConsoleRecordingRead)
	}
	if ev.ArchonAID != "archon-auditor" {
		t.Fatalf("archon_aid = %q, want the READER", ev.ArchonAID)
	}
	if ev.CorrelationID != testRecordingID {
		t.Fatalf("correlation_id = %q, want the recording id", ev.CorrelationID)
	}
	if got := ev.Payload["recorded_archon_aid"]; got != "archon-victim" {
		t.Fatalf("recorded_archon_aid = %v, want the Archon whose session was read", got)
	}
	if got := ev.Payload["sid"]; got != testRecordedSID {
		t.Fatalf("sid = %v, want %q", got, testRecordedSID)
	}
}

// TestConsoleRecordingList_IsNotAudited — the asymmetry is deliberate: a listing
// says a session happened, which `console.opened` already said. Auditing it too
// would bury the event that matters under noise from every UI page load.
func TestConsoleRecordingList_IsNotAudited(t *testing.T) {
	aw := &recordingAuditWriter{}
	h := NewConsoleRecordingHandler(newFakeRecordingReader(), fakeScoper{unrestricted: true}, aw, nil)

	if _, err := h.ListTyped(context.Background(), testClaims("archon-admin"),
		ConsoleRecordingListInput{Limit: 50}); err != nil {
		t.Fatalf("ListTyped: %v", err)
	}
	if _, err := h.GetTyped(context.Background(), testClaims("archon-admin"), testRecordingID); err != nil {
		t.Fatalf("GetTyped: %v", err)
	}
	if len(aw.events) != 0 {
		t.Fatalf("metadata reads wrote %d audit events, want 0", len(aw.events))
	}
}

// TestConsoleRecordingCast_MaskedStaysMasked — the end-to-end guarantee, driven
// through the REAL recorder rather than a hand-written fixture: a fixture would
// only prove the test author can type a mask.
//
// The vault reference is fed ONE BYTE AT A TIME, the way a pty echoes an
// operator typing it (NIM-145). The recorder carries the tail across chunk
// boundaries and masks the reference; playback must hand that back untouched —
// there is no un-masking path here, and no second masking pass either.
func TestConsoleRecordingCast_MaskedStaysMasked(t *testing.T) {
	const secretRef = "vault:secret/db#password"

	store := newReplayStore()
	recorder, err := console.NewRecorder(store, console.RecorderConfig{}, slog.New(slog.NewJSONHandler(io.Discard, nil)))
	if err != nil {
		t.Fatalf("NewRecorder: %v", err)
	}
	rec, err := recorder.Open(context.Background(), console.RecordingSpec{
		SessionID: "01J0000000000000000000SESS",
		Kind:      console.RecordingInteractive,
		SID:       testRecordedSID,
		AID:       "archon-victim",
	})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	for i := 0; i < len(secretRef); i++ {
		if err := rec.Input([]byte{secretRef[i]}); err != nil {
			t.Fatalf("Input %d: %v", i, err)
		}
	}
	rec.Close(context.Background(), "test")

	store.publish(rec.ID(), testRecordedSID, "archon-victim")
	h := NewConsoleRecordingHandler(store, fakeScoper{unrestricted: true}, nil, nil)

	body, err := collectCast(t, h, testClaims("archon-auditor"), rec.ID())
	if err != nil {
		t.Fatalf("cast: %v", err)
	}
	if strings.Contains(body, secretRef) {
		t.Fatalf("playback served the raw vault reference:\n%s", body)
	}
	if !strings.Contains(body, audit.MaskedValue) {
		t.Fatalf("playback lost the mask marker %q:\n%s", audit.MaskedValue, body)
	}
	// The screen around the reference must survive — masking the whole chunk
	// would destroy the record the masking exists to make safe to keep.
	if !strings.Contains(body, `"i"`) {
		t.Fatalf("playback lost the input events entirely:\n%s", body)
	}
}

// TestConsoleRecordingCast_ServesTheStoredBodyVerbatim — the read path does not
// re-encode, re-mask or re-order. The header line comes first, then the parts in
// seq order, concatenated exactly as stored.
func TestConsoleRecordingCast_ServesTheStoredBodyVerbatim(t *testing.T) {
	store := newReplayStore()
	if err := store.Begin(context.Background(), console.RecordingMeta{
		RecordingID: testRecordingID,
		SessionID:   "s1",
		Kind:        console.RecordingInteractive,
		SID:         testRecordedSID,
		AID:         "archon-victim",
		Header:      console.CastHeader{Version: 2, Width: 120, Height: 40},
	}); err != nil {
		t.Fatalf("Begin: %v", err)
	}
	// Out of order on purpose: playback is `ORDER BY seq`, not arrival order.
	_ = store.Append(context.Background(), testRecordingID, 1, "[0.2,\"o\",\"second\"]\n")
	_ = store.Append(context.Background(), testRecordingID, 0, "[0.1,\"o\",\"first\"]\n")
	store.publish(testRecordingID, testRecordedSID, "archon-victim")

	h := NewConsoleRecordingHandler(store, fakeScoper{unrestricted: true}, nil, nil)
	body, err := collectCast(t, h, testClaims("archon-admin"), testRecordingID)
	if err != nil {
		t.Fatalf("cast: %v", err)
	}

	want := "{\"version\":2,\"width\":120,\"height\":40,\"timestamp\":0}\n" +
		"[0.1,\"o\",\"first\"]\n[0.2,\"o\",\"second\"]\n"
	if body != want {
		t.Fatalf("cast body =\n%q\nwant\n%q", body, want)
	}
	// The header must parse as one JSON object on its own line — that is what
	// makes the artifact an asciicast rather than our own format.
	var header map[string]any
	if err := json.Unmarshal([]byte(strings.SplitN(body, "\n", 2)[0]), &header); err != nil {
		t.Fatalf("header line is not JSON: %v", err)
	}
	if header["version"] != float64(2) {
		t.Fatalf("header version = %v, want 2", header["version"])
	}
}

// --- helpers -----------------------------------------------------------

// recordingConsoleScoper captures which (resource, action) the handler resolved
// a purview for.
type recordingConsoleScoper struct {
	resource string
	action   string
}

func (s *recordingConsoleScoper) ResolvePurview(_, resource, action string) rbac.Purview {
	s.resource, s.action = resource, action
	return rbac.Purview{}
}

func assertConsoleRecordingNotFound(t *testing.T, err error) {
	t.Helper()
	if err == nil {
		t.Fatal("want a refusal, got none")
	}
	d, ok := AsProblemDetails(err)
	if !ok {
		t.Fatalf("error %v is not a problem", err)
	}
	if d.Status != http.StatusNotFound {
		t.Fatalf("problem status = %d (%s), want 404 — an out-of-scope read must not be distinguishable from an absent one", d.Status, d.Type)
	}
}

// replayStore implements BOTH halves — the recorder's write interface and the
// handler's read interface — over memory, so a test can record through the real
// recorder and read the result back through the real handler.
type replayStore struct {
	mu     sync.Mutex
	header map[string]console.CastHeader
	parts  map[string]map[int64]string
	meta   map[string]consolepg.Recording
}

func newReplayStore() *replayStore {
	return &replayStore{
		header: map[string]console.CastHeader{},
		parts:  map[string]map[int64]string{},
		meta:   map[string]consolepg.Recording{},
	}
}

func (s *replayStore) Begin(_ context.Context, meta console.RecordingMeta) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.header[meta.RecordingID] = meta.Header
	s.parts[meta.RecordingID] = map[int64]string{}
	return nil
}

func (s *replayStore) Append(_ context.Context, id string, seq int64, body string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.parts[id] == nil {
		s.parts[id] = map[int64]string{}
	}
	s.parts[id][seq] = body
	return nil
}

func (s *replayStore) Finish(_ context.Context, _ string, _ console.RecordingResult) error {
	return nil
}

// publish makes a recorded session visible to the read half.
func (s *replayStore) publish(id, sid, aid string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.meta[id] = consolepg.Recording{
		RecordingID: id,
		SessionID:   "s1",
		Kind:        "interactive",
		SID:         sid,
		ArchonAID:   aid,
		Header:      s.header[id],
		StartedAt:   time.Unix(0, 0).UTC(),
	}
}

func (s *replayStore) List(_ context.Context, _ consolepg.ListFilter, _, _ int) ([]consolepg.Recording, int, error) {
	return nil, 0, errors.New("replayStore: List is not used by these tests")
}

func (s *replayStore) Get(_ context.Context, id string) (consolepg.Recording, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	rec, ok := s.meta[id]
	if !ok {
		return consolepg.Recording{}, consolepg.ErrNotFound
	}
	return rec, nil
}

// WriteCast mirrors the real reader: the header line, then parts in seq order.
func (s *replayStore) WriteCast(_ context.Context, rec consolepg.Recording, write func([]byte) error) error {
	header, err := json.Marshal(rec.Header)
	if err != nil {
		return err
	}
	if err := write(append(header, '\n')); err != nil {
		return err
	}
	s.mu.Lock()
	parts := s.parts[rec.RecordingID]
	seqs := make([]int64, 0, len(parts))
	for seq := range parts {
		seqs = append(seqs, seq)
	}
	bodies := make([]string, 0, len(parts))
	for i := int64(0); i < int64(len(seqs)); i++ {
		bodies = append(bodies, parts[i])
	}
	s.mu.Unlock()
	for _, b := range bodies {
		if err := write([]byte(b)); err != nil {
			return err
		}
	}
	return nil
}
