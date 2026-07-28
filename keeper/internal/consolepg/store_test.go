package consolepg

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/souls-guild/soul-stack/keeper/internal/console"
)

// fakeExec records what the store sent to Postgres.
type fakeExec struct {
	sql  []string
	args [][]any
	err  error
}

func (f *fakeExec) Exec(_ context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	f.sql = append(f.sql, sql)
	f.args = append(f.args, args)
	return pgconn.CommandTag{}, f.err
}

func TestStore_BeginBakesTheTTLIntoTheRow(t *testing.T) {
	fe := &fakeExec{}
	s := newStoreFromExecer(fe, 48*time.Hour)
	started := time.Date(2026, 7, 27, 10, 0, 0, 0, time.UTC)

	err := s.Begin(context.Background(), console.RecordingMeta{
		RecordingID: "rec-1",
		SessionID:   "sess-1",
		Kind:        console.RecordingInteractive,
		SID:         "host-a",
		AID:         "archon-a",
		Header:      console.CastHeader{Version: 2, Width: 80, Height: 24},
		StartedAt:   started,
	})
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	if len(fe.args) != 1 {
		t.Fatalf("Exec calls = %d, want 1", len(fe.args))
	}
	args := fe.args[0]
	if got := args[7]; got != started.Add(48*time.Hour) {
		t.Fatalf("ttl_at = %v, want started_at + retention", got)
	}
	// Structured, so a reader can size a terminal without parsing the body.
	var header console.CastHeader
	if err := json.Unmarshal(args[5].([]byte), &header); err != nil {
		t.Fatalf("cast_header is not JSON: %v", err)
	}
	if header.Width != 80 || header.Height != 24 {
		t.Fatalf("cast header = %+v, want the session geometry", header)
	}
}

// Retention has no "keep forever": a recording of every console ever opened is
// a table nobody prunes.
func TestStore_ZeroRetentionFallsBackToTheDefault(t *testing.T) {
	fe := &fakeExec{}
	s := newStoreFromExecer(fe, 0)
	started := time.Now()

	if err := s.Begin(context.Background(), console.RecordingMeta{
		RecordingID: "rec-1", SessionID: "sess-1", StartedAt: started,
	}); err != nil {
		t.Fatalf("Begin: %v", err)
	}
	ttl, _ := fe.args[0][7].(time.Time)
	if got := ttl.Sub(started); got != DefaultRetention {
		t.Fatalf("retention = %v, want the %v default", got, DefaultRetention)
	}
}

// A store that cannot write is what makes a console fail closed, so the error
// must reach the caller rather than being swallowed.
func TestStore_BeginPropagatesTheError(t *testing.T) {
	want := errors.New("pg down")
	s := newStoreFromExecer(&fakeExec{err: want}, time.Hour)

	if err := s.Begin(context.Background(), console.RecordingMeta{RecordingID: "rec-1"}); !errors.Is(err, want) {
		t.Fatalf("err = %v, want it to wrap %v", err, want)
	}
}

func TestStore_AppendAndFinish(t *testing.T) {
	fe := &fakeExec{}
	s := newStoreFromExecer(fe, time.Hour)

	if err := s.Append(context.Background(), "rec-1", 3, `[0.1,"o","hi"]`+"\n"); err != nil {
		t.Fatalf("Append: %v", err)
	}
	if !strings.Contains(fe.sql[0], "console_recording_parts") {
		t.Fatalf("Append wrote to %q", fe.sql[0])
	}
	if fe.args[0][1] != int64(3) {
		t.Fatalf("seq = %v, want 3", fe.args[0][1])
	}

	if err := s.Finish(context.Background(), "rec-1", console.RecordingResult{
		CloseReason: "exit", EventCount: 9, ByteCount: 512, Truncated: true,
	}); err != nil {
		t.Fatalf("Finish: %v", err)
	}
	last := fe.args[1]
	if last[2] != "exit" || last[3] != int64(9) || last[4] != int64(512) || last[5] != true {
		t.Fatalf("Finish args = %v, want the terminal state of the recording", last)
	}
	// finished_at is stamped even when the caller left it zero, so a NULL in the
	// column keeps its meaning: the writer never got to say goodbye.
	if ts, ok := last[1].(time.Time); !ok || ts.IsZero() {
		t.Fatalf("finished_at = %v, want a stamped time", last[1])
	}
}
