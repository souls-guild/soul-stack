package redis

import (
	"context"
	"testing"
	"time"
)

func TestHeartbeatKey(t *testing.T) {
	got := HeartbeatKey("host.example.com")
	want := "soul:host.example.com:hb"
	if got != want {
		t.Fatalf("HeartbeatKey = %q, want %q", got, want)
	}
}

func TestTouchHeartbeat_WritesAtAndKid(t *testing.T) {
	c, mr := newClientMR(t)
	ctx := context.Background()
	sid := "host.example.com"
	kid := "kid-eu-1"
	when := time.Date(2026, 5, 22, 12, 0, 0, 0, time.UTC)

	if err := TouchHeartbeat(ctx, c, sid, kid, when); err != nil {
		t.Fatalf("TouchHeartbeat: %v", err)
	}

	got := mr.HGet(HeartbeatKey(sid), "at")
	if got != when.Format(time.RFC3339Nano) {
		t.Errorf("at = %q, want %q", got, when.Format(time.RFC3339Nano))
	}
	gotKid := mr.HGet(HeartbeatKey(sid), "kid")
	if gotKid != kid {
		t.Errorf("kid = %q, want %q", gotKid, kid)
	}
}

func TestTouchHeartbeat_DefaultsToNow(t *testing.T) {
	c, mr := newClientMR(t)
	ctx := context.Background()
	sid := "host.example.com"

	before := time.Now().Add(-time.Second)
	if err := TouchHeartbeat(ctx, c, sid, "kid-1", time.Time{}); err != nil {
		t.Fatalf("TouchHeartbeat: %v", err)
	}
	after := time.Now().Add(time.Second)

	atStr := mr.HGet(HeartbeatKey(sid), "at")
	at, err := time.Parse(time.RFC3339Nano, atStr)
	if err != nil {
		t.Fatalf("parse at: %v", err)
	}
	if at.Before(before) || at.After(after) {
		t.Errorf("at=%v outside [%v..%v]", at, before, after)
	}
}

func TestTouchHeartbeat_RejectsEmpty(t *testing.T) {
	c, _ := newClientMR(t)
	ctx := context.Background()
	if err := TouchHeartbeat(ctx, c, "", "kid", time.Now()); err == nil {
		t.Error("empty sid returned nil err")
	}
	if err := TouchHeartbeat(ctx, c, "host", "", time.Now()); err == nil {
		t.Error("empty kid returned nil err")
	}
}

func TestReadHeartbeat_RoundTrip(t *testing.T) {
	c, _ := newClientMR(t)
	ctx := context.Background()
	sid := "host.example.com"
	kid := "kid-1"
	when := time.Date(2026, 5, 22, 12, 0, 0, 0, time.UTC)
	if err := TouchHeartbeat(ctx, c, sid, kid, when); err != nil {
		t.Fatalf("TouchHeartbeat: %v", err)
	}

	gotAt, gotKid, ok, err := ReadHeartbeat(ctx, c, sid)
	if err != nil {
		t.Fatalf("ReadHeartbeat: %v", err)
	}
	if !ok {
		t.Fatal("ok=false, want true")
	}
	if !gotAt.Equal(when) {
		t.Errorf("at = %v, want %v", gotAt, when)
	}
	if gotKid != kid {
		t.Errorf("kid = %q, want %q", gotKid, kid)
	}
}

func TestReadHeartbeat_Missing(t *testing.T) {
	c, _ := newClientMR(t)
	ctx := context.Background()
	_, _, ok, err := ReadHeartbeat(ctx, c, "ghost.example.com")
	if err != nil {
		t.Fatalf("ReadHeartbeat: %v", err)
	}
	if ok {
		t.Error("ok=true on missing key")
	}
}
