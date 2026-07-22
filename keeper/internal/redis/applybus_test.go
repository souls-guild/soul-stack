package redis

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"
)

// crockfordULIDAlphabet is the Crockford-base32 alphabet used by ULID (no I/L/O/U).
const crockfordULIDAlphabet = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"

// randULID generates a 26-character ULID-like string (same alphabet and
// length as real applyIDs). The distribution test only cares about shape and
// entropy, not time monotonicity — so this is self-contained, with no
// external dependency (avoids pulling oklog/ulid into a direct import of the
// redis module).
func randULID(t *testing.T) string {
	t.Helper()
	var raw [26]byte
	if _, err := rand.Read(raw[:]); err != nil {
		t.Fatalf("rand.Read: %v", err)
	}
	var b strings.Builder
	b.Grow(26)
	for _, c := range raw {
		b.WriteByte(crockfordULIDAlphabet[int(c)%len(crockfordULIDAlphabet)])
	}
	return b.String()
}

func TestApplyBusChannel(t *testing.T) {
	got := ApplyBusChannel("01JABC")
	want := fmt.Sprintf("events:shard:%d", ApplyBusShardIndex("01JABC"))
	if got != want {
		t.Fatalf("ApplyBusChannel = %q, want %q", got, want)
	}
	// Prefix is events (the TODO-rename apply:→events: is done), index in range.
	if !strings.HasPrefix(got, "events:shard:") {
		t.Errorf("channel %q must use events:shard: prefix", got)
	}
	if idx := ApplyBusShardIndex("01JABC"); idx >= ApplyBusShardCount {
		t.Errorf("shard index %d out of range [0,%d)", idx, ApplyBusShardCount)
	}
}

// TestApplyBusChannel_DeterministicShard — guards the determinism of shard
// resolution and its uniformity across K shards on a ULID sample. Determinism
// is critical: publisher and subscriber on different Keeper instances must
// compute the SAME shard channel for the same applyID, or cross-keeper
// delivery won't line up. Uniformity justifies choosing K=256: a skewed hot
// shard would defeat the point of sharding.
func TestApplyBusChannel_DeterministicShard(t *testing.T) {
	// (1) Determinism: one applyID → the same channel and index every time.
	const sample = "01J0DETERMINISTICSHARD000A"
	idx0 := ApplyBusShardIndex(sample)
	ch0 := ApplyBusChannel(sample)
	for i := 0; i < 1000; i++ {
		if got := ApplyBusShardIndex(sample); got != idx0 {
			t.Fatalf("ApplyBusShardIndex non-deterministic: %d != %d", got, idx0)
		}
		if got := ApplyBusChannel(sample); got != ch0 {
			t.Fatalf("ApplyBusChannel non-deterministic: %q != %q", got, ch0)
		}
	}

	// (2) All indexes are in range [0, K) and the channel matches the index.
	const n = 50000
	hits := make([]int, ApplyBusShardCount)
	for i := 0; i < n; i++ {
		id := randULID(t)
		idx := ApplyBusShardIndex(id)
		if idx >= ApplyBusShardCount {
			t.Fatalf("shard index %d out of range for %q", idx, id)
		}
		if got, want := ApplyBusChannel(id), fmt.Sprintf("events:shard:%d", idx); got != want {
			t.Fatalf("channel %q inconsistent with index %d (want %q)", got, idx, want)
		}
		hits[idx]++
	}

	// (3) Uniformity: no shard is empty or "hot". Ideal is n/K hits per
	// shard; we allow a ±60% corridor (headroom for random-sample variance,
	// no statistical χ² test, to avoid flakiness).
	ideal := float64(n) / float64(ApplyBusShardCount)
	lo, hi := ideal*0.4, ideal*1.6
	for i, h := range hits {
		if h == 0 {
			t.Errorf("shard %d got zero hits over %d samples — distribution gap", i, n)
			continue
		}
		if float64(h) < lo || float64(h) > hi {
			t.Errorf("shard %d hits = %d, outside uniform corridor [%.0f, %.0f] (ideal %.0f)",
				i, h, lo, hi, ideal)
		}
	}
}

func TestPublishApplyEvent_RejectsBadArgs(t *testing.T) {
	c, _ := newClientMR(t)
	ctx := context.Background()
	payload := json.RawMessage(`{"k":"v"}`)
	cases := []struct {
		name      string
		client    *Client
		applyID   string
		originKID string
		kind      string
	}{
		{"nil client", nil, "id", "kid", "task.executed"},
		{"empty applyID", c, "", "kid", "task.executed"},
		{"empty originKID", c, "id", "", "task.executed"},
		{"empty kind", c, "id", "kid", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := PublishApplyEvent(ctx, tc.client, tc.applyID, tc.originKID, tc.kind, time.Time{}, payload); err == nil {
				t.Errorf("expected validation error, got nil")
			}
		})
	}
}

func TestSubscribeApplyEvent_RejectsBadArgs(t *testing.T) {
	c, _ := newClientMR(t)
	ctx := context.Background()
	cases := []struct {
		name    string
		client  *Client
		applyID string
		selfKID string
		hasLog  bool
	}{
		{"nil client", nil, "id", "kid", true},
		{"empty applyID", c, "", "kid", true},
		{"empty selfKID", c, "id", "", true},
		{"nil logger", c, "id", "kid", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var lg = discardLog()
			if !tc.hasLog {
				lg = nil
			}
			if _, err := SubscribeApplyEvent(ctx, tc.client, tc.applyID, tc.selfKID, lg); err == nil {
				t.Errorf("expected validation error, got nil")
			}
		})
	}
}

// TestApplyBus_RoundTrip — full publish/subscribe cycle. The subscriber
// must receive an ApplyEvent with kind/applyID/payload reconstructed.
func TestApplyBus_RoundTrip(t *testing.T) {
	c, _ := newClientMR(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	sub, err := SubscribeApplyEvent(ctx, c, "01JABC", "keeper-receiver", discardLog())
	if err != nil {
		t.Fatalf("SubscribeApplyEvent: %v", err)
	}
	defer sub.Close()
	if err := sub.Ready(ctx); err != nil {
		t.Fatalf("Ready: %v", err)
	}

	payload := json.RawMessage(`{"sid":"host.example","task_idx":1}`)
	n, err := PublishApplyEvent(ctx, c, "01JABC", "keeper-sender", "task.executed", time.Time{}, payload)
	if err != nil {
		t.Fatalf("PublishApplyEvent: %v", err)
	}
	if n != 1 {
		t.Errorf("subscribers count = %d, want 1", n)
	}

	select {
	case got, ok := <-sub.Channel():
		if !ok {
			t.Fatal("subscription channel closed before message")
		}
		if got.Kind != "task.executed" {
			t.Errorf("kind = %q, want task.executed", got.Kind)
		}
		if got.ApplyID != "01JABC" {
			t.Errorf("apply_id = %q, want 01JABC", got.ApplyID)
		}
		if got.OriginKID != "keeper-sender" {
			t.Errorf("origin_kid = %q, want keeper-sender", got.OriginKID)
		}
		if got.At.IsZero() {
			t.Error("At is zero — publish must stamp default")
		}
		var dec map[string]any
		if err := json.Unmarshal(got.Payload, &dec); err != nil {
			t.Fatalf("payload unmarshal: %v", err)
		}
		if dec["sid"] != "host.example" {
			t.Errorf("payload.sid = %v, want host.example", dec["sid"])
		}
	case <-time.After(2 * time.Second):
		t.Fatal("did not receive message within 2s")
	}
}

// TestApplyBus_SelfFilter — publish with the same origin_kid as the
// subscriber's selfKID → the message is ignored.
func TestApplyBus_SelfFilter(t *testing.T) {
	c, _ := newClientMR(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	sub, err := SubscribeApplyEvent(ctx, c, "id", "keeper-self", discardLog())
	if err != nil {
		t.Fatalf("SubscribeApplyEvent: %v", err)
	}
	defer sub.Close()
	if err := sub.Ready(ctx); err != nil {
		t.Fatalf("Ready: %v", err)
	}

	payload := json.RawMessage(`{"x":1}`)
	// Self-origin first.
	if _, err := PublishApplyEvent(ctx, c, "id", "keeper-self", "task.executed", time.Time{}, payload); err != nil {
		t.Fatalf("PublishApplyEvent self: %v", err)
	}
	// Then other-origin — it must arrive.
	if _, err := PublishApplyEvent(ctx, c, "id", "keeper-other", "apply.completed", time.Time{}, payload); err != nil {
		t.Fatalf("PublishApplyEvent other: %v", err)
	}

	select {
	case got := <-sub.Channel():
		if got.Kind != "apply.completed" {
			t.Errorf("kind = %q, want apply.completed (other-origin)", got.Kind)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("did not receive other-origin message within 2s")
	}

	// Self must not arrive as an extra message.
	select {
	case got, ok := <-sub.Channel():
		if ok {
			t.Errorf("unexpected extra message: kind = %q", got.Kind)
		}
	case <-time.After(150 * time.Millisecond):
		// OK — self was filtered out.
	}
}

// TestApplyBus_NoSubscribers — PublishApplyEvent with no subscribers
// returns 0 with no error.
func TestApplyBus_NoSubscribers(t *testing.T) {
	c, _ := newClientMR(t)
	ctx := context.Background()

	n, err := PublishApplyEvent(ctx, c, "id", "kid", "task.executed", time.Time{}, json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("PublishApplyEvent: %v", err)
	}
	if n != 0 {
		t.Errorf("subscribers count = %d, want 0", n)
	}
}

// TestApplyBus_CloseShutsDownGoroutine — Close correctly terminates the
// goroutine and closes the out-channel.
func TestApplyBus_CloseShutsDownGoroutine(t *testing.T) {
	c, _ := newClientMR(t)
	ctx := context.Background()

	sub, err := SubscribeApplyEvent(ctx, c, "id", "kid", discardLog())
	if err != nil {
		t.Fatalf("SubscribeApplyEvent: %v", err)
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

// TestApplyBus_CloseSurvivesConcurrentReceive — race between an external
// Close and the data stream. -race must pass.
func TestApplyBus_CloseSurvivesConcurrentReceive(t *testing.T) {
	c, _ := newClientMR(t)
	ctx := context.Background()

	sub, err := SubscribeApplyEvent(ctx, c, "id", "kid", discardLog())
	if err != nil {
		t.Fatalf("SubscribeApplyEvent: %v", err)
	}
	if err := sub.Ready(ctx); err != nil {
		t.Fatalf("Ready: %v", err)
	}

	go func() {
		for i := 0; i < 5; i++ {
			_, _ = PublishApplyEvent(ctx, c, "id", "other", "task.executed", time.Time{}, json.RawMessage(`{}`))
		}
	}()

	time.Sleep(20 * time.Millisecond)
	if err := sub.Close(); err != nil && !errors.Is(err, io.EOF) {
		t.Errorf("Close: %v", err)
	}

	for range sub.Channel() {
	}
}

// TestPublishApplyEvent_StampsAtIfZero — at.IsZero gets replaced with
// time.Now().UTC().
func TestPublishApplyEvent_StampsAtIfZero(t *testing.T) {
	c, _ := newClientMR(t)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	sub, err := SubscribeApplyEvent(ctx, c, "id", "kid", discardLog())
	if err != nil {
		t.Fatalf("SubscribeApplyEvent: %v", err)
	}
	defer sub.Close()
	if err := sub.Ready(ctx); err != nil {
		t.Fatalf("Ready: %v", err)
	}

	before := time.Now().UTC()
	if _, err := PublishApplyEvent(ctx, c, "id", "other", "task.executed", time.Time{}, json.RawMessage(`{}`)); err != nil {
		t.Fatalf("PublishApplyEvent: %v", err)
	}
	select {
	case got := <-sub.Channel():
		if got.At.Before(before.Add(-time.Second)) {
			t.Errorf("stamped At=%v is before Publish call (=%v)", got.At, before)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no event")
	}
}

// TestApplyBus_ForwardBufferOverflowDropsOldest — guards the shard-side
// forward buffer (`s.out`, sized applyEventSubBufferSize). The subscriber
// does NOT drain Channel(); the shard channel is flooded with
// >applyEventSubBufferSize cross-origin messages carrying a monotonic seq in
// the payload. Invariants:
//
//	(a) the publisher (PublishApplyEvent) does NOT block on a full buffer;
//	(b) the forward-loop does not panic;
//	(c) excess events are dropped (draining yields no more than buffer events);
//	(d) drop-OLDEST: NEWEST survives — the last seq makes it through, the
//	    earliest ones are discarded (symmetric with applybus
//	    TestBufferOverflowDropsOldest).
//
// origin differs from selfKID, otherwise the self-filter would have dropped
// everything in the forward-loop already (see the doc-comment about echoing
// a node's own publishes).
func TestApplyBus_ForwardBufferOverflowDropsOldest(t *testing.T) {
	c, _ := newClientMR(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	sub, err := SubscribeApplyEvent(ctx, c, "01JOVERFLOW", "keeper-self", discardLog())
	if err != nil {
		t.Fatalf("SubscribeApplyEvent: %v", err)
	}
	defer sub.Close()
	if err := sub.Ready(ctx); err != nil {
		t.Fatalf("Ready: %v", err)
	}

	// Flood buffer + headroom, WITHOUT reading Channel(). seq grows
	// monotonically — after drop-oldest the tail (largest seq) should survive.
	total := applyEventSubBufferSize + 32
	for i := 0; i < total; i++ {
		payload := json.RawMessage(fmt.Sprintf(`{"seq":%d}`, i))
		// (a) the publisher must not block, even when the forward buffer is full.
		if _, err := PublishApplyEvent(ctx, c, "01JOVERFLOW", "keeper-other", "task.executed", time.Time{}, payload); err != nil {
			t.Fatalf("PublishApplyEvent seq=%d: %v (publisher blocked or errored on full buffer)", i, err)
		}
	}

	// Let the forward-loop drain the Redis queue into s.out (with drops).
	// Poll until it stabilizes: channel length stops growing and hits its cap.
	waitForStable(t, 3*time.Second, func() int { return len(sub.Channel()) })

	// (c)+(d) Drain without blocking. No more than buffer events; the last
	// seq is present (newest survived), and the earliest seqs are dropped.
	var got []int
drain:
	for {
		select {
		case ev, ok := <-sub.Channel():
			if !ok {
				break drain
			}
			var dec struct {
				Seq int `json:"seq"`
			}
			if err := json.Unmarshal(ev.Payload, &dec); err != nil {
				t.Fatalf("payload unmarshal: %v", err)
			}
			got = append(got, dec.Seq)
		case <-time.After(150 * time.Millisecond):
			break drain
		}
	}

	if len(got) == 0 {
		t.Fatal("no events forwarded — overflow path dropped everything")
	}
	if len(got) > applyEventSubBufferSize {
		t.Fatalf("forwarded %d events, want <= %d (buffer not bounded)", len(got), applyEventSubBufferSize)
	}
	// (d) newest survived: the last published seq made it through.
	last := got[len(got)-1]
	if last != total-1 {
		t.Errorf("newest forwarded seq = %d, want %d (drop-oldest semantics: freshest must survive)", last, total-1)
	}
	// (d) oldest were dropped: seq=0 must not survive overflow when buffer<total.
	for _, s := range got {
		if s == 0 {
			t.Errorf("oldest seq=0 survived overflow — drop-newest leaked instead of drop-oldest")
			break
		}
	}
	// (b) the subscription is still alive (forward-loop didn't panic): a new publish gets through.
	if _, err := PublishApplyEvent(ctx, c, "01JOVERFLOW", "keeper-other", "apply.completed", time.Time{}, json.RawMessage(`{"seq":-1}`)); err != nil {
		t.Fatalf("post-overflow PublishApplyEvent: %v", err)
	}
	select {
	case ev, ok := <-sub.Channel():
		if !ok {
			t.Fatal("Channel closed after overflow — forward-loop died")
		}
		if ev.Kind != "apply.completed" {
			t.Errorf("post-overflow event kind = %q, want apply.completed", ev.Kind)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("forward-loop stopped delivering after overflow (likely panicked)")
	}
}

// waitForStable polls size() until the value stops growing between two
// consecutive reads (or times out). For async Redis pub/sub: we wait for the
// forward-loop to reach its bounded limit on a full buffer.
func waitForStable(t *testing.T, timeout time.Duration, size func() int) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	prev := -1
	stable := 0
	for time.Now().Before(deadline) {
		cur := size()
		if cur == prev {
			stable++
			if stable >= 3 {
				return
			}
		} else {
			stable = 0
		}
		prev = cur
		time.Sleep(20 * time.Millisecond)
	}
}

// TestPublishApplyEvent_PreservesNonZeroAt — a passed-in at is preserved.
func TestPublishApplyEvent_PreservesNonZeroAt(t *testing.T) {
	c, _ := newClientMR(t)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	sub, err := SubscribeApplyEvent(ctx, c, "id", "kid", discardLog())
	if err != nil {
		t.Fatalf("SubscribeApplyEvent: %v", err)
	}
	defer sub.Close()
	if err := sub.Ready(ctx); err != nil {
		t.Fatalf("Ready: %v", err)
	}

	fixed := time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC)
	if _, err := PublishApplyEvent(ctx, c, "id", "other", "apply.completed", fixed, json.RawMessage(`{}`)); err != nil {
		t.Fatalf("PublishApplyEvent: %v", err)
	}
	select {
	case got := <-sub.Channel():
		if !got.At.Equal(fixed) {
			t.Errorf("At = %v, want preserved %v", got.At, fixed)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no event")
	}
}
