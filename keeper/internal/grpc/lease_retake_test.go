package grpc

import (
	"context"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"

	keeperredis "github.com/souls-guild/soul-stack/keeper/internal/redis"
)

// ★ GUARD (NIM-949). Presence is the SID lease, so a live stream whose lease key
// vanished — Redis restarted without persistence, or failed over — takes it
// again, or a linear step's detector fails the live host after soul_timeout. A
// key another Keeper holds is left to it.
//
// Mutations: return on ErrLeaseLost without retaking → "key gone" never sees the
// key back → red; retake with a force/overwrite → "taken by another" loses the
// other Keeper's value → red.
func TestRenewLeaseLoop_RetakesAVanishedLease(t *testing.T) {
	const sid = "host.example.com"
	for _, tc := range []struct {
		name  string
		after func(mr *miniredis.Miniredis)
		want  string
	}{
		{"key gone", func(mr *miniredis.Miniredis) { mr.Del(keeperredis.SoulLeaseKey(sid)) }, "kid-self"},
		{"taken by another", func(mr *miniredis.Miniredis) {
			if err := mr.Set(keeperredis.SoulLeaseKey(sid), "kid-other"); err != nil {
				t.Fatalf("Set: %v", err)
			}
		}, "kid-other"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mr := miniredis.RunT(t)
			ctx := context.Background()
			rc, err := keeperredis.NewClient(ctx, keeperredis.Config{Addr: mr.Addr()}, nil)
			if err != nil {
				t.Fatalf("redis NewClient: %v", err)
			}
			t.Cleanup(func() { _ = rc.Close() })
			h := newEventStreamHandler(EventStreamDeps{
				SeedDB:       &fakeSeedDB{},
				Redis:        rc,
				AuditWriter:  &captureAudit{},
				KID:          "kid-self",
				SoulLeaseTTL: 300 * time.Millisecond,
			}, discardLogger(t))

			cleanup, err := h.acquireSoulLease(ctx, sid)
			if err != nil {
				t.Fatalf("acquireSoulLease: %v", err)
			}
			tc.after(mr)

			deadline := time.Now().Add(3 * time.Second)
			var owner string
			for time.Now().Before(deadline) {
				owner, _, _ = keeperredis.SoulLeaseOwner(ctx, rc, sid)
				if owner == tc.want && tc.want == "kid-self" {
					break
				}
				time.Sleep(50 * time.Millisecond)
			}
			cleanup()
			if owner != tc.want {
				t.Errorf("lease owner after the renew ticks = %q, want %q", owner, tc.want)
			}
		})
	}
}
