package redis

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestSoulLeaseKey(t *testing.T) {
	got := SoulLeaseKey("host.example.com")
	want := "soul:host.example.com:lock"
	if got != want {
		t.Fatalf("SoulLeaseKey = %q, want %q", got, want)
	}
}

func TestAcquireSoulLease_HappyPath(t *testing.T) {
	c, mr := newClientMR(t)
	ctx := context.Background()

	l, err := AcquireSoulLease(ctx, c, "host.example.com", "kid-eu-1", 5*time.Second)
	if err != nil {
		t.Fatalf("AcquireSoulLease: %v", err)
	}
	if l.Key() != "soul:host.example.com:lock" {
		t.Errorf("Key = %q, want soul:host.example.com:lock", l.Key())
	}
	if l.Holder() != "kid-eu-1" {
		t.Errorf("Holder = %q, want kid-eu-1", l.Holder())
	}
	if v, _ := mr.Get("soul:host.example.com:lock"); v != "kid-eu-1" {
		t.Errorf("redis value = %q, want kid-eu-1", v)
	}
}

func TestAcquireSoulLease_Conflict(t *testing.T) {
	c, _ := newClientMR(t)
	ctx := context.Background()
	sid := "host.example.com"

	if _, err := AcquireSoulLease(ctx, c, sid, "kid-eu-1", 5*time.Second); err != nil {
		t.Fatalf("first acquire: %v", err)
	}
	_, err := AcquireSoulLease(ctx, c, sid, "kid-eu-2", 5*time.Second)
	if !errors.Is(err, ErrLeaseTaken) {
		t.Fatalf("second acquire err = %v, want ErrLeaseTaken", err)
	}
}

func TestSoulLeaseOwner_HeldReturnsKID(t *testing.T) {
	c, _ := newClientMR(t)
	ctx := context.Background()
	sid := "host.example.com"

	if _, err := AcquireSoulLease(ctx, c, sid, "kid-eu-1", 5*time.Second); err != nil {
		t.Fatalf("AcquireSoulLease: %v", err)
	}
	kid, ok, err := SoulLeaseOwner(ctx, c, sid)
	if err != nil {
		t.Fatalf("SoulLeaseOwner: %v", err)
	}
	if !ok {
		t.Fatal("ok = false, want true (lease held)")
	}
	if kid != "kid-eu-1" {
		t.Errorf("kid = %q, want kid-eu-1", kid)
	}
}

func TestSoulLeaseOwner_AbsentKeyNotError(t *testing.T) {
	c, _ := newClientMR(t)
	ctx := context.Background()

	kid, ok, err := SoulLeaseOwner(ctx, c, "no-such-host")
	if err != nil {
		t.Fatalf("SoulLeaseOwner on absent key returned err: %v", err)
	}
	if ok {
		t.Errorf("ok = true, want false (no lease)")
	}
	if kid != "" {
		t.Errorf("kid = %q, want empty", kid)
	}
}

func TestSoulLeaseOwner_Validation(t *testing.T) {
	c, _ := newClientMR(t)
	ctx := context.Background()

	if _, _, err := SoulLeaseOwner(ctx, nil, "host"); err == nil {
		t.Error("nil client returned no error")
	}
	if _, _, err := SoulLeaseOwner(ctx, c, ""); err == nil {
		t.Error("empty sid returned no error")
	}
}

func TestForceAcquireSoulLease_KeyIsPrevHolder_Reacquires(t *testing.T) {
	c, mr := newClientMR(t)
	ctx := context.Background()
	sid := "host.example.com"

	// Мёртвый prev-holder всё ещё держит ключ (TTL не истёк после crash-а).
	if _, err := AcquireSoulLease(ctx, c, sid, "kid-dead", 60*time.Second); err != nil {
		t.Fatalf("seed lease: %v", err)
	}

	l, err := ForceAcquireSoulLease(ctx, c, sid, "kid-dead", "kid-new", 30*time.Second)
	if err != nil {
		t.Fatalf("ForceAcquireSoulLease: %v", err)
	}
	if l == nil {
		t.Fatal("lease = nil on successful re-acquire")
	}
	if l.Holder() != "kid-new" {
		t.Errorf("Holder = %q, want kid-new", l.Holder())
	}
	if v, _ := mr.Get(SoulLeaseKey(sid)); v != "kid-new" {
		t.Errorf("redis value = %q, want kid-new (CAS-by-prev-holder перезахватил)", v)
	}
}

func TestForceAcquireSoulLease_KeyChanged_DoesNotReacquire(t *testing.T) {
	c, mr := newClientMR(t)
	ctx := context.Background()
	sid := "host.example.com"

	// В гонке ключ уже сменился: им владеет НЕ доказанно-мёртвый prevKID, а
	// третий живой Keeper. CAS-by-prev-holder НЕ должен его перетереть.
	if _, err := AcquireSoulLease(ctx, c, sid, "kid-other", 60*time.Second); err != nil {
		t.Fatalf("seed lease: %v", err)
	}

	l, err := ForceAcquireSoulLease(ctx, c, sid, "kid-dead", "kid-new", 30*time.Second)
	if !errors.Is(err, ErrLeaseTaken) {
		t.Fatalf("err = %v, want ErrLeaseTaken (ключ принадлежит не-prev-holder-у)", err)
	}
	if l != nil {
		t.Error("lease != nil on failed CAS")
	}
	if v, _ := mr.Get(SoulLeaseKey(sid)); v != "kid-other" {
		t.Errorf("redis value = %q, want kid-other (не перезахвачен)", v)
	}
}

func TestForceAcquireSoulLease_KeyAbsent_SetnxAcquires(t *testing.T) {
	c, mr := newClientMR(t)
	ctx := context.Background()
	sid := "host.example.com"

	// prev-holder уже истёк по TTL (ключа нет) → штатный SETNX-захват.
	l, err := ForceAcquireSoulLease(ctx, c, sid, "kid-dead", "kid-new", 30*time.Second)
	if err != nil {
		t.Fatalf("ForceAcquireSoulLease on absent key: %v", err)
	}
	if l == nil || l.Holder() != "kid-new" {
		t.Fatalf("lease=%v, want holder kid-new", l)
	}
	if v, _ := mr.Get(SoulLeaseKey(sid)); v != "kid-new" {
		t.Errorf("redis value = %q, want kid-new (SETNX-захват)", v)
	}
}

func TestForceAcquireSoulLease_RenewWorksAfterReacquire(t *testing.T) {
	c, _ := newClientMR(t)
	ctx := context.Background()
	sid := "host.example.com"

	if _, err := AcquireSoulLease(ctx, c, sid, "kid-dead", 60*time.Second); err != nil {
		t.Fatalf("seed lease: %v", err)
	}
	l, err := ForceAcquireSoulLease(ctx, c, sid, "kid-dead", "kid-new", 30*time.Second)
	if err != nil {
		t.Fatalf("ForceAcquireSoulLease: %v", err)
	}
	// Возвращённый handle принадлежит новому holder-у — Renew по CAS проходит.
	if err := l.Renew(ctx); err != nil {
		t.Errorf("Renew after re-acquire: %v (handle должен быть kid-new)", err)
	}
}

func TestForceAcquireSoulLease_Validation(t *testing.T) {
	c, _ := newClientMR(t)
	ctx := context.Background()

	if _, err := ForceAcquireSoulLease(ctx, nil, "h", "p", "n", time.Second); err == nil {
		t.Error("nil client: want error")
	}
	if _, err := ForceAcquireSoulLease(ctx, c, "", "p", "n", time.Second); err == nil {
		t.Error("empty sid: want error")
	}
	if _, err := ForceAcquireSoulLease(ctx, c, "h", "", "n", time.Second); err == nil {
		t.Error("empty prevKID: want error")
	}
	if _, err := ForceAcquireSoulLease(ctx, c, "h", "p", "", time.Second); err == nil {
		t.Error("empty newKID: want error")
	}
	if _, err := ForceAcquireSoulLease(ctx, c, "h", "p", "n", 0); err == nil {
		t.Error("zero ttl: want error")
	}
}
