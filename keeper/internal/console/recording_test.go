package console

import (
	"context"
	"encoding/json"
	"errors"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	keeperv1 "github.com/souls-guild/soul-stack/proto/gen/go/keeper/v1"
	"github.com/souls-guild/soul-stack/shared/audit"
)

// Guard tests for mandatory session recording (ADR-0074(g), NIM-145).
//
// The property under test is not "recording works" but "recording cannot be
// avoided": no configuration turns it off, no failure path lets a session
// continue unrecorded, and no chunking makes a masked secret readable again.

// --- fakes -------------------------------------------------------------------

// fakeRecordingStore is the in-package twin of consoletest.Store. The console
// package's own tests cannot import consoletest (that package imports this one),
// so the small duplication buys the guard tests direct access to unexported
// internals.
type fakeRecordingStore struct {
	mu         sync.Mutex
	metas      map[string]RecordingMeta
	parts      map[string]map[int64]string
	results    map[string]RecordingResult
	order      []string
	failBegin  bool
	failAppend bool
}

var errStoreDown = errors.New("recording store is down")

func newFakeRecordingStore() *fakeRecordingStore {
	return &fakeRecordingStore{
		metas:   make(map[string]RecordingMeta),
		parts:   make(map[string]map[int64]string),
		results: make(map[string]RecordingResult),
	}
}

func (s *fakeRecordingStore) Begin(_ context.Context, meta RecordingMeta) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.failBegin {
		return errStoreDown
	}
	s.metas[meta.RecordingID] = meta
	s.parts[meta.RecordingID] = make(map[int64]string)
	s.order = append(s.order, meta.RecordingID)
	return nil
}

func (s *fakeRecordingStore) Append(_ context.Context, id string, seq int64, body string) error {
	s.mu.Lock()
	fail := s.failAppend
	if !fail {
		if s.parts[id] == nil {
			s.parts[id] = make(map[int64]string)
		}
		s.parts[id][seq] = body
	}
	s.mu.Unlock()
	if fail {
		return errStoreDown
	}
	return nil
}

func (s *fakeRecordingStore) Finish(_ context.Context, id string, res RecordingResult) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.results[id] = res
	return nil
}

func (s *fakeRecordingStore) setFailBegin(v bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.failBegin = v
}

func (s *fakeRecordingStore) setFailAppend(v bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.failAppend = v
}

func (s *fakeRecordingStore) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.metas)
}

// body concatenates the parts of a recording in order.
func (s *fakeRecordingStore) body(id string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	seqs := make([]int64, 0, len(s.parts[id]))
	for seq := range s.parts[id] {
		seqs = append(seqs, seq)
	}
	sort.Slice(seqs, func(i, j int) bool { return seqs[i] < seqs[j] })
	var b strings.Builder
	for _, seq := range seqs {
		b.WriteString(s.parts[id][seq])
	}
	return b.String()
}

func (s *fakeRecordingStore) result(id string) RecordingResult {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.results[id]
}

func newTestRecorder(t *testing.T, store RecordingStore, cfg RecorderConfig) Recorder {
	t.Helper()
	r, err := NewRecorder(store, cfg, testLogger())
	if err != nil {
		t.Fatalf("NewRecorder: %v", err)
	}
	return r
}

// castEvent is one decoded asciicast v2 event line.
type castEvent struct {
	At   float64
	Code string
	Data string
}

func decodeCast(t *testing.T, body string) []castEvent {
	t.Helper()
	var out []castEvent
	for _, line := range strings.Split(strings.TrimSuffix(body, "\n"), "\n") {
		if line == "" {
			continue
		}
		var raw []any
		if err := json.Unmarshal([]byte(line), &raw); err != nil {
			t.Fatalf("cast line %q is not valid JSON: %v", line, err)
		}
		if len(raw) != 3 {
			t.Fatalf("cast line %q has %d fields, want 3", line, len(raw))
		}
		at, _ := raw[0].(float64)
		code, _ := raw[1].(string)
		data, _ := raw[2].(string)
		out = append(out, castEvent{At: at, Code: code, Data: data})
	}
	return out
}

// --- "it cannot be turned off" ----------------------------------------------

// A Hub without a recorder must not exist. This is where "recording is not a
// setting" is enforced: no nil-means-disabled branch, so no deployment,
// refactor or config mistake can produce an unrecorded console.
func TestNewHub_RefusesWithoutARecorder(t *testing.T) {
	_, err := NewHub(HubDeps{Dispatcher: &recordingDispatcher{}, Logger: testLogger()})
	if err == nil {
		t.Fatal("NewHub accepted a nil Recorder — an unrecorded console is reachable")
	}
	if !strings.Contains(err.Error(), "recorder") {
		t.Fatalf("error does not name the missing recorder: %v", err)
	}
}

// A recorder without a store is the same hole one layer down.
func TestNewRecorder_RefusesWithoutAStore(t *testing.T) {
	if _, err := NewRecorder(nil, RecorderConfig{}, testLogger()); err == nil {
		t.Fatal("NewRecorder accepted a nil store")
	}
}

// Every session opened through the Hub is recorded — there is no request field,
// no per-session opt-out and nothing an operator can pass to avoid it.
func TestHubOpen_AlwaysRecords(t *testing.T) {
	store := newFakeRecordingStore()
	h, _ := newTestHub(t, HubDeps{Recorder: newTestRecorder(t, store, RecorderConfig{})})

	sess := mustOpen(t, h, "pane-1", "host-a", "archon-a", &captureSink{})

	if store.count() != 1 {
		t.Fatalf("recordings started = %d, want 1", store.count())
	}
	if sess.RecordingID() == "" {
		t.Fatal("session carries no recording id")
	}
	meta := store.metas[sess.RecordingID()]
	if meta.Kind != RecordingInteractive || meta.SID != "host-a" || meta.AID != "archon-a" {
		t.Fatalf("recording meta = %+v, want the session's kind/sid/aid", meta)
	}
	if meta.SessionID != sess.KeeperID {
		t.Fatalf("recording session_id = %q, want the Keeper-side id %q", meta.SessionID, sess.KeeperID)
	}
}

// --- fail-closed -------------------------------------------------------------

// A session that cannot be recorded is not opened, and — the part that matters
// — no ConsoleOpen reaches the host. A pty that has already started cannot be
// un-started, so the refusal has to land before the dispatch.
func TestHubOpen_RefusedWhenTheRecordingCannotStart(t *testing.T) {
	store := newFakeRecordingStore()
	store.setFailBegin(true)
	h, disp := newTestHub(t, HubDeps{Recorder: newTestRecorder(t, store, RecorderConfig{})})

	_, err := h.Open(context.Background(), OpenRequest{
		ClientID: "pane-1", SID: "host-a", AID: "archon-a", Sink: &captureSink{},
	})
	if !errors.Is(err, ErrRecordingUnavailable) {
		t.Fatalf("Open error = %v, want ErrRecordingUnavailable", err)
	}
	if n := disp.openCount(); n != 0 {
		t.Fatalf("%d ConsoleOpen dispatched — a shell was started for a session that is not recorded", n)
	}
	if h.Count() != 0 {
		t.Fatalf("hub holds %d sessions after a refused open", h.Count())
	}
	if h.CountFor("archon-a") != 0 {
		t.Fatal("the refused open leaked a per-operator limiter slot")
	}
}

// A recording that breaks AFTER the shell exists closes the session. Otherwise
// "mandatory" would mean "until the store hiccups", and the trail would be the
// one ADR-0074(g) calls inadequate: a line saying a shell was opened, and no
// record of what was done with it.
func TestHub_ClosesTheSessionWhenTheRecordingBreaks(t *testing.T) {
	store := newFakeRecordingStore()
	h, disp := newTestHub(t, HubDeps{Recorder: newTestRecorder(t, store, RecorderConfig{})})
	sink := &captureSink{}
	sess := mustOpen(t, h, "pane-1", "host-a", "archon-a", sink)

	store.setFailAppend(true)
	// Enough output to force a flush, so the store failure is real rather than
	// buffered.
	big := make([]byte, recordingFlushBytes+1)
	for i := range big {
		big[i] = 'x'
	}
	h.Deliver(context.Background(), "host-a", &keeperv1.FromSoul{
		Payload: &keeperv1.FromSoul_ConsoleChunk{ConsoleChunk: &keeperv1.ConsoleChunk{
			SessionId: sess.KeeperID, Data: big,
		}},
	})

	// The operator's error frame is the last step of the teardown, so waiting
	// for it also proves the close ran.
	waitFor(t, func() bool { return sink.lastErrorCode() == ErrCodeRecordingUnavailable },
		"the operator was told why the console closed")

	if !sess.closed.Load() {
		t.Fatal("session still live after its recording failed")
	}
	if disp.closeCount() == 0 {
		t.Fatal("no ConsoleClose dispatched — the pty outlived its recording")
	}
}

// Output is recorded BEFORE it is handed to the operator: a byte the store
// refused must not reach the pane.
func TestHub_DoesNotDeliverOutputItCouldNotRecord(t *testing.T) {
	store := newFakeRecordingStore()
	h, _ := newTestHub(t, HubDeps{Recorder: newTestRecorder(t, store, RecorderConfig{})})
	sink := &captureSink{}
	sess := mustOpen(t, h, "pane-1", "host-a", "archon-a", sink)

	// Break the recording without going through the store, so the failure is
	// latched before the chunk arrives.
	rec := sess.recording.(*castRecording)
	rec.fail(errStoreDown)

	h.Deliver(context.Background(), "host-a", &keeperv1.FromSoul{
		Payload: &keeperv1.FromSoul_ConsoleChunk{ConsoleChunk: &keeperv1.ConsoleChunk{
			SessionId: sess.KeeperID, Data: []byte("root password is hunter2"),
		}},
	})

	if _, n, _, _ := sink.counts(); n != 0 {
		t.Fatalf("%d chunks delivered to an operator while the recording was down", n)
	}
}

// Keystrokes are recorded before they are dispatched, so a keystroke the store
// refused never reaches the shell.
func TestHub_DoesNotDispatchInputItCouldNotRecord(t *testing.T) {
	store := newFakeRecordingStore()
	h, disp := newTestHub(t, HubDeps{Recorder: newTestRecorder(t, store, RecorderConfig{})})
	sess := mustOpen(t, h, "pane-1", "host-a", "archon-a", &captureSink{})
	markSessionReady(t, h, sess)

	sess.recording.(*castRecording).fail(errStoreDown)

	if err := h.Stdin(context.Background(), sess, []byte("rm -rf /\n")); err == nil {
		t.Fatal("Stdin succeeded while the recording was down")
	}
	if n := disp.stdinCount(); n != 0 {
		t.Fatalf("%d keystrokes reached the host unrecorded", n)
	}
}

// The per-session cap closes the session rather than silently continuing with a
// truncated record.
func TestRecording_SizeCapClosesTheSession(t *testing.T) {
	store := newFakeRecordingStore()
	h, disp := newTestHub(t, HubDeps{Recorder: newTestRecorder(t, store, RecorderConfig{MaxBytes: 64})})
	sink := &captureSink{}
	sess := mustOpen(t, h, "pane-1", "host-a", "archon-a", sink)

	h.Deliver(context.Background(), "host-a", &keeperv1.FromSoul{
		Payload: &keeperv1.FromSoul_ConsoleChunk{ConsoleChunk: &keeperv1.ConsoleChunk{
			SessionId: sess.KeeperID, Data: []byte(strings.Repeat("y\n", 128)),
		}},
	})

	waitFor(t, func() bool { return sess.closed.Load() }, "session closed on the recording cap")
	if disp.closeCount() == 0 {
		t.Fatal("no ConsoleClose dispatched when the recording hit its cap")
	}
	if res := store.result(sess.RecordingID()); !res.Truncated {
		t.Fatal("the recording is not marked truncated")
	}
}

// --- what lands in the cast --------------------------------------------------

// Both directions and the geometry are in one replayable cast, in order.
func TestRecording_CapturesInputOutputAndResize(t *testing.T) {
	store := newFakeRecordingStore()
	h, _ := newTestHub(t, HubDeps{Recorder: newTestRecorder(t, store, RecorderConfig{})})
	sess := mustOpen(t, h, "pane-1", "host-a", "archon-a", &captureSink{})
	markSessionReady(t, h, sess)

	if err := h.Stdin(context.Background(), sess, []byte("whoami\n")); err != nil {
		t.Fatalf("Stdin: %v", err)
	}
	h.Deliver(context.Background(), "host-a", &keeperv1.FromSoul{
		Payload: &keeperv1.FromSoul_ConsoleChunk{ConsoleChunk: &keeperv1.ConsoleChunk{
			SessionId: sess.KeeperID, Data: []byte("root\n"),
		}},
	})
	if err := h.Resize(context.Background(), sess, 120, 40); err != nil {
		t.Fatalf("Resize: %v", err)
	}
	h.Close(context.Background(), sess, "done")

	events := decodeCast(t, store.body(sess.RecordingID()))
	want := []castEvent{
		{Code: castCodeInput, Data: "whoami\n"},
		{Code: castCodeOutput, Data: "root\n"},
		{Code: castCodeResize, Data: "120x40"},
	}
	if len(events) != len(want) {
		t.Fatalf("cast has %d events, want %d: %+v", len(events), len(want), events)
	}
	for i, w := range want {
		if events[i].Code != w.Code || events[i].Data != w.Data {
			t.Fatalf("event %d = %+v, want code %q data %q", i, events[i], w.Code, w.Data)
		}
		if i > 0 && events[i].At < events[i-1].At {
			t.Fatalf("event %d goes back in time (%v after %v) — the cast will not replay",
				i, events[i].At, events[i-1].At)
		}
	}

	meta := store.metas[sess.RecordingID()]
	if meta.Header.Version != castVersion {
		t.Fatalf("cast header version = %d, want %d", meta.Header.Version, castVersion)
	}
	if res := store.result(sess.RecordingID()); res.CloseReason != "done" || res.EventCount != 3 {
		t.Fatalf("recording result = %+v, want reason=done events=3", res)
	}
}

// A gap the Soul reported dropping is recorded where it happened, so a replay
// shows the loss instead of splicing two screens into one that never existed.
func TestRecording_DroppedBytesBecomeAMarker(t *testing.T) {
	store := newFakeRecordingStore()
	h, _ := newTestHub(t, HubDeps{Recorder: newTestRecorder(t, store, RecorderConfig{})})
	sess := mustOpen(t, h, "pane-1", "host-a", "archon-a", &captureSink{})

	h.Deliver(context.Background(), "host-a", &keeperv1.FromSoul{
		Payload: &keeperv1.FromSoul_ConsoleChunk{ConsoleChunk: &keeperv1.ConsoleChunk{
			SessionId: sess.KeeperID, Data: []byte("after"), DroppedBytes: 4096,
		}},
	})
	h.Close(context.Background(), sess, "done")

	events := decodeCast(t, store.body(sess.RecordingID()))
	if len(events) != 2 || events[0].Code != castCodeMarker {
		t.Fatalf("want a marker before the chunk, got %+v", events)
	}
	if !strings.Contains(events[0].Data, "4096") {
		t.Fatalf("marker %q does not carry the dropped byte count", events[0].Data)
	}
}

// --- masking -----------------------------------------------------------------

// A vault reference in one chunk is masked, and only the reference is: blanking
// the whole chunk would destroy the replay the recording exists to provide.
func TestRecording_MasksAVaultRefWithoutDestroyingTheScreen(t *testing.T) {
	store := newFakeRecordingStore()
	h, _ := newTestHub(t, HubDeps{Recorder: newTestRecorder(t, store, RecorderConfig{})})
	sess := mustOpen(t, h, "pane-1", "host-a", "archon-a", &captureSink{})

	h.Deliver(context.Background(), "host-a", &keeperv1.FromSoul{
		Payload: &keeperv1.FromSoul_ConsoleChunk{ConsoleChunk: &keeperv1.ConsoleChunk{
			SessionId: sess.KeeperID,
			Data:      []byte("reading vault:secret/db/password now\n"),
		}},
	})
	h.Close(context.Background(), sess, "done")

	body := store.body(sess.RecordingID())
	if strings.Contains(body, "vault:secret/db") {
		t.Fatalf("the vault reference is in the recording in plaintext: %q", body)
	}
	if !strings.Contains(body, audit.MaskedValue) {
		t.Fatalf("nothing was masked: %q", body)
	}
	if !strings.Contains(body, "reading ") || !strings.Contains(body, " now") {
		t.Fatalf("masking swallowed the surrounding screen: %q", body)
	}
}

// The case a naive per-chunk masker gets wrong, and the reason the recorder
// carries a tail across chunks: a pty echoes keystrokes ONE BYTE AT A TIME, so
// an operator typing a vault path produces chunks none of which match the
// pattern. Masking each in isolation would mask nothing at all — precisely
// where an operator is most likely to type a credential path.
func TestRecording_MasksAVaultRefTypedOneByteAtATime(t *testing.T) {
	store := newFakeRecordingStore()
	h, _ := newTestHub(t, HubDeps{Recorder: newTestRecorder(t, store, RecorderConfig{})})
	sess := mustOpen(t, h, "pane-1", "host-a", "archon-a", &captureSink{})
	markSessionReady(t, h, sess)

	const typed = "cat vault:secret/db/password\n"
	for i := 0; i < len(typed); i++ {
		if err := h.Stdin(context.Background(), sess, []byte{typed[i]}); err != nil {
			t.Fatalf("Stdin(%q): %v", typed[i], err)
		}
	}
	h.Close(context.Background(), sess, "done")

	body := store.body(sess.RecordingID())
	if strings.Contains(body, "vault:secret/db") {
		t.Fatalf("a reference typed one byte at a time survived unmasked: %q", body)
	}

	// Replaying the cast is what a reader does, so assert on the replayed
	// stream rather than on the raw body: the keystrokes are separate events
	// and only their concatenation is the line that was typed.
	var replayed strings.Builder
	for _, ev := range decodeCast(t, body) {
		if ev.Code == castCodeInput {
			replayed.WriteString(ev.Data)
		}
	}
	if want := "cat " + audit.MaskedValue + "\n"; replayed.String() != want {
		t.Fatalf("replayed input = %q, want %q", replayed.String(), want)
	}
}

// The carry must not swallow the tail of a session that ends mid-reference.
func TestRecording_FlushesAPartialReferenceOnClose(t *testing.T) {
	store := newFakeRecordingStore()
	h, _ := newTestHub(t, HubDeps{Recorder: newTestRecorder(t, store, RecorderConfig{})})
	sess := mustOpen(t, h, "pane-1", "host-a", "archon-a", &captureSink{})
	markSessionReady(t, h, sess)

	if err := h.Stdin(context.Background(), sess, []byte("vault:sec")); err != nil {
		t.Fatalf("Stdin: %v", err)
	}
	h.Close(context.Background(), sess, "done")

	if body := store.body(sess.RecordingID()); !strings.Contains(body, "vault:sec") {
		t.Fatalf("the held-back tail never reached the recording: %q", body)
	}
}

// --- audit -------------------------------------------------------------------

// The audit log is the index into the recordings: `console.opened` carries the
// id of the artifact it refers to (ADR-0074(f)).
func TestHub_AuditCarriesTheRecordingID(t *testing.T) {
	store := newFakeRecordingStore()
	aw := &captureAudit{}
	h, _ := newTestHub(t, HubDeps{
		Recorder:    newTestRecorder(t, store, RecorderConfig{}),
		AuditWriter: aw,
	})
	sess := mustOpen(t, h, "pane-1", "host-a", "archon-a", &captureSink{})
	h.Close(context.Background(), sess, "done")

	for _, evt := range aw.events() {
		got, _ := evt.Payload["recording_id"].(string)
		if got != sess.RecordingID() {
			t.Fatalf("%s payload recording_id = %q, want %q", evt.EventType, got, sess.RecordingID())
		}
	}
	if len(aw.events()) != 2 {
		t.Fatalf("audit events = %d, want console.opened + console.closed", len(aw.events()))
	}
}

// --- helpers -----------------------------------------------------------------

// markSessionReady releases the pre-`opened` park (NIM-188) so input is
// dispatched rather than queued.
func markSessionReady(t *testing.T, h *Hub, sess *Session) {
	t.Helper()
	h.Deliver(context.Background(), sess.SID, &keeperv1.FromSoul{
		Payload: &keeperv1.FromSoul_ConsoleOpened{ConsoleOpened: &keeperv1.ConsoleOpened{
			SessionId: sess.KeeperID, Pid: 4242,
		}},
	})
}

func waitFor(t *testing.T, cond func() bool, what string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for: %s", what)
}
