package redis

import (
	"context"
	"testing"
	"time"

	keeperv1 "github.com/souls-guild/soul-stack/proto/gen/go/keeper/v1"
)

func TestConsoleKeys(t *testing.T) {
	if got, want := ConsoleOwnerKey("01JSESSION"), "console:owner:01JSESSION"; got != want {
		t.Fatalf("ConsoleOwnerKey = %q, want %q", got, want)
	}
	if got, want := ConsoleUpstreamChannelKey("kid-a"), "console:kid-a"; got != want {
		t.Fatalf("ConsoleUpstreamChannelKey = %q, want %q", got, want)
	}
}

func TestConsoleSessionClaimLifecycle(t *testing.T) {
	c, _ := newClientMR(t)
	ctx := context.Background()

	// No claim yet — the holder must learn "nobody owns this" rather than an error.
	owner, err := ReadConsoleSessionOwner(ctx, c, "s1")
	if err != nil {
		t.Fatalf("ReadConsoleSessionOwner: %v", err)
	}
	if owner.KID != "" {
		t.Fatalf("owner = %+v, want empty", owner)
	}

	if err := ClaimConsoleSession(ctx, c, "s1", "kid-a", "host-x"); err != nil {
		t.Fatalf("ClaimConsoleSession: %v", err)
	}
	owner, err = ReadConsoleSessionOwner(ctx, c, "s1")
	if err != nil {
		t.Fatalf("ReadConsoleSessionOwner: %v", err)
	}
	if owner.KID != "kid-a" || owner.SID != "host-x" {
		t.Fatalf("owner = %+v, want {kid-a host-x}", owner)
	}

	if err := ReleaseConsoleSession(ctx, c, "s1"); err != nil {
		t.Fatalf("ReleaseConsoleSession: %v", err)
	}
	owner, _ = ReadConsoleSessionOwner(ctx, c, "s1")
	if owner.KID != "" {
		t.Fatalf("owner after release = %+v, want empty", owner)
	}
}

// The claim binds a session to the host it was opened against (NIM-196): a
// publisher checks a frame's authenticated SID against this, so a claim that
// lost the binding would silently disable the check.
func TestConsoleSessionClaimCarriesTheSID(t *testing.T) {
	c, _ := newClientMR(t)
	ctx := context.Background()

	// A KID is operator-set and may contain the separator; a SID is an FQDN and
	// cannot — so the split has to be anchored at the END of the value.
	if err := ClaimConsoleSession(ctx, c, "s1", "kid|weird", "db-07.example.com"); err != nil {
		t.Fatalf("ClaimConsoleSession: %v", err)
	}
	owner, err := ReadConsoleSessionOwner(ctx, c, "s1")
	if err != nil {
		t.Fatalf("ReadConsoleSessionOwner: %v", err)
	}
	if owner.KID != "kid|weird" || owner.SID != "db-07.example.com" {
		t.Fatalf("owner = %+v, want {kid|weird db-07.example.com}", owner)
	}
}

// A claim written before the SID binding existed must read back as "host
// unknown", not as a corrupt or mismatched one: mid-rolling-upgrade an old
// instance is still writing these, and reading one as a mismatch would break
// every cross-instance console until the last node restarts.
func TestConsoleSessionClaimLegacyFormatHasNoSID(t *testing.T) {
	c, _ := newClientMR(t)
	ctx := context.Background()

	if err := c.underlying().Set(ctx, ConsoleOwnerKey("s1"), "kid-a", ConsoleOwnerTTL).Err(); err != nil {
		t.Fatalf("seed legacy claim: %v", err)
	}
	owner, err := ReadConsoleSessionOwner(ctx, c, "s1")
	if err != nil {
		t.Fatalf("ReadConsoleSessionOwner: %v", err)
	}
	if owner.KID != "kid-a" {
		t.Fatalf("owner.KID = %q, want kid-a", owner.KID)
	}
	if owner.SID != "" {
		t.Fatalf("owner.SID = %q, want empty for a pre-binding claim", owner.SID)
	}
}

// The claim must expire on its own: a Keeper that dies mid-session cannot run
// its own cleanup, and a permanent key would make the id unroutable forever.
func TestConsoleSessionClaimExpires(t *testing.T) {
	c, mr := newClientMR(t)
	ctx := context.Background()

	if err := ClaimConsoleSession(ctx, c, "s1", "kid-a", "host-x"); err != nil {
		t.Fatalf("ClaimConsoleSession: %v", err)
	}
	mr.FastForward(ConsoleOwnerTTL + time.Second)

	owner, err := ReadConsoleSessionOwner(ctx, c, "s1")
	if err != nil {
		t.Fatalf("ReadConsoleSessionOwner: %v", err)
	}
	if owner.KID != "" {
		t.Fatalf("owner after TTL = %+v, want empty", owner)
	}
}

func TestConsoleClaimRejectsBadArgs(t *testing.T) {
	c, _ := newClientMR(t)
	ctx := context.Background()

	if err := ClaimConsoleSession(ctx, nil, "s", "k", "host-x"); err == nil {
		t.Fatal("nil client accepted")
	}
	if err := ClaimConsoleSession(ctx, c, "", "k", "host-x"); err == nil {
		t.Fatal("empty sessionID accepted")
	}
	if err := ClaimConsoleSession(ctx, c, "s", "", "host-x"); err == nil {
		t.Fatal("empty kid accepted")
	}
	// A claim with no SID would be indistinguishable from a pre-binding one and
	// would silently opt its session out of the publisher-side check.
	if err := ClaimConsoleSession(ctx, c, "s", "k", ""); err == nil {
		t.Fatal("empty sid accepted")
	}
	if _, err := ReadConsoleSessionOwner(ctx, c, ""); err == nil {
		t.Fatal("empty sessionID accepted by the reader")
	}
}

// The upstream half of a cross-Keeper console: the instance holding the
// EventStream publishes pty output to the instance holding the socket.
func TestPublishConsoleUpstream_DeliversToOwner(t *testing.T) {
	c, _ := newClientMR(t)
	ctx := context.Background()

	sub, err := SubscribeConsoleUpstream(ctx, c, "kid-owner", discardLog())
	if err != nil {
		t.Fatalf("SubscribeConsoleUpstream: %v", err)
	}
	defer func() { _ = sub.Close() }()
	if err := sub.Ready(ctx); err != nil {
		t.Fatalf("Ready: %v", err)
	}

	payload := []byte{0x00, 0x1b, 0xff, 'o', 'k'}
	msg := &keeperv1.FromSoul{
		Payload: &keeperv1.FromSoul_ConsoleChunk{ConsoleChunk: &keeperv1.ConsoleChunk{
			SessionId: "01JSESSION",
			Stream:    keeperv1.ConsoleStream_CONSOLE_STREAM_STDOUT,
			Data:      payload,
			Seq:       7,
		}},
	}
	n, err := PublishConsoleUpstream(ctx, c, "kid-owner", "kid-holder", msg)
	if err != nil {
		t.Fatalf("PublishConsoleUpstream: %v", err)
	}
	if n != 1 {
		t.Fatalf("subscribers = %d, want 1", n)
	}

	select {
	case got := <-sub.Channel():
		chunk := got.GetConsoleChunk()
		if chunk == nil {
			t.Fatalf("payload = %T, want a ConsoleChunk", got.GetPayload())
		}
		if chunk.GetSessionId() != "01JSESSION" {
			t.Fatalf("session_id = %q", chunk.GetSessionId())
		}
		if string(chunk.GetData()) != string(payload) {
			t.Fatalf("data = %q, want %q — raw pty bytes must survive the bridge", chunk.GetData(), payload)
		}
		if chunk.GetSeq() != 7 {
			t.Fatalf("seq = %d, want 7", chunk.GetSeq())
		}
	case <-time.After(3 * time.Second):
		t.Fatal("no frame arrived over the bridge")
	}
}

// A session can migrate back to the publishing instance between the owner
// lookup and delivery; the echo must not loop.
func TestPublishConsoleUpstream_SelfEchoIgnored(t *testing.T) {
	c, _ := newClientMR(t)
	ctx := context.Background()

	sub, err := SubscribeConsoleUpstream(ctx, c, "kid-a", discardLog())
	if err != nil {
		t.Fatalf("SubscribeConsoleUpstream: %v", err)
	}
	defer func() { _ = sub.Close() }()
	if err := sub.Ready(ctx); err != nil {
		t.Fatalf("Ready: %v", err)
	}

	// Published BY kid-a TO kid-a.
	if _, err := PublishConsoleUpstream(ctx, c, "kid-a", "kid-a", &keeperv1.FromSoul{
		Payload: &keeperv1.FromSoul_ConsoleChunk{ConsoleChunk: &keeperv1.ConsoleChunk{SessionId: "s"}},
	}); err != nil {
		t.Fatalf("PublishConsoleUpstream: %v", err)
	}
	// And a genuine one from another instance, to prove the channel still works.
	if _, err := PublishConsoleUpstream(ctx, c, "kid-a", "kid-b", &keeperv1.FromSoul{
		Payload: &keeperv1.FromSoul_ConsoleExit{ConsoleExit: &keeperv1.ConsoleExit{SessionId: "s"}},
	}); err != nil {
		t.Fatalf("PublishConsoleUpstream: %v", err)
	}

	select {
	case got := <-sub.Channel():
		if got.GetConsoleExit() == nil {
			t.Fatalf("first delivered frame is %T — the self-echo was not filtered", got.GetPayload())
		}
	case <-time.After(3 * time.Second):
		t.Fatal("no frame arrived")
	}
}

// Nobody listening is a normal outcome (the owner's socket closed); the caller
// uses the count to forget the route rather than publishing into the void.
func TestPublishConsoleUpstream_NoSubscribers(t *testing.T) {
	c, _ := newClientMR(t)
	n, err := PublishConsoleUpstream(context.Background(), c, "kid-gone", "kid-holder", &keeperv1.FromSoul{
		Payload: &keeperv1.FromSoul_ConsoleChunk{ConsoleChunk: &keeperv1.ConsoleChunk{SessionId: "s"}},
	})
	if err != nil {
		t.Fatalf("PublishConsoleUpstream: %v", err)
	}
	if n != 0 {
		t.Fatalf("subscribers = %d, want 0", n)
	}
}

func TestPublishConsoleUpstream_RejectsBadArgs(t *testing.T) {
	c, _ := newClientMR(t)
	ctx := context.Background()
	msg := &keeperv1.FromSoul{
		Payload: &keeperv1.FromSoul_ConsoleChunk{ConsoleChunk: &keeperv1.ConsoleChunk{SessionId: "s"}},
	}
	cases := []struct {
		name                string
		client              *Client
		ownerKID, originKID string
		msg                 *keeperv1.FromSoul
	}{
		{"nil client", nil, "o", "k", msg},
		{"empty ownerKID", c, "", "k", msg},
		{"empty originKID", c, "o", "", msg},
		{"nil msg", c, "o", "k", nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := PublishConsoleUpstream(ctx, tc.client, tc.ownerKID, tc.originKID, tc.msg); err == nil {
				t.Fatal("bad arguments accepted")
			}
		})
	}
}

// Close must stop the loop and the goroutine behind it; a leak here would
// accumulate one subscription per Keeper restart in tests and per process in
// production.
func TestSubscribeConsoleUpstream_CloseIsIdempotent(t *testing.T) {
	c, _ := newClientMR(t)
	ctx := context.Background()

	sub, err := SubscribeConsoleUpstream(ctx, c, "kid-a", discardLog())
	if err != nil {
		t.Fatalf("SubscribeConsoleUpstream: %v", err)
	}
	if err := sub.Ready(ctx); err != nil {
		t.Fatalf("Ready: %v", err)
	}
	if err := sub.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := sub.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}

	select {
	case _, ok := <-sub.Channel():
		if ok {
			t.Fatal("channel still delivering after Close")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("channel was not closed by Close")
	}
}

func TestSubscribeConsoleUpstream_RejectsBadArgs(t *testing.T) {
	c, _ := newClientMR(t)
	ctx := context.Background()

	if _, err := SubscribeConsoleUpstream(ctx, nil, "kid", discardLog()); err == nil {
		t.Fatal("nil client accepted")
	}
	if _, err := SubscribeConsoleUpstream(ctx, c, "", discardLog()); err == nil {
		t.Fatal("empty selfKID accepted")
	}
	if _, err := SubscribeConsoleUpstream(ctx, c, "kid", nil); err == nil {
		t.Fatal("nil logger accepted")
	}
}
