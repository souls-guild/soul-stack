package redis

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
)

const (
	testKey     = "reaper:leader"
	testHolderA = "keeper-test-a"
	testHolderB = "keeper-test-b"
)

// newClientMR is a helper that spins up a miniredis instance and wraps it in
// a [Client]. miniredis.RunT registers cleanup automatically.
func newClientMR(t *testing.T) (*Client, *miniredis.Miniredis) {
	t.Helper()
	mr := miniredis.RunT(t)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	c, err := NewClient(ctx, Config{Addr: mr.Addr()}, nil)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c, mr
}

func TestAcquire_NewKey(t *testing.T) {
	c, mr := newClientMR(t)
	ctx := context.Background()

	l, err := Acquire(ctx, c, testKey, testHolderA, 5*time.Second)
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	if l.Key() != testKey || l.Holder() != testHolderA {
		t.Errorf("lease metadata mismatch: key=%q holder=%q", l.Key(), l.Holder())
	}

	if v, _ := mr.Get(testKey); v != testHolderA {
		t.Errorf("redis stored value = %q, want %q", v, testHolderA)
	}
}

func TestAcquire_Conflict(t *testing.T) {
	c, _ := newClientMR(t)
	ctx := context.Background()

	if _, err := Acquire(ctx, c, testKey, testHolderA, 5*time.Second); err != nil {
		t.Fatalf("first Acquire: %v", err)
	}

	_, err := Acquire(ctx, c, testKey, testHolderB, 5*time.Second)
	if !errors.Is(err, ErrLeaseTaken) {
		t.Fatalf("second Acquire err = %v, want ErrLeaseTaken", err)
	}
}

func TestAcquire_TTLExpiresAllowsReacquire(t *testing.T) {
	c, mr := newClientMR(t)
	ctx := context.Background()

	if _, err := Acquire(ctx, c, testKey, testHolderA, 100*time.Millisecond); err != nil {
		t.Fatalf("Acquire A: %v", err)
	}
	mr.FastForward(200 * time.Millisecond)

	l, err := Acquire(ctx, c, testKey, testHolderB, 5*time.Second)
	if err != nil {
		t.Fatalf("Acquire B after TTL: %v", err)
	}
	if l.Holder() != testHolderB {
		t.Errorf("holder after reacquire = %q, want %q", l.Holder(), testHolderB)
	}
}

func TestAcquire_RejectsInvalidArgs(t *testing.T) {
	c, _ := newClientMR(t)
	ctx := context.Background()

	cases := []struct {
		name   string
		key    string
		holder string
		ttl    time.Duration
	}{
		{"empty_key", "", testHolderA, time.Second},
		{"empty_holder", testKey, "", time.Second},
		{"zero_ttl", testKey, testHolderA, 0},
		{"negative_ttl", testKey, testHolderA, -time.Second},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := Acquire(ctx, c, tc.key, tc.holder, tc.ttl); err == nil {
				t.Errorf("Acquire(%v) returned nil err; want validation error", tc)
			}
		})
	}

	if _, err := Acquire(ctx, nil, testKey, testHolderA, time.Second); err == nil {
		t.Error("Acquire(nil client) returned nil err; want validation error")
	}
}

func TestRenew_HappyPath(t *testing.T) {
	c, mr := newClientMR(t)
	ctx := context.Background()

	l, err := Acquire(ctx, c, testKey, testHolderA, 500*time.Millisecond)
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}

	// Advance time by 300 ms (lease still alive), Renew extends it.
	mr.FastForward(300 * time.Millisecond)
	if err := l.Renew(ctx); err != nil {
		t.Fatalf("Renew: %v", err)
	}

	// Another 300 ms — without Renew the lease would have died
	// (300+300=600 > 500), but Renew reset the TTL → the value is still there.
	mr.FastForward(300 * time.Millisecond)
	if v, _ := mr.Get(testKey); v != testHolderA {
		t.Errorf("value after Renew+FastForward = %q, want %q (Renew must extend TTL)", v, testHolderA)
	}
}

func TestRenew_HolderChanged(t *testing.T) {
	c, mr := newClientMR(t)
	ctx := context.Background()

	l, err := Acquire(ctx, c, testKey, testHolderA, 5*time.Second)
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}

	// Simulate "we got preempted": overwrite the value under a different
	// holder without going through [Lease].
	mr.Set(testKey, testHolderB)

	err = l.Renew(ctx)
	if !errors.Is(err, ErrLeaseLost) {
		t.Fatalf("Renew err = %v, want ErrLeaseLost", err)
	}
}

func TestRenew_KeyExpired(t *testing.T) {
	c, mr := newClientMR(t)
	ctx := context.Background()

	l, err := Acquire(ctx, c, testKey, testHolderA, 100*time.Millisecond)
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	mr.FastForward(200 * time.Millisecond)

	// The key has expired, GET returns nil → CAS won't match.
	if err := l.Renew(ctx); !errors.Is(err, ErrLeaseLost) {
		t.Fatalf("Renew after expiry err = %v, want ErrLeaseLost", err)
	}
}

func TestRelease_HappyPath(t *testing.T) {
	c, mr := newClientMR(t)
	ctx := context.Background()

	l, err := Acquire(ctx, c, testKey, testHolderA, 5*time.Second)
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	if err := l.Release(ctx); err != nil {
		t.Fatalf("Release: %v", err)
	}

	if mr.Exists(testKey) {
		t.Errorf("key %q must be deleted after Release", testKey)
	}
}

func TestRelease_HolderChanged_NoOp(t *testing.T) {
	c, mr := newClientMR(t)
	ctx := context.Background()

	l, err := Acquire(ctx, c, testKey, testHolderA, 5*time.Second)
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	mr.Set(testKey, testHolderB)

	// Release must not delete a foreign key — but also doesn't return a
	// CAS-failure error (idempotent stop).
	if err := l.Release(ctx); err != nil {
		t.Fatalf("Release on foreign holder: %v", err)
	}
	if !mr.Exists(testKey) {
		t.Errorf("key %q was deleted under foreign holder — Release must be CAS", testKey)
	}
	if v, _ := mr.Get(testKey); v != testHolderB {
		t.Errorf("key value = %q, want untouched %q", v, testHolderB)
	}
}

func TestRelease_KeyAlreadyGone_NoOp(t *testing.T) {
	c, mr := newClientMR(t)
	ctx := context.Background()

	l, err := Acquire(ctx, c, testKey, testHolderA, 5*time.Second)
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	mr.Del(testKey)

	if err := l.Release(ctx); err != nil {
		t.Fatalf("Release on missing key: %v", err)
	}
}

// TestRenew_AfterRelease — after Release, a repeat Renew returns
// ErrLeaseLost (Redis-state-driven, no flag in the Go struct).
func TestRenew_AfterRelease(t *testing.T) {
	c, _ := newClientMR(t)
	ctx := context.Background()

	l, err := Acquire(ctx, c, testKey, testHolderA, 5*time.Second)
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	if err := l.Release(ctx); err != nil {
		t.Fatalf("Release: %v", err)
	}
	if err := l.Renew(ctx); !errors.Is(err, ErrLeaseLost) {
		t.Fatalf("Renew after Release err = %v, want ErrLeaseLost", err)
	}
}
