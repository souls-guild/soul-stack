package redis

import (
	"context"
	"errors"
	"io"
	"testing"
	"time"
)

func TestSigilInvalidateChannel(t *testing.T) {
	if SigilInvalidateChannel != "sigil:invalidate" {
		t.Fatalf("SigilInvalidateChannel = %q, want sigil:invalidate", SigilInvalidateChannel)
	}
}

func TestPublishSigilInvalidate_RejectsNilClient(t *testing.T) {
	if _, err := PublishSigilInvalidate(context.Background(), nil); err == nil {
		t.Errorf("expected validation error for nil client, got nil")
	}
}

func TestSubscribeSigilInvalidate_RejectsBadArgs(t *testing.T) {
	c, _ := newClientMR(t)
	ctx := context.Background()
	cases := []struct {
		name   string
		client *Client
		hasLog bool
	}{
		{"nil client", nil, true},
		{"nil logger", c, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var lg = discardLog()
			if !tc.hasLog {
				lg = nil
			}
			if _, err := SubscribeSigilInvalidate(ctx, tc.client, lg); err == nil {
				t.Errorf("expected validation error, got nil")
			}
		})
	}
}

// TestSigilInvalidate_RoundTrip — full publish/subscribe cycle: a
// subscriber receives the invalidate message with an At stamp. There's no
// self-filter (the mutating node must also re-broadcast to its own Souls).
func TestSigilInvalidate_RoundTrip(t *testing.T) {
	c, _ := newClientMR(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	sub, err := SubscribeSigilInvalidate(ctx, c, discardLog())
	if err != nil {
		t.Fatalf("SubscribeSigilInvalidate: %v", err)
	}
	defer sub.Close()
	if err := sub.Ready(ctx); err != nil {
		t.Fatalf("Ready: %v", err)
	}

	before := time.Now().UTC()
	n, err := PublishSigilInvalidate(ctx, c)
	if err != nil {
		t.Fatalf("PublishSigilInvalidate: %v", err)
	}
	if n != 1 {
		t.Errorf("subscribers count = %d, want 1", n)
	}

	select {
	case got, ok := <-sub.Channel():
		if !ok {
			t.Fatal("subscription channel closed before message")
		}
		if got.At.Before(before.Add(-time.Second)) {
			t.Errorf("At=%v is before Publish call (=%v)", got.At, before)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("did not receive message within 2s")
	}
}

// TestSigilInvalidate_NoSelfFilter — its own publish is NOT filtered: the
// mutating node must receive its own signal and re-broadcast to its Souls.
func TestSigilInvalidate_NoSelfFilter(t *testing.T) {
	c, _ := newClientMR(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	sub, err := SubscribeSigilInvalidate(ctx, c, discardLog())
	if err != nil {
		t.Fatalf("SubscribeSigilInvalidate: %v", err)
	}
	defer sub.Close()
	if err := sub.Ready(ctx); err != nil {
		t.Fatalf("Ready: %v", err)
	}

	if _, err := PublishSigilInvalidate(ctx, c); err != nil {
		t.Fatalf("PublishSigilInvalidate: %v", err)
	}
	select {
	case _, ok := <-sub.Channel():
		if !ok {
			t.Fatal("channel closed before self message — self не должен фильтроваться")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("собственный invalidate не пришёл (а должен — self-filter-а нет)")
	}
}

// TestSigilInvalidate_NoSubscribers — publish with no subscribers → 0, no error.
func TestSigilInvalidate_NoSubscribers(t *testing.T) {
	c, _ := newClientMR(t)
	n, err := PublishSigilInvalidate(context.Background(), c)
	if err != nil {
		t.Fatalf("PublishSigilInvalidate: %v", err)
	}
	if n != 0 {
		t.Errorf("subscribers count = %d, want 0", n)
	}
}

// TestSigilInvalidate_CloseShutsDownGoroutine — Close terminates the
// goroutine and closes the out channel; a repeated Close is idempotent.
func TestSigilInvalidate_CloseShutsDownGoroutine(t *testing.T) {
	c, _ := newClientMR(t)
	ctx := context.Background()

	sub, err := SubscribeSigilInvalidate(ctx, c, discardLog())
	if err != nil {
		t.Fatalf("SubscribeSigilInvalidate: %v", err)
	}
	if err := sub.Ready(ctx); err != nil {
		t.Fatalf("Ready: %v", err)
	}

	if err := sub.Close(); err != nil {
		t.Errorf("Close: %v", err)
	}
	select {
	case _, ok := <-sub.Channel():
		if ok {
			t.Error("Channel returned value after Close")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Channel not closed within 2s after Close")
	}
	if err := sub.Close(); err != nil {
		t.Errorf("second Close: %v", err)
	}
}

// TestSigilInvalidate_CloseSurvivesConcurrentReceive — a race between Close
// and the data stream. -race must pass.
func TestSigilInvalidate_CloseSurvivesConcurrentReceive(t *testing.T) {
	c, _ := newClientMR(t)
	ctx := context.Background()

	sub, err := SubscribeSigilInvalidate(ctx, c, discardLog())
	if err != nil {
		t.Fatalf("SubscribeSigilInvalidate: %v", err)
	}
	if err := sub.Ready(ctx); err != nil {
		t.Fatalf("Ready: %v", err)
	}

	go func() {
		for i := 0; i < 5; i++ {
			_, _ = PublishSigilInvalidate(ctx, c)
		}
	}()

	time.Sleep(20 * time.Millisecond)
	if err := sub.Close(); err != nil && !errors.Is(err, io.EOF) {
		t.Errorf("Close: %v", err)
	}
	for range sub.Channel() {
	}
}
