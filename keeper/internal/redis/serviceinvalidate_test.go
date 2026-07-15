package redis

import (
	"context"
	"errors"
	"io"
	"testing"
	"time"
)

func TestServiceInvalidateChannel(t *testing.T) {
	if ServiceInvalidateChannel != "service:invalidate" {
		t.Fatalf("ServiceInvalidateChannel = %q, want service:invalidate", ServiceInvalidateChannel)
	}
}

func TestPublishServiceInvalidate_RejectsBadArgs(t *testing.T) {
	c, _ := newClientMR(t)
	ctx := context.Background()
	cases := []struct {
		name      string
		client    *Client
		originKID string
	}{
		{"nil client", nil, "kid"},
		{"empty originKID", c, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := PublishServiceInvalidate(ctx, tc.client, tc.originKID); err == nil {
				t.Errorf("expected validation error, got nil")
			}
		})
	}
}

func TestSubscribeServiceInvalidate_RejectsBadArgs(t *testing.T) {
	c, _ := newClientMR(t)
	ctx := context.Background()
	cases := []struct {
		name    string
		client  *Client
		selfKID string
		hasLog  bool
	}{
		{"nil client", nil, "kid", true},
		{"empty selfKID", c, "", true},
		{"nil logger", c, "kid", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var lg = discardLog()
			if !tc.hasLog {
				lg = nil
			}
			if _, err := SubscribeServiceInvalidate(ctx, tc.client, tc.selfKID, lg); err == nil {
				t.Errorf("expected validation error, got nil")
			}
		})
	}
}

// TestServiceInvalidate_RoundTrip — full publish/subscribe cycle: a
// subscriber with a different KID receives the invalidate message with an
// At stamp.
func TestServiceInvalidate_RoundTrip(t *testing.T) {
	c, _ := newClientMR(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	sub, err := SubscribeServiceInvalidate(ctx, c, "keeper-receiver", discardLog())
	if err != nil {
		t.Fatalf("SubscribeServiceInvalidate: %v", err)
	}
	defer sub.Close()
	if err := sub.Ready(ctx); err != nil {
		t.Fatalf("Ready: %v", err)
	}

	before := time.Now().UTC()
	n, err := PublishServiceInvalidate(ctx, c, "keeper-sender")
	if err != nil {
		t.Fatalf("PublishServiceInvalidate: %v", err)
	}
	if n != 1 {
		t.Errorf("subscribers count = %d, want 1", n)
	}

	select {
	case got, ok := <-sub.Channel():
		if !ok {
			t.Fatal("subscription channel closed before message")
		}
		if got.OriginKID != "keeper-sender" {
			t.Errorf("origin_kid = %q, want keeper-sender", got.OriginKID)
		}
		if got.At.Before(before.Add(-time.Second)) {
			t.Errorf("At=%v is before Publish call (=%v)", got.At, before)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("did not receive message within 2s")
	}
}

// TestServiceInvalidate_SelfFilter — publishing with the same origin_kid as
// the subscriber's selfKID → ignored; a different origin gets through.
func TestServiceInvalidate_SelfFilter(t *testing.T) {
	c, _ := newClientMR(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	sub, err := SubscribeServiceInvalidate(ctx, c, "keeper-self", discardLog())
	if err != nil {
		t.Fatalf("SubscribeServiceInvalidate: %v", err)
	}
	defer sub.Close()
	if err := sub.Ready(ctx); err != nil {
		t.Fatalf("Ready: %v", err)
	}

	// Self-origin first — should be filtered out.
	if _, err := PublishServiceInvalidate(ctx, c, "keeper-self"); err != nil {
		t.Fatalf("PublishServiceInvalidate self: %v", err)
	}
	// Then a different origin — should arrive.
	if _, err := PublishServiceInvalidate(ctx, c, "keeper-other"); err != nil {
		t.Fatalf("PublishServiceInvalidate other: %v", err)
	}

	select {
	case got := <-sub.Channel():
		if got.OriginKID != "keeper-other" {
			t.Errorf("origin_kid = %q, want keeper-other (self filtered)", got.OriginKID)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("did not receive other-origin message within 2s")
	}

	// Self shouldn't arrive as an extra message.
	select {
	case got, ok := <-sub.Channel():
		if ok {
			t.Errorf("unexpected extra message: origin_kid = %q", got.OriginKID)
		}
	case <-time.After(150 * time.Millisecond):
		// OK — self was filtered out.
	}
}

// TestServiceInvalidate_NoSubscribers — publish with no subscribers → 0, no error.
func TestServiceInvalidate_NoSubscribers(t *testing.T) {
	c, _ := newClientMR(t)
	ctx := context.Background()

	n, err := PublishServiceInvalidate(ctx, c, "kid")
	if err != nil {
		t.Fatalf("PublishServiceInvalidate: %v", err)
	}
	if n != 0 {
		t.Errorf("subscribers count = %d, want 0", n)
	}
}

// TestServiceInvalidate_CloseShutsDownGoroutine — Close terminates the
// goroutine and closes the out channel; a repeated Close is idempotent.
func TestServiceInvalidate_CloseShutsDownGoroutine(t *testing.T) {
	c, _ := newClientMR(t)
	ctx := context.Background()

	sub, err := SubscribeServiceInvalidate(ctx, c, "kid", discardLog())
	if err != nil {
		t.Fatalf("SubscribeServiceInvalidate: %v", err)
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

// TestServiceInvalidate_CloseSurvivesConcurrentReceive — a race between
// Close and the data stream. -race must pass.
func TestServiceInvalidate_CloseSurvivesConcurrentReceive(t *testing.T) {
	c, _ := newClientMR(t)
	ctx := context.Background()

	sub, err := SubscribeServiceInvalidate(ctx, c, "kid", discardLog())
	if err != nil {
		t.Fatalf("SubscribeServiceInvalidate: %v", err)
	}
	if err := sub.Ready(ctx); err != nil {
		t.Fatalf("Ready: %v", err)
	}

	go func() {
		for i := 0; i < 5; i++ {
			_, _ = PublishServiceInvalidate(ctx, c, "other")
		}
	}()

	time.Sleep(20 * time.Millisecond)
	if err := sub.Close(); err != nil && !errors.Is(err, io.EOF) {
		t.Errorf("Close: %v", err)
	}
	for range sub.Channel() {
	}
}
