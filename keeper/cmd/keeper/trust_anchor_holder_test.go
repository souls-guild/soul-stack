package main

import (
	"sync"
	"testing"

	keepergrpc "github.com/souls-guild/soul-stack/keeper/internal/grpc"
)

// trustAnchorHolder must satisfy [keepergrpc.TrustAnchorSource] (connect-time
// broadcast of the anchor set, R3-S6).
var _ keepergrpc.TrustAnchorSource = (*trustAnchorHolder)(nil)

func TestTrustAnchorHolder_SetAndGet(t *testing.T) {
	h := &trustAnchorHolder{}
	if got := h.AnchorSetPEM(); got != nil {
		t.Fatalf("fresh holder: AnchorSetPEM = %v, want nil", got)
	}

	h.set([]string{"pem-1", "pem-2"})
	got := h.AnchorSetPEM()
	if len(got) != 2 || got[0] != "pem-1" || got[1] != "pem-2" {
		t.Fatalf("AnchorSetPEM = %v, want [pem-1 pem-2]", got)
	}

	// ReplaceAll: a new set replaces the whole set entirely.
	h.set([]string{"pem-3"})
	got = h.AnchorSetPEM()
	if len(got) != 1 || got[0] != "pem-3" {
		t.Fatalf("after ReplaceAll AnchorSetPEM = %v, want [pem-3]", got)
	}
}

// TestTrustAnchorHolder_SetCopiesInput -- the caller may reuse the buffer:
// mutating the original slice doesn't affect the holder's snapshot.
func TestTrustAnchorHolder_SetCopiesInput(t *testing.T) {
	h := &trustAnchorHolder{}
	in := []string{"pem-1", "pem-2"}
	h.set(in)
	in[0] = "mutated"
	if got := h.AnchorSetPEM(); got[0] != "pem-1" {
		t.Errorf("holder shares the buffer with the caller: got[0] = %q, want pem-1", got[0])
	}
}

// TestTrustAnchorHolder_NilSafe -- AnchorSetPEM on a nil holder returns nil
// without panicking (Sigil disabled -> connect-time broadcast is a no-op).
func TestTrustAnchorHolder_NilSafe(t *testing.T) {
	var h *trustAnchorHolder
	if got := h.AnchorSetPEM(); got != nil {
		t.Errorf("nil holder: AnchorSetPEM = %v, want nil", got)
	}
}

// TestTrustAnchorHolder_Race -- concurrent set (rotation watcher) and
// AnchorSetPEM (connect-time broadcast). Run with -race.
func TestTrustAnchorHolder_Race(t *testing.T) {
	h := &trustAnchorHolder{}
	h.set([]string{"pem-0"})

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		for i := 0; i < 500; i++ {
			h.set([]string{"pem-a", "pem-b"})
		}
	}()
	go func() {
		defer wg.Done()
		for i := 0; i < 500; i++ {
			_ = h.AnchorSetPEM()
		}
	}()
	wg.Wait()
}
