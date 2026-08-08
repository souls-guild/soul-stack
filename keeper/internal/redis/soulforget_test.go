package redis

import (
	"context"
	"log/slog"
	"testing"
	"time"

	keeperv1 "github.com/souls-guild/soul-stack/proto/gen/go/keeper/v1"
)

// The guards below all answer one question: does forgetting a host RELEASE what
// the host held, or does it only delete a row somewhere else and leave the
// Redis half behind? A forget that reports success while `soul:<sid>:hb`
// survives is the exact failure this operation exists to prevent, so it has to
// go red here rather than be noticed months later as unexplained key growth.

// TestHeartbeatKey_HasNoTTL_SoOnlyAnExplicitPurgeCollectsIt is the premise the
// rest of the file rests on, pinned against the REAL writer rather than assumed.
//
// [TouchHeartbeat] deliberately sets no expiry (see heartbeat.go — the record's
// lifetime is an explicit DEL by the Reaper). That is fine while a host exists
// and fatal once it does not: the Reaper rules key off `souls` rows, so for a
// host whose row is gone nothing is ever going to come back for this key. If a
// TTL is ever added, this test fails and [PurgeSoulKeys] stops being the only
// thing standing between a forget and a permanent leak — which is a fact worth
// re-reading the purge path over, not a line to silently update.
func TestHeartbeatKey_HasNoTTL_SoOnlyAnExplicitPurgeCollectsIt(t *testing.T) {
	c, mr := newClientMR(t)
	ctx := context.Background()
	const sid = "host1.example.com"

	if err := TouchHeartbeat(ctx, c, sid, "kid-1", time.Now()); err != nil {
		t.Fatalf("TouchHeartbeat: %v", err)
	}
	if ttl := mr.TTL(HeartbeatKey(sid)); ttl != 0 {
		t.Fatalf("heartbeat key TTL = %v, want 0 (no expiry). "+
			"If an expiry was added on purpose, re-read PurgeSoulKeys and this file: "+
			"the leak argument for the heartbeat hash no longer holds.", ttl)
	}
}

// TestPurgeSoulKeys_ReleasesTheHeartbeatAndUtilizationKeys — the release half of
// `soul.forget`, written against the production key constructors so a renamed
// key cannot leave the purge pointing at a name nothing writes any more.
func TestPurgeSoulKeys_ReleasesTheHeartbeatAndUtilizationKeys(t *testing.T) {
	c, mr := newClientMR(t)
	ctx := context.Background()
	const sid = "host1.example.com"

	if err := TouchHeartbeat(ctx, c, sid, "kid-1", time.Now()); err != nil {
		t.Fatalf("TouchHeartbeat: %v", err)
	}
	ev := &keeperv1.HostUtilization{CpuPct: 12.5, Load1: 0.4, MemUsedMb: 512, MemTotalMb: 2048}
	if err := WriteUtilization(ctx, c, sid, ev, time.Now()); err != nil {
		t.Fatalf("WriteUtilization: %v", err)
	}

	for _, k := range []string{HeartbeatKey(sid), UtilizationKey(sid), UtilizationWindowKey(sid)} {
		if !mr.Exists(k) {
			t.Fatalf("precondition: key %q was not written, the test would prove nothing", k)
		}
	}

	n, err := PurgeSoulKeys(ctx, c, sid)
	if err != nil {
		t.Fatalf("PurgeSoulKeys: %v", err)
	}
	if n != 3 {
		t.Errorf("purged = %d, want 3", n)
	}
	for _, k := range []string{HeartbeatKey(sid), UtilizationKey(sid), UtilizationWindowKey(sid)} {
		if mr.Exists(k) {
			t.Errorf("key %q survived the purge — the host's row is gone, so nothing will ever collect it", k)
		}
	}
}

// TestPurgeSoulKeys_LeavesTheSIDLeaseToItsOwner — the deliberate exception.
//
// The lease is not this call's to release: it is held and renewed by whichever
// instance owns the stream, so deleting it from outside would either be undone
// by the next renewal or let a second instance take a lease the first still
// believes it holds. The stream teardown releases it. Reading a shrinking key
// count as "the purge got weaker" is the mistake this test forestalls.
func TestPurgeSoulKeys_LeavesTheSIDLeaseToItsOwner(t *testing.T) {
	c, mr := newClientMR(t)
	ctx := context.Background()
	const sid = "host1.example.com"

	if _, err := AcquireSoulLease(ctx, c, sid, "kid-1", 30*time.Second); err != nil {
		t.Fatalf("AcquireSoulLease: %v", err)
	}
	if _, err := PurgeSoulKeys(ctx, c, sid); err != nil {
		t.Fatalf("PurgeSoulKeys: %v", err)
	}
	if !mr.Exists(SoulLeaseKey(sid)) {
		t.Errorf("the SID lease was deleted by the purge; it belongs to the instance holding "+
			"the stream and is released by closing it, not from the outside (key %q)", SoulLeaseKey(sid))
	}
}

// TestPurgeSoulKeys_PurgesOnlyTheNamedHost — a forget is scoped to one host, and
// the key namespace `soul:<sid>:*` is prefix-shaped, so a purge written with a
// pattern DEL would take neighbours with it.
func TestPurgeSoulKeys_PurgesOnlyTheNamedHost(t *testing.T) {
	c, mr := newClientMR(t)
	ctx := context.Background()
	const victim = "host1.example.com"
	const bystander = "host1.example.com.internal" // shares the victim's prefix

	for _, sid := range []string{victim, bystander} {
		if err := TouchHeartbeat(ctx, c, sid, "kid-1", time.Now()); err != nil {
			t.Fatalf("TouchHeartbeat(%s): %v", sid, err)
		}
	}
	if _, err := PurgeSoulKeys(ctx, c, victim); err != nil {
		t.Fatalf("PurgeSoulKeys: %v", err)
	}
	if !mr.Exists(HeartbeatKey(bystander)) {
		t.Errorf("purging %q also removed %q — a forget must not reach a host it did not name", victim, bystander)
	}
}

// TestPublishSoulForget_DeliversToOtherInstancesAndSkipsTheOrigin — the
// cross-instance half. The origin closes its own stream synchronously inside
// soulforget.Erase, so hearing its own echo would be at best redundant and at
// worst (NIM-421) the thing it waits for instead of acting.
func TestPublishSoulForget_DeliversToOtherInstancesAndSkipsTheOrigin(t *testing.T) {
	c, _ := newClientMR(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	peer, err := SubscribeSoulForget(ctx, c, "kid-peer", slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatalf("SubscribeSoulForget(peer): %v", err)
	}
	defer func() { _ = peer.Close() }()
	origin, err := SubscribeSoulForget(ctx, c, "kid-origin", slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatalf("SubscribeSoulForget(origin): %v", err)
	}
	defer func() { _ = origin.Close() }()

	if err := peer.Ready(ctx); err != nil {
		t.Fatalf("peer.Ready: %v", err)
	}
	if err := origin.Ready(ctx); err != nil {
		t.Fatalf("origin.Ready: %v", err)
	}

	if _, err := PublishSoulForget(ctx, c, "host1.example.com", "kid-origin"); err != nil {
		t.Fatalf("PublishSoulForget: %v", err)
	}

	select {
	case ev := <-peer.Channel():
		if ev == nil || ev.SID != "host1.example.com" {
			t.Fatalf("peer got %+v, want a notice for host1.example.com", ev)
		}
		if ev.OriginKID != "kid-origin" {
			t.Errorf("origin_kid = %q, want kid-origin", ev.OriginKID)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("the other instance never heard the forget notice — its stream to the " +
			"forgotten host would stay open with nothing to close it")
	}

	select {
	case ev := <-origin.Channel():
		t.Errorf("the publishing instance received its own notice (%+v); it acts locally in "+
			"soulforget.Erase and must not learn about its own action from the wire", ev)
	case <-time.After(200 * time.Millisecond):
	}
}

// TestPublishSoulForget_ZeroSubscribersIsNotAnError — a single-instance cluster
// has nobody to tell. Erase treats a publish ERROR as a hard stop before the
// delete, so returning one here would make the operation unusable exactly where
// it is safest.
func TestPublishSoulForget_ZeroSubscribersIsNotAnError(t *testing.T) {
	c, _ := newClientMR(t)
	n, err := PublishSoulForget(context.Background(), c, "host1.example.com", "kid-1")
	if err != nil {
		t.Fatalf("PublishSoulForget with no subscribers: %v", err)
	}
	if n != 0 {
		t.Errorf("subscribers = %d, want 0", n)
	}
}
