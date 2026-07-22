package grpc

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// fakeSoulDB — soul.ExecQueryRower stub: counts Exec calls (UpdateLastSeen
// only goes through Exec). RowsAffected=1 by default, so UpdateLastSeen
// doesn't return ErrSoulNotFound.
type fakeSoulDB struct {
	execCalls int
	lastArgs  []any
}

func (f *fakeSoulDB) Exec(_ context.Context, _ string, args ...any) (pgconn.CommandTag, error) {
	f.execCalls++
	f.lastArgs = args
	return pgconn.NewCommandTag("UPDATE 1"), nil
}

func (f *fakeSoulDB) QueryRow(context.Context, string, ...any) pgx.Row {
	return errRow{err: pgx.ErrNoRows}
}

func (f *fakeSoulDB) Query(context.Context, string, ...any) (pgx.Rows, error) {
	return nil, nil
}

func TestLastSeenFlusher_ThrottlesWithinWindow(t *testing.T) {
	f := newLastSeenFlusher(30 * time.Second)
	base := time.Now()

	if !f.shouldFlush("host.example.com", base) {
		t.Fatal("first call should flush")
	}
	if f.shouldFlush("host.example.com", base.Add(10*time.Second)) {
		t.Error("second call within the window should not flush")
	}
	if f.shouldFlush("host.example.com", base.Add(29*time.Second)) {
		t.Error("call at window boundary (29s < 30s) should not flush")
	}
	if !f.shouldFlush("host.example.com", base.Add(30*time.Second)) {
		t.Error("call at/after the interval (30s) should flush")
	}
}

func TestLastSeenFlusher_PerSIDIndependent(t *testing.T) {
	f := newLastSeenFlusher(30 * time.Second)
	base := time.Now()

	if !f.shouldFlush("a.example.com", base) {
		t.Fatal("first flush for a")
	}
	// A different SID within the first one's window should not be throttled.
	if !f.shouldFlush("b.example.com", base.Add(time.Second)) {
		t.Error("first flush for b should pass independently of a")
	}
	if f.shouldFlush("a.example.com", base.Add(time.Second)) {
		t.Error("a is throttled within its own window")
	}
}

func TestLastSeenFlusher_ForgetResetsThrottle(t *testing.T) {
	f := newLastSeenFlusher(30 * time.Second)
	base := time.Now()

	if !f.shouldFlush("host.example.com", base) {
		t.Fatal("first flush")
	}
	f.forget("host.example.com")
	// After forget, the next call within the window flushes again (as if
	// it were a new connection of the same SID).
	if !f.shouldFlush("host.example.com", base.Add(time.Second)) {
		t.Error("after forget, a call within the window should flush")
	}
}

func TestNewEventStreamHandler_FlushIntervalDefault(t *testing.T) {
	h := newEventStreamHandler(EventStreamDeps{
		SeedDB:      &fakeSeedDB{},
		AuditWriter: nopAudit{},
		KID:         "kid-test",
	}, discardLogger(t))
	if h.lastSeenFlush.interval != defaultLastSeenFlushInterval {
		t.Errorf("interval = %v, want default %v", h.lastSeenFlush.interval, defaultLastSeenFlushInterval)
	}
}

func TestNewEventStreamHandler_FlushIntervalFromDeps(t *testing.T) {
	h := newEventStreamHandler(EventStreamDeps{
		SeedDB:                &fakeSeedDB{},
		AuditWriter:           nopAudit{},
		KID:                   "kid-test",
		LastSeenFlushInterval: 15 * time.Second,
	}, discardLogger(t))
	if h.lastSeenFlush.interval != 15*time.Second {
		t.Errorf("interval = %v, want 15s", h.lastSeenFlush.interval)
	}
}

// TestFlushLastSeen_NoSoulDB_NoOp — without a SoulDB, flushing is disabled
// (dev / unit mode): the heartbeat lives only in Redis, PG is untouched.
func TestFlushLastSeen_NoSoulDB_NoOp(t *testing.T) {
	h := newEventStreamHandler(EventStreamDeps{
		SeedDB:      &fakeSeedDB{},
		AuditWriter: nopAudit{},
		KID:         "kid-test",
	}, discardLogger(t))
	// shouldFlush shouldn't even be called with a nil SoulDB, but the main
	// thing we're checking is no panic and the fact that there's nowhere
	// to write.
	h.flushLastSeen(context.Background(), "host.example.com", time.Now())
}

// TestFlushLastSeen_WritesThenThrottles — the first flush writes to PG, the
// second one within the window doesn't (one Exec for both calls).
func TestFlushLastSeen_WritesThenThrottles(t *testing.T) {
	db := &fakeSoulDB{}
	h := newEventStreamHandler(EventStreamDeps{
		SeedDB:                &fakeSeedDB{},
		SoulDB:                db,
		AuditWriter:           nopAudit{},
		KID:                   "kid-test",
		LastSeenFlushInterval: 30 * time.Second,
	}, discardLogger(t))

	now := time.Now()
	h.flushLastSeen(context.Background(), "host.example.com", now)
	if db.execCalls != 1 {
		t.Fatalf("after first flush execCalls = %d, want 1", db.execCalls)
	}
	h.flushLastSeen(context.Background(), "host.example.com", now.Add(5*time.Second))
	if db.execCalls != 1 {
		t.Errorf("second flush within window: execCalls = %d, want 1 (throttled)", db.execCalls)
	}
	h.flushLastSeen(context.Background(), "host.example.com", now.Add(31*time.Second))
	if db.execCalls != 2 {
		t.Errorf("flush after interval: execCalls = %d, want 2", db.execCalls)
	}
}

// TestTouchSeen_FlushesLastSeen — touchSeen flushes PG regardless of Redis
// (the Redis layer is optional, the PG snapshot is needed by the Reaper
// independently).
func TestTouchSeen_FlushesLastSeen(t *testing.T) {
	db := &fakeSoulDB{}
	h := newEventStreamHandler(EventStreamDeps{
		SeedDB:                &fakeSeedDB{},
		SoulDB:                db,
		AuditWriter:           nopAudit{},
		KID:                   "kid-test",
		LastSeenFlushInterval: 30 * time.Second,
	}, discardLogger(t))

	h.touchSeen(context.Background(), "host.example.com")
	if db.execCalls != 1 {
		t.Fatalf("touchSeen: execCalls = %d, want 1", db.execCalls)
	}
	// UpdateLastSeen arguments: sid, at(UTC), kid.
	if len(db.lastArgs) != 3 {
		t.Fatalf("UpdateLastSeen args = %d, want 3", len(db.lastArgs))
	}
	if db.lastArgs[0] != "host.example.com" {
		t.Errorf("arg[0] sid = %v, want host.example.com", db.lastArgs[0])
	}
	if db.lastArgs[2] != "kid-test" {
		t.Errorf("arg[2] kid = %v, want kid-test", db.lastArgs[2])
	}
}
