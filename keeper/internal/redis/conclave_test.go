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
	// Второй процесс с тем же KID при requireUnique=true → коллизия.
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
	// Рестарт того же KID: requireUnique=false должен безусловно перетереть
	// собственный остаток (чужой TTL-ключ ещё не истёк).
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

	// Ещё 300 ms: без Renew ключ бы умер (300+300 > 500), Renew сбросил TTL.
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

	// Отсутствующий ключ → no-op без ошибки.
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

	// Один инстанс crash-нул (TTL истёк, без Deregister) — выпадает из выборки.
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

	// Ключ никогда не регистрировался (или crash-нул и истёк по TTL) → мёртв.
	alive, err := InstanceAlive(ctx, c, testKIDa)
	if err != nil {
		t.Fatalf("InstanceAlive: %v", err)
	}
	if alive {
		t.Error("InstanceAlive = true on absent key, want false (presence-смерть владельца)")
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
	mr.Close() // Redis недоступен → EXISTS вернёт ошибку.

	if _, err := InstanceAlive(ctx, c, testKIDa); err == nil {
		t.Error("InstanceAlive on broken Redis: want error (presence-чек fail-safe у caller-а)")
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
