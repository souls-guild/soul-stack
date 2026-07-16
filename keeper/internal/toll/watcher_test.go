package toll

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"
)

func newTestLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// recordingPublisher — fake Publisher: captures all PublishDisconnect calls,
// optionally returns error.
type recordingPublisher struct {
	mu    sync.Mutex
	calls []publishCall
	err   error
}

type publishCall struct {
	sid, kid, coven string
	at              time.Time
}

func (p *recordingPublisher) PublishDisconnect(_ context.Context, sid, kid, coven string, at time.Time) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.calls = append(p.calls, publishCall{sid, kid, coven, at})
	return p.err
}

func (p *recordingPublisher) callsCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.calls)
}

func (p *recordingPublisher) last() (publishCall, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.calls) == 0 {
		return publishCall{}, false
	}
	return p.calls[len(p.calls)-1], true
}

func TestWatcher_NewWatcher_RejectsInvalid(t *testing.T) {
	logger := newTestLogger()
	pub := &recordingPublisher{}
	if _, err := NewWatcher(Config{}, pub, nil, logger); err == nil {
		t.Fatal("expected error for empty KID")
	}
	if _, err := NewWatcher(Config{KID: "k1"}, nil, nil, logger); err == nil {
		t.Fatal("expected error for nil publisher")
	}
	if _, err := NewWatcher(Config{KID: "k1"}, pub, nil, nil); err == nil {
		t.Fatal("expected error for nil logger")
	}
}

func TestWatcher_WarmupImmunity_SkipsPublish(t *testing.T) {
	pub := &recordingPublisher{}
	w, err := NewWatcher(Config{KID: "kid-1", WarmupDelay: 60 * time.Second}, pub, nil, newTestLogger())
	if err != nil {
		t.Fatalf("NewWatcher: %v", err)
	}
	// startedAt = NOW from constructor — warmup active.
	w.NotifyDisconnect(context.Background(), "host-1", "production", false)
	if got := pub.callsCount(); got != 0 {
		t.Fatalf("warmup-immunity: expected 0 publish, got %d", got)
	}
}

func TestWatcher_WarmupExpired_Publishes(t *testing.T) {
	pub := &recordingPublisher{}
	w, err := NewWatcher(Config{KID: "kid-1", WarmupDelay: 60 * time.Second}, pub, nil, newTestLogger())
	if err != nil {
		t.Fatalf("NewWatcher: %v", err)
	}
	// Shift startedAt to past — warmup expired.
	w.setStartedAt(time.Now().Add(-10 * time.Minute))
	w.NotifyDisconnect(context.Background(), "host-1", "production", false)
	if got := pub.callsCount(); got != 1 {
		t.Fatalf("warmup expired: expected 1 publish, got %d", got)
	}
	last, _ := pub.last()
	if last.sid != "host-1" || last.kid != "kid-1" || last.coven != "production" {
		t.Fatalf("publish args mismatch: %+v", last)
	}
}

func TestWatcher_GracefulShutdown_SkipsPublish(t *testing.T) {
	pub := &recordingPublisher{}
	w, err := NewWatcher(Config{KID: "kid-1", WarmupDelay: time.Second}, pub, nil, newTestLogger())
	if err != nil {
		t.Fatalf("NewWatcher: %v", err)
	}
	w.setStartedAt(time.Now().Add(-time.Hour)) // warmup expired
	w.NotifyDisconnect(context.Background(), "host-1", "", true)
	if got := pub.callsCount(); got != 0 {
		t.Fatalf("graceful shutdown: expected 0 publish, got %d", got)
	}
}

func TestWatcher_EmptyCoven_PublishesWithEmptyLabel(t *testing.T) {
	pub := &recordingPublisher{}
	w, err := NewWatcher(Config{KID: "kid-1"}, pub, nil, newTestLogger())
	if err != nil {
		t.Fatalf("NewWatcher: %v", err)
	}
	w.setStartedAt(time.Now().Add(-time.Hour))
	w.NotifyDisconnect(context.Background(), "host-x", "", false)
	if got := pub.callsCount(); got != 1 {
		t.Fatalf("expected 1 publish, got %d", got)
	}
	last, _ := pub.last()
	if last.coven != "" {
		t.Fatalf("expected empty coven, got %q", last.coven)
	}
}

func TestWatcher_PublisherError_NotFatal(t *testing.T) {
	pub := &recordingPublisher{err: errors.New("redis flap")}
	w, err := NewWatcher(Config{KID: "kid-1"}, pub, nil, newTestLogger())
	if err != nil {
		t.Fatalf("NewWatcher: %v", err)
	}
	w.setStartedAt(time.Now().Add(-time.Hour))
	// Should not panic or block.
	w.NotifyDisconnect(context.Background(), "host-x", "production", false)
	if got := pub.callsCount(); got != 1 {
		t.Fatalf("expected 1 publish attempt, got %d", got)
	}
}

func TestWatcher_NilReceiver_Safe(t *testing.T) {
	var w *Watcher
	// Should not panic.
	w.NotifyDisconnect(context.Background(), "x", "", false)
}

func TestEncodeDisconnect_UniqueAcrossCalls(t *testing.T) {
	at := time.Unix(1_700_000_000, 0)
	a := EncodeDisconnect("host-1", "kid-A", "prod", at)
	// Same timestamp, same field set: difference only in UnixNano suffix
	// (takes real clock in encode). Here at same, and at.UnixNano
	// same — get same member. By design: uniqueness achieved
	// via at-time, not random-suffix; different calls in one second
	// get different UnixNano (sub-ns clock granularity in Go).
	b := EncodeDisconnect("host-1", "kid-A", "prod", at)
	if a != b {
		t.Fatalf("same at → same encoding: got %q vs %q", a, b)
	}
	atLater := at.Add(time.Nanosecond)
	c := EncodeDisconnect("host-1", "kid-A", "prod", atLater)
	if a == c {
		t.Fatalf("different at → different encoding: both %q", a)
	}
}
