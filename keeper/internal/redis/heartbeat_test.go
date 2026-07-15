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

// --- Soul-capabilities (ADR-056 §S5 forward-compat) ---

func TestSoulCapabilities_SetAndHas(t *testing.T) {
	c, _ := newClientMR(t)
	ctx := context.Background()
	sid := "host.example.com"

	if err := SetSoulCapabilities(ctx, c, sid, []string{"passage"}); err != nil {
		t.Fatalf("SetSoulCapabilities: %v", err)
	}
	has, err := SoulHasCapability(ctx, c, sid, "passage")
	if err != nil {
		t.Fatalf("SoulHasCapability: %v", err)
	}
	if !has {
		t.Error("passage not reported after announce")
	}
	hasOther, _ := SoulHasCapability(ctx, c, sid, "unknown-cap")
	if hasOther {
		t.Error("unknown-cap reported as supported")
	}
}

// TestSoulCapabilities_EmptySetOverwrites — an old binary (empty set) reconnecting
// after a newer one MUST overwrite the stale "passage" flag (fail-closed on reconnect).
func TestSoulCapabilities_EmptySetOverwrites(t *testing.T) {
	c, _ := newClientMR(t)
	ctx := context.Background()
	sid := "host.example.com"

	if err := SetSoulCapabilities(ctx, c, sid, []string{"passage"}); err != nil {
		t.Fatalf("SetSoulCapabilities(passage): %v", err)
	}
	// Same SID reconnected with the old binary — empty set.
	if err := SetSoulCapabilities(ctx, c, sid, nil); err != nil {
		t.Fatalf("SetSoulCapabilities(nil): %v", err)
	}
	has, _ := SoulHasCapability(ctx, c, sid, "passage")
	if has {
		t.Error("stale passage survived reconnect of old binary — fail-closed broken")
	}
}

// TestSoulHasCapability_MissingFailClosed — missing key/field → false (an old Soul
// without an announcement is treated as unsupporting, fail-closed).
func TestSoulHasCapability_MissingFailClosed(t *testing.T) {
	c, _ := newClientMR(t)
	ctx := context.Background()
	has, err := SoulHasCapability(ctx, c, "ghost.example.com", "passage")
	if err != nil {
		t.Fatalf("SoulHasCapability: %v", err)
	}
	if has {
		t.Error("missing key reported passage-capable")
	}
}

// TestSoulsLackingCapability_Batch — batch check: host-a announced passage,
// host-b didn't (empty), host-c has no key at all → lacking = {host-b, host-c}.
func TestSoulsLackingCapability_Batch(t *testing.T) {
	c, _ := newClientMR(t)
	ctx := context.Background()
	if err := SetSoulCapabilities(ctx, c, "host-a", []string{"passage"}); err != nil {
		t.Fatalf("set a: %v", err)
	}
	if err := SetSoulCapabilities(ctx, c, "host-b", nil); err != nil {
		t.Fatalf("set b: %v", err)
	}
	// host-c — no key at all.

	lacking, err := SoulsLackingCapability(ctx, c, []string{"host-a", "host-b", "host-c"}, "passage")
	if err != nil {
		t.Fatalf("SoulsLackingCapability: %v", err)
	}
	got := map[string]bool{}
	for _, s := range lacking {
		got[s] = true
	}
	if got["host-a"] {
		t.Error("host-a (passage-capable) reported as lacking")
	}
	if !got["host-b"] || !got["host-c"] {
		t.Errorf("lacking = %v, want host-b and host-c (fail-closed)", lacking)
	}
}

// TestSoulsLackingCapability_AllCapable — everyone announced passage → lacking is
// empty (all Souls on a single beta version: the staged gate passes).
func TestSoulsLackingCapability_AllCapable(t *testing.T) {
	c, _ := newClientMR(t)
	ctx := context.Background()
	for _, sid := range []string{"host-a", "host-b"} {
		if err := SetSoulCapabilities(ctx, c, sid, []string{"passage"}); err != nil {
			t.Fatalf("set %s: %v", sid, err)
		}
	}
	lacking, err := SoulsLackingCapability(ctx, c, []string{"host-a", "host-b"}, "passage")
	if err != nil {
		t.Fatalf("SoulsLackingCapability: %v", err)
	}
	if len(lacking) != 0 {
		t.Errorf("lacking = %v, want [] (all passage-capable)", lacking)
	}
}
