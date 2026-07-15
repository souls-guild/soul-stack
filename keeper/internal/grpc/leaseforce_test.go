package grpc

// Guard tests for presence-gated force-release of a SID lease (ADR-027
// amend (n), S2).
//
// Cover the [eventStreamHandler.acquireSoulLease] branch on ErrLeaseTaken:
// re-acquiring the lease from a PROVABLY-DEAD prev-holder (force-release)
// versus preserving AlreadyExists (split-brain guard / fail-safe). This is
// a security-sensitive ownership-takeover operation — every invariant is
// pinned by a test that catches regressions.
//
// Level — unit (in-process miniredis, no PG): acquireSoulLease depends only
// on Redis (lease + Conclave presence) and AuditWriter.

import (
	"context"
	"errors"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	keeperredis "github.com/souls-guild/soul-stack/keeper/internal/redis"
	"github.com/souls-guild/soul-stack/shared/audit"
)

// newForceLeaseHandler assembles a handler with a miniredis-backed Redis, a
// given KID, and a capture-audit writer — shared boilerplate for all
// force-release tests.
func newForceLeaseHandler(t *testing.T, kid string) (*eventStreamHandler, *keeperredis.Client, *captureAudit) {
	t.Helper()
	rc := newClusterRedis(t)
	ca := &captureAudit{}
	h := newEventStreamHandler(EventStreamDeps{
		SeedDB:       &fakeSeedDB{},
		Redis:        rc,
		AuditWriter:  ca,
		KID:          kid,
		SoulLeaseTTL: 5 * time.Second,
	}, discardLogger(t))
	return h, rc, ca
}

// markInstanceAlive registers a Conclave presence record for the KID (a
// live keeper instance), so that InstanceAlive(kid) returns true.
func markInstanceAlive(t *testing.T, ctx context.Context, rc *keeperredis.Client, kid string) {
	t.Helper()
	if err := keeperredis.RegisterInstance(ctx, rc, kid, kid, 30*time.Second, false); err != nil {
		t.Fatalf("RegisterInstance(%s): %v", kid, err)
	}
}

// TestAcquireSoulLease_DeadPrevHolder_ForceReleases — prevKID is dead in
// Conclave (no presence key) → force-release: the lease is re-acquired for
// our own KID, cleanup is non-nil, no error (the stream lives), and audit
// `eventstream.lease_force_released` is emitted.
func TestAcquireSoulLease_DeadPrevHolder_ForceReleases(t *testing.T) {
	h, rc, ca := newForceLeaseHandler(t, "kid-self")
	ctx := context.Background()
	sid := "host.example.com"

	// The dead prev-holder still holds the lease (TTL hasn't expired since
	// the crash), but it has NO presence record in Conclave → provably
	// dead.
	if _, err := keeperredis.AcquireSoulLease(ctx, rc, sid, "kid-dead", 60*time.Second); err != nil {
		t.Fatalf("seed lease: %v", err)
	}

	cleanup, err := h.acquireSoulLease(ctx, sid)
	if err != nil {
		t.Fatalf("acquireSoulLease: err = %v, want nil (force-release должен был перехватить)", err)
	}
	if cleanup == nil {
		t.Fatal("cleanup = nil on successful force-release")
	}
	defer cleanup()

	if owner, _, _ := keeperredis.SoulLeaseOwner(ctx, rc, sid); owner != "kid-self" {
		t.Errorf("lease owner = %q, want kid-self (перезахвачен)", owner)
	}

	evs := ca.snapshot()
	if len(evs) != 1 {
		t.Fatalf("audit count = %d, want 1 (eventstream.lease_force_released)", len(evs))
	}
	ev := evs[0]
	if ev.EventType != audit.EventLeaseForceReleased {
		t.Errorf("audit event_type = %q, want %q", ev.EventType, audit.EventLeaseForceReleased)
	}
	if ev.Source != audit.SourceSoulGRPC {
		t.Errorf("audit source = %q, want %q", ev.Source, audit.SourceSoulGRPC)
	}
	if ev.Payload["sid"] != sid {
		t.Errorf("audit payload.sid = %v, want %q", ev.Payload["sid"], sid)
	}
	if ev.Payload["prev_kid"] != "kid-dead" {
		t.Errorf("audit payload.prev_kid = %v, want kid-dead", ev.Payload["prev_kid"])
	}
	if ev.Payload["new_kid"] != "kid-self" {
		t.Errorf("audit payload.new_kid = %v, want kid-self", ev.Payload["new_kid"])
	}
}

// TestAcquireSoulLease_LivePrevHolder_NoForce — prevKID is ALIVE in
// Conclave (presence key exists) → NOT forced: AlreadyExists, the lease is
// untouched, audit is empty. Split-brain protection: we don't hijack a
// live holder (or a partition with a live Conclave).
func TestAcquireSoulLease_LivePrevHolder_NoForce(t *testing.T) {
	h, rc, ca := newForceLeaseHandler(t, "kid-self")
	ctx := context.Background()
	sid := "host.example.com"

	if _, err := keeperredis.AcquireSoulLease(ctx, rc, sid, "kid-live", 60*time.Second); err != nil {
		t.Fatalf("seed lease: %v", err)
	}
	markInstanceAlive(t, ctx, rc, "kid-live")

	cleanup, err := h.acquireSoulLease(ctx, sid)
	if cleanup != nil {
		t.Error("cleanup != nil, want nil (lease не захвачен)")
	}
	if got := status.Code(err); got != codes.AlreadyExists {
		t.Fatalf("code = %v, want AlreadyExists (split-brain guard)", got)
	}
	if owner, _, _ := keeperredis.SoulLeaseOwner(ctx, rc, sid); owner != "kid-live" {
		t.Errorf("lease owner = %q, want kid-live (не перезахвачен)", owner)
	}
	if n := len(ca.snapshot()); n != 0 {
		t.Errorf("audit count = %d, want 0 (force не происходил)", n)
	}
}

// TestAcquireSoulLease_PresenceCheckError_FailSafeNoForce — InstanceAlive
// returned an ERROR (Redis flapped on the presence check) → fail-safe: do
// NOT declare it dead, do NOT force → AlreadyExists. Under uncertainty the
// lease is not hijacked.
func TestAcquireSoulLease_PresenceCheckError_FailSafeNoForce(t *testing.T) {
	h, rc, ca := newForceLeaseHandler(t, "kid-self")
	ctx := context.Background()
	sid := "host.example.com"

	if _, err := keeperredis.AcquireSoulLease(ctx, rc, sid, "kid-unknown", 60*time.Second); err != nil {
		t.Fatalf("seed lease: %v", err)
	}
	// The presence check fails: swap the seam for an error (emulating a
	// Redis flap specifically on the conclave key's EXISTS, without
	// tearing down all of miniredis).
	h.instanceAlive = func(context.Context, *keeperredis.Client, string) (bool, error) {
		return false, errors.New("redis flap on EXISTS")
	}

	cleanup, err := h.acquireSoulLease(ctx, sid)
	if cleanup != nil {
		t.Error("cleanup != nil, want nil (fail-safe: lease не захвачен)")
	}
	if got := status.Code(err); got != codes.AlreadyExists {
		t.Fatalf("code = %v, want AlreadyExists (fail-safe)", got)
	}
	if owner, _, _ := keeperredis.SoulLeaseOwner(ctx, rc, sid); owner != "kid-unknown" {
		t.Errorf("lease owner = %q, want kid-unknown (не перезахвачен)", owner)
	}
	if n := len(ca.snapshot()); n != 0 {
		t.Errorf("audit count = %d, want 0 (force не происходил)", n)
	}
}

// TestAcquireSoulLease_PrevHolderIsSelf_NoForce — prevKID == our own KID
// (reconnect to the same keeper / our own lease) → NOT forced, current
// behavior is AlreadyExists. Protects against falsely hijacking our own
// lease.
func TestAcquireSoulLease_PrevHolderIsSelf_NoForce(t *testing.T) {
	h, rc, ca := newForceLeaseHandler(t, "kid-self")
	ctx := context.Background()
	sid := "host.example.com"

	// The lease is already held by THIS SAME keeper instance (kid-self).
	if _, err := keeperredis.AcquireSoulLease(ctx, rc, sid, "kid-self", 60*time.Second); err != nil {
		t.Fatalf("seed lease: %v", err)
	}
	// We deliberately don't register Conclave presence for self: the self
	// branch must trigger BEFORE the presence check (otherwise self would
	// falsely look "dead" → force our own lease).

	cleanup, err := h.acquireSoulLease(ctx, sid)
	if cleanup != nil {
		t.Error("cleanup != nil, want nil")
	}
	if got := status.Code(err); got != codes.AlreadyExists {
		t.Fatalf("code = %v, want AlreadyExists (self не перехватывается)", got)
	}
	if owner, _, _ := keeperredis.SoulLeaseOwner(ctx, rc, sid); owner != "kid-self" {
		t.Errorf("lease owner = %q, want kid-self (не тронут)", owner)
	}
	if n := len(ca.snapshot()); n != 0 {
		t.Errorf("audit count = %d, want 0 (self-reconnect, force не нужен)", n)
	}
}

// TestAcquireSoulLease_ForceRace_KeyChanged_FallbackNoHijack — race: prevKID
// is provably dead, but between the presence check and the force-release
// the key changed to a third, LIVE owner (TTL expired / another keeper won
// the race). The CAS-by-prev-holder in ForceAcquireSoulLease returns
// ErrLeaseTaken → correct fallback to AlreadyExists, without hijacking
// someone else's fresh lease and without an infinite loop.
func TestAcquireSoulLease_ForceRace_KeyChanged_FallbackNoHijack(t *testing.T) {
	h, rc, ca := newForceLeaseHandler(t, "kid-self")
	ctx := context.Background()
	sid := "host.example.com"

	// prevKID is dead in Conclave (we don't register presence) — but by
	// the time force-release runs, the key already belongs to kid-fresh
	// (race emulation: the lease was overwritten after SoulLeaseOwner
	// returned prevKID). To make SoulLeaseOwner inside the handler return
	// exactly kid-dead while the CAS sees kid-fresh, we swap the owner
	// seam for kid-dead and set the real key to kid-fresh.
	if _, err := keeperredis.AcquireSoulLease(ctx, rc, sid, "kid-fresh", 60*time.Second); err != nil {
		t.Fatalf("seed lease: %v", err)
	}
	h.soulLeaseOwner = func(context.Context, *keeperredis.Client, string) (string, bool, error) {
		return "kid-dead", true, nil
	}
	// kid-dead is provably dead (no presence); kid-fresh is alive — but the
	// force CAS compares the key against prevKID(kid-dead), which won't
	// match kid-fresh → ErrLeaseTaken.

	cleanup, err := h.acquireSoulLease(ctx, sid)
	if cleanup != nil {
		t.Error("cleanup != nil, want nil (force провалился по гонке)")
	}
	if got := status.Code(err); got != codes.AlreadyExists {
		t.Fatalf("code = %v, want AlreadyExists (fallback после гонки)", got)
	}
	if owner, _, _ := keeperredis.SoulLeaseOwner(ctx, rc, sid); owner != "kid-fresh" {
		t.Errorf("lease owner = %q, want kid-fresh (чужой свежий lease НЕ перехвачен)", owner)
	}
	if n := len(ca.snapshot()); n != 0 {
		t.Errorf("audit count = %d, want 0 (force не удался)", n)
	}
}

// TestAcquireSoulLease_NoConflict_HappyPath — no contender: the lease is
// free → a regular acquire, with no presence check and no audit (control
// check that the new logic doesn't break the normal path).
func TestAcquireSoulLease_NoConflict_HappyPath(t *testing.T) {
	h, rc, ca := newForceLeaseHandler(t, "kid-self")
	ctx := context.Background()
	sid := "host.example.com"

	cleanup, err := h.acquireSoulLease(ctx, sid)
	if err != nil {
		t.Fatalf("acquireSoulLease: %v", err)
	}
	if cleanup == nil {
		t.Fatal("cleanup = nil on happy path")
	}
	defer cleanup()

	if owner, _, _ := keeperredis.SoulLeaseOwner(ctx, rc, sid); owner != "kid-self" {
		t.Errorf("lease owner = %q, want kid-self", owner)
	}
	if n := len(ca.snapshot()); n != 0 {
		t.Errorf("audit count = %d, want 0 (нет force на свободном lease)", n)
	}
}
