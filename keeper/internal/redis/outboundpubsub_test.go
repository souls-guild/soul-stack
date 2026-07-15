package redis

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"

	keeperv1 "github.com/souls-guild/soul-stack/proto/gen/go/keeper/v1"
)

func discardLog() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func TestOutboundChannelKey(t *testing.T) {
	got := OutboundChannelKey("host.example.com")
	want := "outbound:host.example.com"
	if got != want {
		t.Fatalf("OutboundChannelKey = %q, want %q", got, want)
	}
}

func TestPublishOutbound_RejectsBadArgs(t *testing.T) {
	c, _ := newClientMR(t)
	ctx := context.Background()
	msg := &keeperv1.FromKeeper{
		Payload: &keeperv1.FromKeeper_HelloReply{HelloReply: &keeperv1.HelloReply{Kid: "k1"}},
	}
	cases := []struct {
		name      string
		client    *Client
		sid       string
		originKID string
		msg       *keeperv1.FromKeeper
	}{
		{"nil client", nil, "sid", "kid", msg},
		{"empty sid", c, "", "kid", msg},
		{"empty originKID", c, "sid", "", msg},
		{"nil msg", c, "sid", "kid", nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := PublishOutbound(ctx, tc.client, tc.sid, tc.originKID, tc.msg); err == nil {
				t.Errorf("expected validation error, got nil")
			}
		})
	}
}

func TestSubscribeOutbound_RejectsBadArgs(t *testing.T) {
	c, _ := newClientMR(t)
	ctx := context.Background()
	cases := []struct {
		name    string
		client  *Client
		sid     string
		selfKID string
		hasLog  bool
	}{
		{"nil client", nil, "sid", "kid", true},
		{"empty sid", c, "", "kid", true},
		{"empty selfKID", c, "sid", "", true},
		{"nil logger", c, "sid", "kid", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var lg *slog.Logger
			if tc.hasLog {
				lg = discardLog()
			}
			if _, err := SubscribeOutbound(ctx, tc.client, tc.sid, tc.selfKID, lg); err == nil {
				t.Errorf("expected validation error, got nil")
			}
		})
	}
}

// TestOutboundPubSub_RoundTrip — a full publish/subscribe cycle with
// different origin_kid values. The subscriber must receive FromKeeper and
// unpack the payload correctly.
func TestOutboundPubSub_RoundTrip(t *testing.T) {
	c, _ := newClientMR(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	sub, err := SubscribeOutbound(ctx, c, "host.example.com", "keeper-receiver", discardLog())
	if err != nil {
		t.Fatalf("SubscribeOutbound: %v", err)
	}
	defer sub.Close()
	if err := sub.Ready(ctx); err != nil {
		t.Fatalf("Ready: %v", err)
	}

	msg := &keeperv1.FromKeeper{
		Payload: &keeperv1.FromKeeper_ApplyRequest{ApplyRequest: &keeperv1.ApplyRequest{
			ApplyId: "01HABC",
			Tasks: []*keeperv1.RenderedTask{
				{Name: "t1", Module: "core.pkg.installed"},
			},
		}},
	}
	n, err := PublishOutbound(ctx, c, "host.example.com", "keeper-sender", msg)
	if err != nil {
		t.Fatalf("PublishOutbound: %v", err)
	}
	if n != 1 {
		t.Errorf("subscribers count = %d, want 1", n)
	}

	select {
	case got, ok := <-sub.Channel():
		if !ok {
			t.Fatal("subscription channel closed before message")
		}
		req := got.GetApplyRequest()
		if req == nil {
			t.Fatalf("payload = %T, want ApplyRequest", got.GetPayload())
		}
		if req.GetApplyId() != "01HABC" {
			t.Errorf("apply_id = %q, want 01HABC", req.GetApplyId())
		}
		if len(req.GetTasks()) != 1 || req.GetTasks()[0].GetName() != "t1" {
			t.Errorf("tasks = %+v", req.GetTasks())
		}
	case <-time.After(2 * time.Second):
		t.Fatal("did not receive message within 2s")
	}
}

// TestOutboundPubSub_SelfFilter — publish with the same origin_kid as the
// subscriber's selfKID → the message is ignored.
func TestOutboundPubSub_SelfFilter(t *testing.T) {
	c, _ := newClientMR(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	sub, err := SubscribeOutbound(ctx, c, "sid", "keeper-self", discardLog())
	if err != nil {
		t.Fatalf("SubscribeOutbound: %v", err)
	}
	defer sub.Close()
	if err := sub.Ready(ctx); err != nil {
		t.Fatalf("Ready: %v", err)
	}

	msg := &keeperv1.FromKeeper{
		Payload: &keeperv1.FromKeeper_CancelApply{CancelApply: &keeperv1.CancelApply{ApplyId: "x"}},
	}
	// Self-origin first.
	if _, err := PublishOutbound(ctx, c, "sid", "keeper-self", msg); err != nil {
		t.Fatalf("PublishOutbound self: %v", err)
	}
	// Then other-origin — it must arrive.
	if _, err := PublishOutbound(ctx, c, "sid", "keeper-other", msg); err != nil {
		t.Fatalf("PublishOutbound other: %v", err)
	}

	select {
	case got := <-sub.Channel():
		if got.GetCancelApply() == nil {
			t.Errorf("payload = %T, want CancelApply (other-origin)", got.GetPayload())
		}
	case <-time.After(2 * time.Second):
		t.Fatal("did not receive other-origin message within 2s")
	}

	// After the other-origin message, the channel must not contain another
	// one (self was filtered). The extra delay gives the subscribe loop a
	// chance to put a self-message in, if the filter weren't working.
	select {
	case got, ok := <-sub.Channel():
		if ok {
			t.Errorf("unexpected extra message: payload = %T", got.GetPayload())
		}
	case <-time.After(150 * time.Millisecond):
		// OK — self was filtered.
	}
}

// TestOutboundPubSub_NoSubscribers — PublishOutbound with no subscribers
// returns 0 without error.
func TestOutboundPubSub_NoSubscribers(t *testing.T) {
	c, _ := newClientMR(t)
	ctx := context.Background()

	msg := &keeperv1.FromKeeper{}
	n, err := PublishOutbound(ctx, c, "sid", "kid", msg)
	if err != nil {
		t.Fatalf("PublishOutbound: %v", err)
	}
	if n != 0 {
		t.Errorf("subscribers count = %d, want 0", n)
	}
}

// TestOutboundPubSub_CloseShutsDownGoroutine — Close on a subscription
// correctly terminates the goroutine and closes the out channel.
func TestOutboundPubSub_CloseShutsDownGoroutine(t *testing.T) {
	c, _ := newClientMR(t)
	ctx := context.Background()

	sub, err := SubscribeOutbound(ctx, c, "sid", "kid", discardLog())
	if err != nil {
		t.Fatalf("SubscribeOutbound: %v", err)
	}
	if err := sub.Ready(ctx); err != nil {
		t.Fatalf("Ready: %v", err)
	}

	if err := sub.Close(); err != nil {
		t.Errorf("Close: %v", err)
	}

	// The channel must close within a reasonable time.
	select {
	case _, ok := <-sub.Channel():
		if ok {
			t.Error("Channel returned value after Close")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Channel not closed within 2s after Close")
	}

	// A double Close — no-op.
	if err := sub.Close(); err != nil {
		t.Errorf("second Close: %v", err)
	}
}

func TestReadSoulLeaseHolder_Empty(t *testing.T) {
	c, _ := newClientMR(t)
	ctx := context.Background()

	v, err := ReadSoulLeaseHolder(ctx, c, "ghost.example.com")
	if err != nil {
		t.Fatalf("ReadSoulLeaseHolder: %v", err)
	}
	if v != "" {
		t.Errorf("holder = %q, want empty", v)
	}
}

func TestReadSoulLeaseHolder_AfterAcquire(t *testing.T) {
	c, _ := newClientMR(t)
	ctx := context.Background()

	l, err := AcquireSoulLease(ctx, c, "host.example.com", "keeper-k1", 5*time.Second)
	if err != nil {
		t.Fatalf("AcquireSoulLease: %v", err)
	}
	defer l.Release(ctx)

	v, err := ReadSoulLeaseHolder(ctx, c, "host.example.com")
	if err != nil {
		t.Fatalf("ReadSoulLeaseHolder: %v", err)
	}
	if v != "keeper-k1" {
		t.Errorf("holder = %q, want keeper-k1", v)
	}
}

func TestReadSoulLeaseHolder_RejectsBadArgs(t *testing.T) {
	c, _ := newClientMR(t)
	ctx := context.Background()

	if _, err := ReadSoulLeaseHolder(ctx, nil, "sid"); err == nil {
		t.Error("nil client returned no error")
	}
	if _, err := ReadSoulLeaseHolder(ctx, c, ""); err == nil {
		t.Error("empty sid returned no error")
	}
}

// TestSubscribeOutbound_CloseSurvivesConcurrentReceive — a race between an
// external Close and the data stream. -race must pass.
func TestSubscribeOutbound_CloseSurvivesConcurrentReceive(t *testing.T) {
	c, _ := newClientMR(t)
	ctx := context.Background()

	sub, err := SubscribeOutbound(ctx, c, "sid", "kid", discardLog())
	if err != nil {
		t.Fatalf("SubscribeOutbound: %v", err)
	}
	if err := sub.Ready(ctx); err != nil {
		t.Fatalf("Ready: %v", err)
	}

	go func() {
		for i := 0; i < 5; i++ {
			_, _ = PublishOutbound(ctx, c, "sid", "other", &keeperv1.FromKeeper{
				Payload: &keeperv1.FromKeeper_CancelApply{CancelApply: &keeperv1.CancelApply{ApplyId: "x"}},
			})
		}
	}()

	// Don't drain — the channel may fill up; Close must still terminate the
	// goroutine correctly.
	time.Sleep(20 * time.Millisecond)
	if err := sub.Close(); err != nil && !errors.Is(err, io.EOF) {
		t.Errorf("Close: %v", err)
	}

	// The channel is closed — drain to empty.
	for range sub.Channel() {
	}
}
