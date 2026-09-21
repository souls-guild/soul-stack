package grpc

import (
	"log/slog"
	"sync/atomic"
	"testing"

	keeperv1 "github.com/souls-guild/soul-stack/proto/gen/go/keeper/v1"
)

// mutableAnchorSource — a TrustAnchorSource with an atomically swappable set
// (models daemon.trustAnchorHolder): SetAnchors replaces the set, AnchorSetPEM
// reads the current one. For the "bootstrap reply from a live source" test
// (R3-S7, architect af7d).
type mutableAnchorSource struct {
	pems atomic.Pointer[[]string]
}

func (s *mutableAnchorSource) SetAnchors(p []string) { s.pems.Store(&p) }
func (s *mutableAnchorSource) AnchorSetPEM() []string {
	if v := s.pems.Load(); v != nil {
		return *v
	}
	return nil
}

// TestBootstrap_ApplySigilAnchors_LiveSource — gate fix R3-S7: the reply reads
// the anchor set LIVE every time it's built, not a startup snapshot. After
// SetAnchors, the next reply carries the NEW set (a Soul between bootstrap and
// connect gets the current anchors — safety for Retire).
func TestBootstrap_ApplySigilAnchors_LiveSource(t *testing.T) {
	src := &mutableAnchorSource{}
	src.SetAnchors([]string{"PEM-A", "PEM-B"})

	h := &bootstrapHandler{
		deps:   BootstrapDeps{SigilAnchorSource: src},
		logger: slog.New(slog.DiscardHandler),
	}

	// Reply #1 — the starting set.
	r1 := &keeperv1.BootstrapReply{}
	h.applySigilAnchors(r1)
	if got := r1.GetSigilPubkeyPemSet(); len(got) != 2 || got[0] != "PEM-A" || got[1] != "PEM-B" {
		t.Fatalf("reply1 set = %v, want [PEM-A PEM-B]", got)
	}
	if r1.GetSigilPubkeyPem() != "PEM-A" {
		t.Errorf("reply1 single = %q, want PEM-A (first anchor)", r1.GetSigilPubkeyPem())
	}

	// Rotation: the set changed (old PEM-A retired, C/D added).
	src.SetAnchors([]string{"PEM-C", "PEM-D"})

	// Reply #2 — the NEW set (not a startup snapshot).
	r2 := &keeperv1.BootstrapReply{}
	h.applySigilAnchors(r2)
	if got := r2.GetSigilPubkeyPemSet(); len(got) != 2 || got[0] != "PEM-C" || got[1] != "PEM-D" {
		t.Fatalf("reply2 set = %v, want [PEM-C PEM-D] (live source)", got)
	}
	if r2.GetSigilPubkeyPem() != "PEM-C" {
		t.Errorf("reply2 single = %q, want PEM-C", r2.GetSigilPubkeyPem())
	}
}

// TestBootstrap_ApplySigilAnchors_Disabled — nil source / empty set → both
// Sigil reply fields stay empty (Sigil disabled, bootstrap stays backward-compatible).
func TestBootstrap_ApplySigilAnchors_Disabled(t *testing.T) {
	// nil source.
	h := &bootstrapHandler{deps: BootstrapDeps{}, logger: slog.New(slog.DiscardHandler)}
	r := &keeperv1.BootstrapReply{}
	h.applySigilAnchors(r)
	if r.GetSigilPubkeyPem() != "" || r.GetSigilPubkeyPemSet() != nil {
		t.Errorf("nil source: single=%q set=%v, want empty", r.GetSigilPubkeyPem(), r.GetSigilPubkeyPemSet())
	}

	// Empty set.
	src := &mutableAnchorSource{}
	src.SetAnchors(nil)
	h2 := &bootstrapHandler{deps: BootstrapDeps{SigilAnchorSource: src}, logger: slog.New(slog.DiscardHandler)}
	r2 := &keeperv1.BootstrapReply{}
	h2.applySigilAnchors(r2)
	if r2.GetSigilPubkeyPem() != "" || r2.GetSigilPubkeyPemSet() != nil {
		t.Errorf("empty set: single=%q set=%v, want empty", r2.GetSigilPubkeyPem(), r2.GetSigilPubkeyPemSet())
	}
}
