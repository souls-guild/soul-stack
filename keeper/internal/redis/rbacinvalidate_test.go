package redis

import (
	"context"
	"errors"
	"io"
	"testing"
	"time"
)

func TestRBACInvalidateChannel(t *testing.T) {
	if RBACInvalidateChannel != "rbac:invalidate" {
		t.Fatalf("RBACInvalidateChannel = %q, want rbac:invalidate", RBACInvalidateChannel)
	}
}

func TestPublishRBACInvalidate_RejectsBadArgs(t *testing.T) {
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
			if _, err := PublishRBACInvalidate(ctx, tc.client, tc.originKID); err == nil {
				t.Errorf("expected validation error, got nil")
			}
		})
	}
}

func TestSubscribeRBACInvalidate_RejectsBadArgs(t *testing.T) {
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
			if _, err := SubscribeRBACInvalidate(ctx, tc.client, tc.selfKID, lg); err == nil {
				t.Errorf("expected validation error, got nil")
			}
		})
	}
}

// TestRBACInvalidate_RoundTrip — full publish/subscribe cycle: a subscriber
// with a different KID receives the invalidate message with an At stamp.
func TestRBACInvalidate_RoundTrip(t *testing.T) {
	c, _ := newClientMR(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	sub, err := SubscribeRBACInvalidate(ctx, c, "keeper-receiver", discardLog())
	if err != nil {
		t.Fatalf("SubscribeRBACInvalidate: %v", err)
	}
	defer sub.Close()
	if err := sub.Ready(ctx); err != nil {
		t.Fatalf("Ready: %v", err)
	}

	before := time.Now().UTC()
	n, err := PublishRBACInvalidate(ctx, c, "keeper-sender")
	if err != nil {
		t.Fatalf("PublishRBACInvalidate: %v", err)
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

// TestRBACInvalidate_SelfFilter — publishing with the same origin_kid as the
// subscriber's selfKID → ignored; a different origin gets through.
func TestRBACInvalidate_SelfFilter(t *testing.T) {
	c, _ := newClientMR(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	sub, err := SubscribeRBACInvalidate(ctx, c, "keeper-self", discardLog())
	if err != nil {
		t.Fatalf("SubscribeRBACInvalidate: %v", err)
	}
	defer sub.Close()
	if err := sub.Ready(ctx); err != nil {
		t.Fatalf("Ready: %v", err)
	}

	// Self-origin first — should be filtered out.
	if _, err := PublishRBACInvalidate(ctx, c, "keeper-self"); err != nil {
		t.Fatalf("PublishRBACInvalidate self: %v", err)
	}
	// Then a different origin — should arrive.
	if _, err := PublishRBACInvalidate(ctx, c, "keeper-other"); err != nil {
		t.Fatalf("PublishRBACInvalidate other: %v", err)
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

// TestRBACInvalidate_NoSubscribers — publish with no subscribers → 0, no error.
func TestRBACInvalidate_NoSubscribers(t *testing.T) {
	c, _ := newClientMR(t)
	ctx := context.Background()

	n, err := PublishRBACInvalidate(ctx, c, "kid")
	if err != nil {
		t.Fatalf("PublishRBACInvalidate: %v", err)
	}
	if n != 0 {
		t.Errorf("subscribers count = %d, want 0", n)
	}
}

// TestRBACInvalidate_CloseShutsDownGoroutine — Close terminates the goroutine
// and closes the out channel; a repeated Close is idempotent.
func TestRBACInvalidate_CloseShutsDownGoroutine(t *testing.T) {
	c, _ := newClientMR(t)
	ctx := context.Background()

	sub, err := SubscribeRBACInvalidate(ctx, c, "kid", discardLog())
	if err != nil {
		t.Fatalf("SubscribeRBACInvalidate: %v", err)
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

// TestRBACInvalidate_CloseSurvivesConcurrentReceive — a race between Close
// and the data stream. -race must pass.
func TestRBACInvalidate_CloseSurvivesConcurrentReceive(t *testing.T) {
	c, _ := newClientMR(t)
	ctx := context.Background()

	sub, err := SubscribeRBACInvalidate(ctx, c, "kid", discardLog())
	if err != nil {
		t.Fatalf("SubscribeRBACInvalidate: %v", err)
	}
	if err := sub.Ready(ctx); err != nil {
		t.Fatalf("Ready: %v", err)
	}

	go func() {
		for i := 0; i < 5; i++ {
			_, _ = PublishRBACInvalidate(ctx, c, "other")
		}
	}()

	time.Sleep(20 * time.Millisecond)
	if err := sub.Close(); err != nil && !errors.Is(err, io.EOF) {
		t.Errorf("Close: %v", err)
	}
	for range sub.Channel() {
	}
}
