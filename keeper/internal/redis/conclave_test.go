package redis

import (
	"context"
	"errors"
	"sort"
	"testing"
	"time"
)

const (
	testKIDa = "keeper-eu-west-01"
	testKIDb = "keeper-eu-west-02"
	testKIDc = "keeper-us-east-01"
)

func TestRegisterInstance_CreatesKeyWithTTL(t *testing.T) {
	c, mr := newClientMR(t)
	ctx := context.Background()

	if err := RegisterInstance(ctx, c, testKIDa, "meta-a", 30*time.Second, false); err != nil {
		t.Fatalf("RegisterInstance: %v", err)
	}

	key := ConclaveKey(testKIDa)
	if v, _ := mr.Get(key); v != "meta-a" {
		t.Errorf("stored value = %q, want %q", v, "meta-a")
	}
	if ttl := mr.TTL(key); ttl <= 0 {
		t.Errorf("key %q has no TTL set (got %v)", key, ttl)
	}
}

func TestRegisterInstance_TTLExpiryRemovesKey(t *testing.T) {
	c, mr := newClientMR(t)
	ctx := context.Background()

	if err := RegisterInstance(ctx, c, testKIDa, "meta-a", 100*time.Millisecond, false); err != nil {
		t.Fatalf("RegisterInstance: %v", err)
	}
	mr.FastForward(200 * time.Millisecond)

	if mr.Exists(ConclaveKey(testKIDa)) {
		t.Errorf("key must be gone after TTL expiry")
	}
}

func TestRegisterInstance_RequireUnique_Collision(t *testing.T) {
	c, _ := newClientMR(t)
	ctx := context.Background()

	if err := RegisterInstance(ctx, c, testKIDa, "first", 30*time.Second, true); err != nil {
		t.Fatalf("first RegisterInstance: %v", err)
	}
	// A second process with the same KID under requireUnique=true → collision.
	err := RegisterInstance(ctx, c, testKIDa, "second", 30*time.Second, true)
	if !errors.Is(err, ErrConclaveKIDTaken) {
		t.Fatalf("second RegisterInstance err = %v, want ErrConclaveKIDTaken", err)
	}
}

func TestRegisterInstance_NonUnique_Overwrites(t *testing.T) {
	c, mr := newClientMR(t)
	ctx := context.Background()

	if err := RegisterInstance(ctx, c, testKIDa, "stale", 30*time.Second, true); err != nil {
		t.Fatalf("first RegisterInstance: %v", err)
	}
	// Restart of the same KID: requireUnique=false must unconditionally
	// overwrite its own leftover (its own stale TTL key hasn't expired yet).
	if err := RegisterInstance(ctx, c, testKIDa, "fresh", 30*time.Second, false); err != nil {
		t.Fatalf("re-RegisterInstance: %v", err)
	}
	if v, _ := mr.Get(ConclaveKey(testKIDa)); v != "fresh" {
		t.Errorf("value after re-register = %q, want %q", v, "fresh")
	}
}

func TestRegisterInstance_RejectsInvalidArgs(t *testing.T) {
	c, _ := newClientMR(t)
	ctx := context.Background()

	if err := RegisterInstance(ctx, c, "", "m", time.Second, false); err == nil {
		t.Error("empty kid: want error")
	}
	if err := RegisterInstance(ctx, c, testKIDa, "m", 0, false); err == nil {
		t.Error("zero ttl: want error")
	}
	if err := RegisterInstance(ctx, nil, testKIDa, "m", time.Second, false); err == nil {
		t.Error("nil client: want error")
	}
}

func TestRenewInstance_ExtendsTTL(t *testing.T) {
	c, mr := newClientMR(t)
	ctx := context.Background()

	if err := RegisterInstance(ctx, c, testKIDa, "meta-a", 500*time.Millisecond, false); err != nil {
		t.Fatalf("RegisterInstance: %v", err)
	}

	mr.FastForward(300 * time.Millisecond)
	ok, err := RenewInstance(ctx, c, testKIDa, 500*time.Millisecond)
	if err != nil {
		t.Fatalf("RenewInstance: %v", err)
	}
	if !ok {
		t.Fatal("RenewInstance ok=false, want true (key still alive)")
	}

	// Another 300ms: without Renew the key would have died (300+300 > 500), Renew reset the TTL.
	mr.FastForward(300 * time.Millisecond)
	if !mr.Exists(ConclaveKey(testKIDa)) {
		t.Error("key gone after Renew+FastForward — Renew must extend TTL")
	}
}

func TestRenewInstance_KeyExpired_NotOK(t *testing.T) {
	c, mr := newClientMR(t)
	ctx := context.Background()

	if err := RegisterInstance(ctx, c, testKIDa, "meta-a", 100*time.Millisecond, false); err != nil {
		t.Fatalf("RegisterInstance: %v", err)
	}
	mr.FastForward(200 * time.Millisecond)

	ok, err := RenewInstance(ctx, c, testKIDa, time.Second)
	if err != nil {
		t.Fatalf("RenewInstance after expiry: %v", err)
	}
	if ok {
		t.Error("RenewInstance ok=true on expired key, want false (caller re-registers)")
	}
}

func TestDeregisterInstance_RemovesKey(t *testing.T) {
	c, mr := newClientMR(t)
	ctx := context.Background()

	if err := RegisterInstance(ctx, c, testKIDa, "meta-a", 30*time.Second, false); err != nil {
		t.Fatalf("RegisterInstance: %v", err)
	}
	if err := DeregisterInstance(ctx, c, testKIDa); err != nil {
		t.Fatalf("DeregisterInstance: %v", err)
	}
	if mr.Exists(ConclaveKey(testKIDa)) {
		t.Error("key must be deleted after Deregister")
	}
}

func TestDeregisterInstance_Idempotent(t *testing.T) {
	c, _ := newClientMR(t)
	ctx := context.Background()

	// A missing key → no-op, no error.
	if err := DeregisterInstance(ctx, c, testKIDa); err != nil {
		t.Fatalf("DeregisterInstance on missing key: %v", err)
	}
}

func TestLiveKIDsAndCount(t *testing.T) {
	c, mr := newClientMR(t)
	ctx := context.Background()

	for _, kid := range []string{testKIDa, testKIDb, testKIDc} {
		if err := RegisterInstance(ctx, c, kid, "m", 30*time.Second, false); err != nil {
			t.Fatalf("RegisterInstance %q: %v", kid, err)
		}
	}

	kids, err := LiveKIDs(ctx, c)
	if err != nil {
		t.Fatalf("LiveKIDs: %v", err)
	}
	sort.Strings(kids)
	want := []string{testKIDa, testKIDb, testKIDc}
	sort.Strings(want)
	if len(kids) != len(want) {
		t.Fatalf("LiveKIDs = %v, want %v", kids, want)
	}
	for i := range want {
		if kids[i] != want[i] {
			t.Fatalf("LiveKIDs = %v, want %v", kids, want)
		}
	}

	n, err := CountLive(ctx, c)
	if err != nil {
		t.Fatalf("CountLive: %v", err)
	}
	if n != 3 {
		t.Errorf("CountLive = %d, want 3", n)
	}

	// One instance crashed (TTL expired, no Deregister) — drops out of the result.
	mr.Del(ConclaveKey(testKIDb))
	n, err = CountLive(ctx, c)
	if err != nil {
		t.Fatalf("CountLive after death: %v", err)
	}
	if n != 2 {
		t.Errorf("CountLive after one death = %d, want 2", n)
	}
}

func TestLiveKIDs_Empty(t *testing.T) {
	c, _ := newClientMR(t)
	ctx := context.Background()

	kids, err := LiveKIDs(ctx, c)
	if err != nil {
		t.Fatalf("LiveKIDs: %v", err)
	}
	if len(kids) != 0 {
		t.Errorf("LiveKIDs on empty registry = %v, want empty", kids)
	}
	n, err := CountLive(ctx, c)
	if err != nil {
		t.Fatalf("CountLive: %v", err)
	}
	if n != 0 {
		t.Errorf("CountLive on empty = %d, want 0", n)
	}
}

func TestInstanceAlive_Live(t *testing.T) {
	c, _ := newClientMR(t)
	ctx := context.Background()

	if err := RegisterInstance(ctx, c, testKIDa, "m", 30*time.Second, false); err != nil {
		t.Fatalf("RegisterInstance: %v", err)
	}
	alive, err := InstanceAlive(ctx, c, testKIDa)
	if err != nil {
		t.Fatalf("InstanceAlive: %v", err)
	}
	if !alive {
		t.Error("InstanceAlive = false on live instance, want true")
	}
}

func TestInstanceAlive_Dead(t *testing.T) {
	c, _ := newClientMR(t)
	ctx := context.Background()

	// The key was never registered (or it crashed and expired by TTL) → dead.
	alive, err := InstanceAlive(ctx, c, testKIDa)
	if err != nil {
		t.Fatalf("InstanceAlive: %v", err)
	}
	if alive {
		t.Error("InstanceAlive = true on absent key, want false (owner presence death)")
	}
}

func TestInstanceAlive_TTLExpiryDead(t *testing.T) {
	c, mr := newClientMR(t)
	ctx := context.Background()

	if err := RegisterInstance(ctx, c, testKIDa, "m", 100*time.Millisecond, false); err != nil {
		t.Fatalf("RegisterInstance: %v", err)
	}
	mr.FastForward(200 * time.Millisecond)

	alive, err := InstanceAlive(ctx, c, testKIDa)
	if err != nil {
		t.Fatalf("InstanceAlive: %v", err)
	}
	if alive {
		t.Error("InstanceAlive = true after TTL expiry, want false")
	}
}

func TestInstanceAlive_RedisErrorPropagates(t *testing.T) {
	c, mr := newClientMR(t)
	ctx := context.Background()
	mr.Close() // Redis unavailable → EXISTS will return an error.

	if _, err := InstanceAlive(ctx, c, testKIDa); err == nil {
		t.Error("InstanceAlive on broken Redis: want error (presence-check fail-safe on the caller side)")
	}
}

func TestInstanceAlive_RejectsInvalidArgs(t *testing.T) {
	c, _ := newClientMR(t)
	ctx := context.Background()

	if _, err := InstanceAlive(ctx, c, ""); err == nil {
		t.Error("empty kid: want error")
	}
	if _, err := InstanceAlive(ctx, nil, testKIDa); err == nil {
		t.Error("nil client: want error")
	}
}

func TestLiveKIDs_TTLExpiryDropsDead(t *testing.T) {
	c, mr := newClientMR(t)
	ctx := context.Background()

	if err := RegisterInstance(ctx, c, testKIDa, "m", 30*time.Second, false); err != nil {
		t.Fatalf("RegisterInstance a: %v", err)
	}
	if err := RegisterInstance(ctx, c, testKIDb, "m", 100*time.Millisecond, false); err != nil {
		t.Fatalf("RegisterInstance b: %v", err)
	}
	mr.FastForward(200 * time.Millisecond)

	kids, err := LiveKIDs(ctx, c)
	if err != nil {
		t.Fatalf("LiveKIDs: %v", err)
	}
	if len(kids) != 1 || kids[0] != testKIDa {
		t.Errorf("LiveKIDs after b expiry = %v, want [%s]", kids, testKIDa)
	}
}

func TestReadInstanceMeta_ReturnsStoredValue(t *testing.T) {
	c, _ := newClientMR(t)
	ctx := context.Background()

	if err := RegisterInstance(ctx, c, testKIDa, `{"started_at":"2026-07-01T00:00:00Z","kid":"keeper-eu-west-01"}`, 30*time.Second, false); err != nil {
		t.Fatalf("RegisterInstance: %v", err)
	}

	meta, ok, err := ReadInstanceMeta(ctx, c, testKIDa)
	if err != nil {
		t.Fatalf("ReadInstanceMeta: %v", err)
	}
	if !ok {
		t.Fatal("ReadInstanceMeta ok=false, want true (key exists)")
	}
	if meta != `{"started_at":"2026-07-01T00:00:00Z","kid":"keeper-eu-west-01"}` {
		t.Errorf("meta = %q, want stored JSON", meta)
	}
}

func TestReadInstanceMeta_MissingKey_NotOK(t *testing.T) {
	c, _ := newClientMR(t)
	ctx := context.Background()

	meta, ok, err := ReadInstanceMeta(ctx, c, testKIDa)
	if err != nil {
		t.Fatalf("ReadInstanceMeta on missing key: %v", err)
	}
	if ok {
		t.Errorf("ReadInstanceMeta ok=true on missing key (meta=%q), want false", meta)
	}
}

func TestPeekLeaseHolder_ReturnsHolder(t *testing.T) {
	c, mr := newClientMR(t)
	ctx := context.Background()

	mr.Set("reaper:leader", "keeper-us-east-01")

	holder, ok, err := PeekLeaseHolder(ctx, c, "reaper:leader")
	if err != nil {
		t.Fatalf("PeekLeaseHolder: %v", err)
	}
	if !ok || holder != "keeper-us-east-01" {
		t.Errorf("PeekLeaseHolder = (%q, %v), want (keeper-us-east-01, true)", holder, ok)
	}
}

func TestPeekLeaseHolder_NoLeader_NotOK(t *testing.T) {
	c, _ := newClientMR(t)
	ctx := context.Background()

	holder, ok, err := PeekLeaseHolder(ctx, c, "reaper:leader")
	if err != nil {
		t.Fatalf("PeekLeaseHolder on missing key: %v", err)
	}
	if ok {
		t.Errorf("PeekLeaseHolder ok=true on missing key (holder=%q), want false (no leader)", holder)
	}
}
