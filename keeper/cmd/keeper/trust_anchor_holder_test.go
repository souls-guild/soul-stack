package main

import (
	"sync"
	"testing"

	keepergrpc "github.com/souls-guild/soul-stack/keeper/internal/grpc"
)

// trustAnchorHolder обязан удовлетворять [keepergrpc.TrustAnchorSource]
// (connect-time broadcast набора якорей, R3-S6).
var _ keepergrpc.TrustAnchorSource = (*trustAnchorHolder)(nil)

func TestTrustAnchorHolder_SetAndGet(t *testing.T) {
	h := &trustAnchorHolder{}
	if got := h.AnchorSetPEM(); got != nil {
		t.Fatalf("свежий holder: AnchorSetPEM = %v, want nil", got)
	}

	h.set([]string{"pem-1", "pem-2"})
	got := h.AnchorSetPEM()
	if len(got) != 2 || got[0] != "pem-1" || got[1] != "pem-2" {
		t.Fatalf("AnchorSetPEM = %v, want [pem-1 pem-2]", got)
	}

	// ReplaceAll: новый set заменяет весь набор целиком.
	h.set([]string{"pem-3"})
	got = h.AnchorSetPEM()
	if len(got) != 1 || got[0] != "pem-3" {
		t.Fatalf("после ReplaceAll AnchorSetPEM = %v, want [pem-3]", got)
	}
}

// TestTrustAnchorHolder_SetCopiesInput — caller может переиспользовать буфер:
// мутация исходного слайса не трогает снимок holder-а.
func TestTrustAnchorHolder_SetCopiesInput(t *testing.T) {
	h := &trustAnchorHolder{}
	in := []string{"pem-1", "pem-2"}
	h.set(in)
	in[0] = "mutated"
	if got := h.AnchorSetPEM(); got[0] != "pem-1" {
		t.Errorf("holder разделяет буфер с caller-ом: got[0] = %q, want pem-1", got[0])
	}
}

// TestTrustAnchorHolder_NilSafe — AnchorSetPEM на nil-holder возвращает nil без
// паники (Sigil выключен → connect-time broadcast no-op).
func TestTrustAnchorHolder_NilSafe(t *testing.T) {
	var h *trustAnchorHolder
	if got := h.AnchorSetPEM(); got != nil {
		t.Errorf("nil holder: AnchorSetPEM = %v, want nil", got)
	}
}

// TestTrustAnchorHolder_Race — конкурентные set (watcher ротации) и AnchorSetPEM
// (connect-time broadcast). Запускать с -race.
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
