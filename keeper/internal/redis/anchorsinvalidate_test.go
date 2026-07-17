package redis

import (
	"context"
	"errors"
	"io"
	"testing"
	"time"
)

func TestAnchorsChangedChannel(t *testing.T) {
	if AnchorsChangedChannel != "sigil:anchors-changed" {
		t.Fatalf("AnchorsChangedChannel = %q, want sigil:anchors-changed", AnchorsChangedChannel)
	}
}

func TestPublishAnchorsChanged_RejectsNilClient(t *testing.T) {
	if _, err := PublishAnchorsChanged(context.Background(), nil); err == nil {
		t.Errorf("expected validation error for nil client, got nil")
	}
}

func TestSubscribeAnchorsChanged_RejectsBadArgs(t *testing.T) {
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
			if _, err := SubscribeAnchorsChanged(ctx, tc.client, lg); err == nil {
				t.Errorf("expected validation error, got nil")
			}
		})
	}
}

// TestAnchorsChanged_RoundTrip — full publish/subscribe cycle: the subscriber
// receives the signal with an At timestamp. No self-filter (the mutating node
// must also reload + re-broadcast).
func TestAnchorsChanged_RoundTrip(t *testing.T) {
	c, _ := newClientMR(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	sub, err := SubscribeAnchorsChanged(ctx, c, discardLog())
	if err != nil {
		t.Fatalf("SubscribeAnchorsChanged: %v", err)
	}
	defer sub.Close()
	if err := sub.Ready(ctx); err != nil {
		t.Fatalf("Ready: %v", err)
	}

	before := time.Now().UTC()
	n, err := PublishAnchorsChanged(ctx, c)
	if err != nil {
		t.Fatalf("PublishAnchorsChanged: %v", err)
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

// TestAnchorsChanged_NoSelfFilter — a node's own publish is NOT filtered:
// the mutating node must receive its own signal and reload the set.
func TestAnchorsChanged_NoSelfFilter(t *testing.T) {
	c, _ := newClientMR(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	sub, err := SubscribeAnchorsChanged(ctx, c, discardLog())
	if err != nil {
		t.Fatalf("SubscribeAnchorsChanged: %v", err)
	}
	defer sub.Close()
	if err := sub.Ready(ctx); err != nil {
		t.Fatalf("Ready: %v", err)
	}

	if _, err := PublishAnchorsChanged(ctx, c); err != nil {
		t.Fatalf("PublishAnchorsChanged: %v", err)
	}
	select {
	case _, ok := <-sub.Channel():
		if !ok {
			t.Fatal("channel closed before self message - self must not be filtered")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("own anchors-changed did not arrive (it should - there is no self-filter)")
	}
}

// TestAnchorsChanged_NoSubscribers — publish with no subscribers → 0, no error.
func TestAnchorsChanged_NoSubscribers(t *testing.T) {
	c, _ := newClientMR(t)
	n, err := PublishAnchorsChanged(context.Background(), c)
	if err != nil {
		t.Fatalf("PublishAnchorsChanged: %v", err)
	}
	if n != 0 {
		t.Errorf("subscribers count = %d, want 0", n)
	}
}

// TestAnchorsChanged_CloseShutsDownGoroutine — Close terminates the goroutine
// and closes the out-channel; a repeat Close is idempotent.
func TestAnchorsChanged_CloseShutsDownGoroutine(t *testing.T) {
	c, _ := newClientMR(t)
	ctx := context.Background()

	sub, err := SubscribeAnchorsChanged(ctx, c, discardLog())
	if err != nil {
		t.Fatalf("SubscribeAnchorsChanged: %v", err)
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

// TestAnchorsChanged_CloseSurvivesConcurrentReceive — race between Close and
// the data stream. -race must pass.
func TestAnchorsChanged_CloseSurvivesConcurrentReceive(t *testing.T) {
	c, _ := newClientMR(t)
	ctx := context.Background()

	sub, err := SubscribeAnchorsChanged(ctx, c, discardLog())
	if err != nil {
		t.Fatalf("SubscribeAnchorsChanged: %v", err)
	}
	if err := sub.Ready(ctx); err != nil {
		t.Fatalf("Ready: %v", err)
	}

	go func() {
		for i := 0; i < 5; i++ {
			_, _ = PublishAnchorsChanged(ctx, c)
		}
	}()

	time.Sleep(20 * time.Millisecond)
	if err := sub.Close(); err != nil && !errors.Is(err, io.EOF) {
		t.Errorf("Close: %v", err)
	}
	for range sub.Channel() {
	}
}
