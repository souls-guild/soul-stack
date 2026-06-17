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
