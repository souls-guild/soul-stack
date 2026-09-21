package console

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"sync"
	"time"

	keeperv1 "github.com/souls-guild/soul-stack/proto/gen/go/keeper/v1"
	"github.com/souls-guild/soul-stack/shared/audit"
)

// Mandatory session recording of the console plane — ADR-0074(g), NIM-145.
//
// A console is the only operator action whose effect cannot be reconstructed
// from its request. `console.opened` records that a shell was opened and by
// whom; nothing records what was typed into it. Recording is what makes the
// right worth granting, so it is not a setting: there is no way to open an
// unrecorded console, and a session that cannot be recorded is not opened at
// all (`ErrRecordingUnavailable`).
//
// The interception point is the Hub and nothing below it. After NIM-188 a
// session may ride its own `ConsoleStream` RPC or the shared `EventStream`, and
// both carriers converge here — on the instance that holds the operator's
// socket, which is also the only instance that sees both directions of one
// session. Recording at the carrier would mean two implementations that could
// drift; recording at the Hub means one, and the one-shot
// `keeper.soul.run-command` (NIM-147) uses the same [Recorder] rather than a
// second recording path of its own.
//
// What lands here is what KEEPER received, which is a superset of what the
// operator saw: a chunk dropped by socket backpressure is recorded before it is
// dropped. The reverse is not true — output the Soul itself discarded never
// arrives, so the count it reports is recorded as a marker in the cast.

// Recording format — asciicast v2 (https://docs.asciinema.org/manual/asciicast/v2/).
//
// Chosen over a bespoke timestamped log because replay is the whole point of
// the artifact and this one already replays: `asciinema play`, `agg` for a GIF,
// and the xterm.js the operator UI is built on. Event codes used here are `o`
// (output), `i` (operator input), `r` (resize) and `m` (marker, for a gap the
// Soul reported dropping).
const (
	// castVersion is the asciicast format version in the header.
	castVersion = 2

	castCodeOutput = "o"
	castCodeInput  = "i"
	castCodeResize = "r"
	castCodeMarker = "m"

	// defaultCastCols / defaultCastRows fill the header when the session has no
	// geometry of its own — a one-shot command, or a pane opened before the
	// browser measured it. A resize event corrects it in the body if one comes.
	defaultCastCols = 80
	defaultCastRows = 24
)

// RecordingKind separates the two things that hold `soul.console`.
type RecordingKind string

const (
	// RecordingInteractive is a PTY session opened over `GET /v1/console`.
	RecordingInteractive RecordingKind = "interactive"
	// RecordingCommand is a one-shot `keeper.soul.run-command` (NIM-147). It
	// rides the Errand transport, not this session manager, but it is the same
	// right reaching the same shell — so it leaves the same artifact.
	RecordingCommand RecordingKind = "command"
)

// Flow-control of the recorder. Code constants, not operator policy: they
// describe how this writer batches, and exposing them would invite tuning that
// breaks the size cap accounting.
const (
	// recordingFlushBytes is how much cast body accumulates before a part row
	// is written. One row per 32 KiB keeps a busy session to a few INSERTs a
	// second while a quiet one still writes whole lines.
	recordingFlushBytes = 32 << 10

	// recordingFlushInterval bounds how long a written byte may sit in memory.
	// It is the loss window if this Keeper instance dies mid-session — the
	// recording is then short by up to this much, and its `finished_at` stays
	// NULL to say so.
	recordingFlushInterval = 2 * time.Second

	// recordingQueueDepth is how many flushed parts may await Postgres. Full
	// means the store is not keeping up at all, which fails the session rather
	// than dropping its record — see [ErrRecordingUnavailable].
	recordingQueueDepth = 64

	// recordingWriteTimeout bounds one part INSERT.
	recordingWriteTimeout = 10 * time.Second

	// DefaultMaxRecordingBytes caps one session's recording. Reaching it closes
	// the session: a console that can no longer be recorded may not keep
	// running, or "mandatory" would mean "until it gets expensive".
	//
	// Sized so that hitting it means a runaway rather than a long shift. At the
	// ~4 KiB/s a full-screen `top` redraws at, 256 MiB is around eighteen hours;
	// against the Soul's default `rate_limit_kbps` of 1 MB/s — output nobody is
	// reading — it is four minutes.
	DefaultMaxRecordingBytes = 256 << 20
)

// Errors of the recording path.
var (
	// ErrRecordingUnavailable — the recording could not be started or could not
	// be kept. Fail-closed: the console is refused, or an open one is closed.
	ErrRecordingUnavailable = errors.New("console: session recording is unavailable")

	// ErrRecordingCapped — the session reached [RecorderConfig.MaxBytes].
	ErrRecordingCapped = errors.New("console: session recording size cap reached")
)

// CastHeader is the asciicast v2 header line.
//
// Operator, host and session live in the `console_recordings` row instead: a
// reader querying "every console on this host last week" must not parse bodies,
// and a player must not choke on keys it does not know.
type CastHeader struct {
	Version   int    `json:"version"`
	Width     uint32 `json:"width"`
	Height    uint32 `json:"height"`
	Timestamp int64  `json:"timestamp"`
	Title     string `json:"title,omitempty"`
}

// RecordingSpec is one request to record a session.
type RecordingSpec struct {
	// SessionID is the Keeper-minted console session id. A [RecordingCommand]
	// has no session, so it mints one of its own — the errand that carried it is
	// reachable through the `console.command` audit event, which holds both ids.
	// One recording per session, enforced by a unique index.
	SessionID string
	Kind      RecordingKind
	SID       string
	AID       string
	Cols      uint32
	Rows      uint32

	// OnFailure is called once, from the writer goroutine, when the recording
	// can no longer be kept. It is how fail-closed reaches a session that has
	// gone quiet: without it a Postgres outage would only surface on the next
	// keystroke, and a shell left at a prompt would keep running unrecorded.
	// Optional — [RecordingCommand] has nothing left to close.
	OnFailure func(error)
}

// RecordingMeta is what the store persists when a recording opens.
type RecordingMeta struct {
	RecordingID string
	SessionID   string
	Kind        RecordingKind
	SID         string
	AID         string
	Header      CastHeader
	StartedAt   time.Time
}

// RecordingResult is the terminal state of a recording.
type RecordingResult struct {
	FinishedAt  time.Time
	CloseReason string
	EventCount  int64
	ByteCount   int64
	Truncated   bool
}

// RecordingStore is the persistence half. Implemented by
// keeper/internal/consolepg over pgxpool; Postgres rather than a local file
// because Keeper is a stateless cluster (ADR-005) and the instance that serves
// the playback is rarely the one that held the socket.
type RecordingStore interface {
	// Begin creates the recording. Its error refuses the session.
	Begin(ctx context.Context, meta RecordingMeta) error
	// Append writes one part of the body. Parts are dense from seq 0 and never
	// split a cast line.
	Append(ctx context.Context, recordingID string, seq int64, body string) error
	// Finish stamps the terminal state. Best-effort: a recording whose Finish
	// never lands is still complete and replayable, and reads as "the instance
	// died" rather than as an empty session.
	Finish(ctx context.Context, recordingID string, res RecordingResult) error
}

// Recorder opens recordings. The Hub requires one — [NewHub] refuses to build
// without it, which is what makes "recording cannot be turned off" a property
// of the code rather than of a default.
type Recorder interface {
	Open(ctx context.Context, spec RecordingSpec) (Recording, error)
}

// Recording is one live recording. Every method is safe for concurrent use: the
// two directions of a session arrive on different goroutines.
//
// A non-nil error means the session may no longer run — the caller closes it.
type Recording interface {
	// ID is the recording id, for the audit trail and for NIM-148 to fetch by.
	ID() string
	// Output records pty output. soulDropped is what the Soul discarded before
	// this chunk; a non-zero count is recorded as a gap marker.
	Output(stream keeperv1.ConsoleStream, data []byte, soulDropped uint64) error
	// Input records operator keystrokes.
	Input(data []byte) error
	// Resize records a new terminal geometry.
	Resize(cols, rows uint32) error
	// Close flushes and stamps the terminal state. Idempotent.
	Close(ctx context.Context, reason string)
}

// RecorderConfig is the recorder's operator-facing envelope. It deliberately
// has no "enabled" field: ADR-0074(g) lets policy decide where recordings go
// and how long they are kept, not whether a session is recorded.
type RecorderConfig struct {
	// MaxBytes caps one session's recording; 0 → [DefaultMaxRecordingBytes].
	// Negative disables the cap, for an operator who would rather grow the
	// table than lose a session.
	MaxBytes int64
}

func (c RecorderConfig) resolve() RecorderConfig {
	if c.MaxBytes == 0 {
		c.MaxBytes = DefaultMaxRecordingBytes
	}
	return c
}

// StaticRecorderConfig adapts a fixed envelope to the provider shape
// [NewRecorder] wants, for wiring with no live config behind it.
func StaticRecorderConfig(c RecorderConfig) func() RecorderConfig {
	return func() RecorderConfig { return c }
}

// castRecorder is the only [Recorder] implementation.
type castRecorder struct {
	store  RecordingStore
	cfg    func() RecorderConfig
	logger *slog.Logger
	now    func() time.Time
	newID  func() string
}

// NewRecorder builds the recorder over a store. Both arguments are required —
// a recorder with no store would be a way to run an unrecorded console.
//
// cfg is resolved per session rather than once here, so a change to the cap
// applies to the next recording instead of the next restart (ADR-0073(j.5)).
// Sessions already recording keep the cap they started under: the cap decides
// when a recording is closed, and moving that line under a live session would
// close it for a reason its operator never saw. nil → the defaults.
func NewRecorder(store RecordingStore, cfg func() RecorderConfig, logger *slog.Logger) (Recorder, error) {
	if store == nil {
		return nil, errors.New("console: recorder requires a store")
	}
	if logger == nil {
		return nil, errors.New("console: recorder requires a logger")
	}
	if cfg == nil {
		cfg = StaticRecorderConfig(RecorderConfig{})
	}
	return &castRecorder{
		store:  store,
		cfg:    cfg,
		logger: logger,
		now:    time.Now,
		newID:  audit.NewULID,
	}, nil
}

// Open starts a recording. The header row is committed before the caller may
// proceed, so "the session is open" implies "the recording exists".
func (r *castRecorder) Open(ctx context.Context, spec RecordingSpec) (Recording, error) {
	if spec.SessionID == "" {
		return nil, errors.New("console: recording requires a session id")
	}
	start := r.now()
	rec := &castRecording{
		id:       r.newID(),
		store:    r.store,
		maxBytes: r.cfg().resolve().MaxBytes,
		logger:   r.logger,
		now:      r.now,
		start:    start,
		onFail:   spec.OnFailure,
		// Detached: a recording outlives the request that opened it, and the
		// final flush must not be cancelled by the socket going away.
		writeCtx: context.WithoutCancel(ctx),
		parts:    make(chan recordingPart, recordingQueueDepth),
		done:     make(chan struct{}),
	}

	// A player sizes its terminal from the header, and 0x0 is not a terminal.
	// Zero geometry is normal here — a one-shot command has none, and a browser
	// that opened a pane before measuring it sends a resize right after.
	cols, rows := spec.Cols, spec.Rows
	if cols == 0 || rows == 0 {
		cols, rows = defaultCastCols, defaultCastRows
	}
	header := CastHeader{
		Version:   castVersion,
		Width:     cols,
		Height:    rows,
		Timestamp: start.Unix(),
		Title:     string(spec.Kind) + " console on " + spec.SID,
	}
	if err := r.store.Begin(ctx, RecordingMeta{
		RecordingID: rec.id,
		SessionID:   spec.SessionID,
		Kind:        spec.Kind,
		SID:         spec.SID,
		AID:         spec.AID,
		Header:      header,
		StartedAt:   start,
	}); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrRecordingUnavailable, err)
	}

	rec.wg.Add(1)
	go rec.writeLoop()
	return rec, nil
}

// recordingPart is one flushed batch on its way to the store.
type recordingPart struct {
	seq  int64
	body string
}

// maskCarry holds the tail of a stream that might be a vault reference in
// progress, with the timestamp of its first byte so the event it eventually
// joins is stamped when it actually arrived.
type maskCarry struct {
	buf []byte
	at  time.Time
}

// castRecording is one live recording: a cast encoder in front of a batching
// writer.
type castRecording struct {
	id       string
	store    RecordingStore
	maxBytes int64
	logger   *slog.Logger
	now      func() time.Time
	start    time.Time
	onFail   func(error)
	writeCtx context.Context

	parts chan recordingPart
	done  chan struct{}
	wg    sync.WaitGroup

	mu        sync.Mutex
	buf       []byte
	seq       int64
	events    int64
	bytes     int64
	truncated bool
	closed    bool
	// lastElapsed is the timestamp of the last event written, so the cast never
	// goes backwards — see [castRecording.elapsedLocked].
	lastElapsed time.Duration
	// failed latches the first persistence error; every later write returns it.
	failed error
	// carries hold the per-stream mask tails, keyed by cast event code.
	carries map[string]*maskCarry
}

// elapsedLocked is the cast timestamp for an event, clamped to be monotonic.
//
// A carried event is stamped with the arrival time of its FIRST byte, which can
// be earlier than an event written in the meantime: a resize arriving while a
// few input bytes are held back for masking is the case. Write order is already
// correct — only the stamp can go backwards — and a decreasing timestamp is a
// negative delay to a player. Clamping keeps the file replayable and costs at
// most the sub-second skew of the window it happens in.
func (r *castRecording) elapsedLocked(at time.Time) time.Duration {
	d := at.Sub(r.start)
	if d < r.lastElapsed {
		d = r.lastElapsed
	}
	r.lastElapsed = d
	return d
}

func (r *castRecording) ID() string { return r.id }

// Output records pty output. The stream is deliberately NOT recorded: a tty
// merges stdout and stderr onto one fd before Keeper sees either, so a replay
// that separated them would show a screen the operator never had. The frame
// carries it for the live pane, and asciicast has one output code.
func (r *castRecording) Output(_ keeperv1.ConsoleStream, data []byte, soulDropped uint64) error {
	if soulDropped > 0 {
		// The gap is recorded where it happened. Without it a replay would
		// splice two screens together and read as continuous — the same lie the
		// live `dropped_bytes` counter exists to prevent (ADR-0074(e)).
		if err := r.emit(castCodeMarker, []byte("dropped "+strconv.FormatUint(soulDropped, 10)+" bytes"), false); err != nil {
			return err
		}
	}
	if len(data) == 0 {
		return nil
	}
	return r.emit(castCodeOutput, data, true)
}

func (r *castRecording) Input(data []byte) error {
	if len(data) == 0 {
		return nil
	}
	return r.emit(castCodeInput, data, true)
}

func (r *castRecording) Resize(cols, rows uint32) error {
	if cols == 0 || rows == 0 {
		return nil
	}
	geom := strconv.FormatUint(uint64(cols), 10) + "x" + strconv.FormatUint(uint64(rows), 10)
	return r.emit(castCodeResize, []byte(geom), false)
}

// emit masks, encodes and buffers one event.
//
// `carried` selects the streams whose masking must survive a chunk boundary —
// output and input, where an operator's typing arrives one byte at a time. A
// resize or a marker is generated here and has nothing to mask.
func (r *castRecording) emit(code string, data []byte, carried bool) error {
	at := r.now()

	r.mu.Lock()
	defer r.mu.Unlock()

	if r.failed != nil {
		return r.failed
	}
	if r.closed {
		return nil
	}

	if carried {
		var ok bool
		if data, at, ok = r.withCarryLocked(code, data, at); !ok {
			return nil
		}
	}

	line, n := castLine(r.elapsedLocked(at), code, data)
	if r.maxBytes > 0 && r.bytes+int64(n) > r.maxBytes {
		r.truncated = true
		r.flushLocked()
		return fmt.Errorf("%w: %d bytes", ErrRecordingCapped, r.maxBytes)
	}

	r.buf = append(r.buf, line...)
	r.events++
	r.bytes += int64(n)
	if len(r.buf) >= recordingFlushBytes {
		r.flushLocked()
	}
	return r.failed
}

// withCarryLocked splices the stream's held-back tail onto data, masks what is
// now safe to write, and keeps the new tail. Reports false when the whole chunk
// is held back — nothing to emit yet.
//
// The returned timestamp is the arrival time of the carry's FIRST byte, not of
// this chunk: the bytes really did arrive then, and using the later stamp would
// push events past ones already written and break replay ordering.
func (r *castRecording) withCarryLocked(code string, data []byte, at time.Time) ([]byte, time.Time, bool) {
	if r.carries == nil {
		r.carries = make(map[string]*maskCarry, 2)
	}
	c := r.carries[code]
	if c == nil {
		c = &maskCarry{}
		r.carries[code] = c
	}
	if len(c.buf) == 0 {
		c.at = at
	}
	c.buf = append(c.buf, data...)

	split := audit.SafeMaskSplit(c.buf)
	if split == 0 {
		return nil, time.Time{}, false
	}
	// Copied out before the carry is rewound: MaskRefsInBytes returns its input
	// when there is nothing to mask, and that aliases the buffer about to move.
	out := append([]byte(nil), audit.MaskRefsInBytes(c.buf[:split])...)
	stamp := c.at
	c.buf = append(c.buf[:0], c.buf[split:]...)
	c.at = at
	return out, stamp, true
}

// flushCarriesLocked releases every held-back tail. Called on close: a session
// that ended mid-reference must still record the bytes it saw, and there is no
// next chunk to complete the match.
func (r *castRecording) flushCarriesLocked() {
	for code, c := range r.carries {
		if len(c.buf) == 0 {
			continue
		}
		line, n := castLine(r.elapsedLocked(c.at), code, audit.MaskRefsInBytes(c.buf))
		c.buf = nil
		r.buf = append(r.buf, line...)
		r.events++
		r.bytes += int64(n)
	}
}

// castLine encodes one asciicast v2 event line and reports its ENCODED size.
//
// Encoded, not the raw data length: the cap exists to bound what lands in
// Postgres, and a screen of ANSI escapes inflates about six-fold through JSON
// (six characters for every ESC byte). Counting the payload instead would turn
// a 256 MiB cap into a 1.5 GiB table.
func castLine(elapsed time.Duration, code string, data []byte) ([]byte, int) {
	if elapsed < 0 {
		elapsed = 0
	}
	// json.Marshal of a []any is the only encoder that gets the escaping of
	// arbitrary control bytes right, and a cast body is nothing but control
	// bytes. Invalid UTF-8 — a truncated multi-byte sequence at a chunk edge —
	// becomes U+FFFD, which is what a terminal renders for it anyway.
	line, err := json.Marshal([]any{elapsed.Seconds(), code, string(data)})
	if err != nil {
		// []any of float64/string/string cannot fail to marshal; keep the
		// stream well-formed rather than returning a half line.
		return nil, 0
	}
	line = append(line, '\n')
	return line, len(line)
}

// flushLocked hands the buffered body to the writer goroutine.
//
// A full queue is not a reason to drop the batch: it means Postgres has stopped
// accepting writes, and a console whose record stops is a console that stops.
//
// The `closed` guard is what keeps a ticker flush from sending on the channel
// [castRecording.Close] is about to close. Close sets the flag under this lock
// and closes the channel only after releasing it, so a flush either runs before
// the flag is set or sees it.
func (r *castRecording) flushLocked() {
	if len(r.buf) == 0 || r.failed != nil || r.closed {
		return
	}
	part := recordingPart{seq: r.seq, body: string(r.buf)}
	select {
	case r.parts <- part:
		r.seq++
		r.buf = r.buf[:0]
	default:
		r.failLocked(fmt.Errorf("%w: writer is %d parts behind", ErrRecordingUnavailable, recordingQueueDepth))
	}
}

// Flush moves whatever is buffered toward the store. The ticker calls it so a
// quiet session's last line is not held in memory indefinitely.
func (r *castRecording) Flush() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.flushLocked()
}

func (r *castRecording) writeLoop() {
	defer r.wg.Done()
	ticker := time.NewTicker(recordingFlushInterval)
	defer ticker.Stop()

	for {
		select {
		case part, ok := <-r.parts:
			if !ok {
				return
			}
			ctx, cancel := context.WithTimeout(r.writeCtx, recordingWriteTimeout)
			err := r.store.Append(ctx, r.id, part.seq, part.body)
			cancel()
			if err != nil {
				r.fail(fmt.Errorf("%w: %v", ErrRecordingUnavailable, err))
			}
		case <-ticker.C:
			r.Flush()
		}
	}
}

func (r *castRecording) fail(err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.failLocked(err)
}

// failLocked latches the first error and wakes the owner exactly once.
func (r *castRecording) failLocked(err error) {
	if r.failed != nil {
		return
	}
	r.failed = err
	r.logger.Error("console: session recording failed — closing the session",
		slog.String("recording_id", r.id), slog.Any("error", err))
	if r.onFail != nil {
		// Not under the lock: the callback closes the session, which records a
		// terminal state through this same recording.
		go r.onFail(err)
	}
}

// Close flushes what is left and stamps the terminal state.
func (r *castRecording) Close(ctx context.Context, reason string) {
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return
	}
	r.flushCarriesLocked()
	r.flushLocked()
	r.closed = true
	res := RecordingResult{
		FinishedAt:  r.now(),
		CloseReason: reason,
		EventCount:  r.events,
		ByteCount:   r.bytes,
		Truncated:   r.truncated,
	}
	r.mu.Unlock()

	// Drain first, then stamp: a `finished_at` written while parts are still in
	// flight would mark a recording complete that is not.
	close(r.parts)
	r.wg.Wait()

	finishCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), recordingWriteTimeout)
	defer cancel()
	if err := r.store.Finish(finishCtx, r.id, res); err != nil {
		// Nothing to fail closed on — the session is already over, and the body
		// is on disk and replayable. A NULL `finished_at` reads as "the writer
		// did not get to say goodbye", which is exactly what happened.
		r.logger.Warn("console: recording finish failed",
			slog.String("recording_id", r.id), slog.Any("error", err))
	}
}
