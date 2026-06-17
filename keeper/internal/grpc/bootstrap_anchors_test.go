package grpc

import (
	"log/slog"
	"sync/atomic"
	"testing"

	keeperv1 "github.com/souls-guild/soul-stack/proto/gen/go/keeper/v1"
)

// mutableAnchorSource — TrustAnchorSource с атомарно-сменяемым набором (модель
// daemon.trustAnchorHolder): SetAnchors подменяет набор, AnchorSetPEM читает
// текущий. Для теста «bootstrap-reply из живого источника» (R3-S7, architect af7d).
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

// TestBootstrap_ApplySigilAnchors_LiveSource — gate-фикс R3-S7: reply читает
// набор якорей ЖИВЫМ при каждом формировании, а не снимок старта. После
// SetAnchors следующий reply несёт НОВЫЙ набор (старый Soul между bootstrap и
// connect получает актуальные якоря — safety для Retire).
func TestBootstrap_ApplySigilAnchors_LiveSource(t *testing.T) {
	src := &mutableAnchorSource{}
	src.SetAnchors([]string{"PEM-A", "PEM-B"})

	h := &bootstrapHandler{
		deps:   BootstrapDeps{SigilAnchorSource: src},
		logger: slog.New(slog.DiscardHandler),
	}

	// Reply №1 — стартовый набор.
	r1 := &keeperv1.BootstrapReply{}
	h.applySigilAnchors(r1)
	if got := r1.GetSigilPubkeyPemSet(); len(got) != 2 || got[0] != "PEM-A" || got[1] != "PEM-B" {
		t.Fatalf("reply1 set = %v, want [PEM-A PEM-B]", got)
	}
	if r1.GetSigilPubkeyPem() != "PEM-A" {
		t.Errorf("reply1 single = %q, want PEM-A (первый якорь)", r1.GetSigilPubkeyPem())
	}

	// Ротация: набор сменился (старый PEM-A выведен, добавлены C/D).
	src.SetAnchors([]string{"PEM-C", "PEM-D"})

	// Reply №2 — НОВЫЙ набор (а не снимок старта).
	r2 := &keeperv1.BootstrapReply{}
	h.applySigilAnchors(r2)
	if got := r2.GetSigilPubkeyPemSet(); len(got) != 2 || got[0] != "PEM-C" || got[1] != "PEM-D" {
		t.Fatalf("reply2 set = %v, want [PEM-C PEM-D] (живой источник)", got)
	}
	if r2.GetSigilPubkeyPem() != "PEM-C" {
		t.Errorf("reply2 single = %q, want PEM-C", r2.GetSigilPubkeyPem())
	}
}

// TestBootstrap_ApplySigilAnchors_Disabled — source nil / пустой набор → оба
// Sigil-поля reply остаются пустыми (Sigil выключен, bootstrap обратносовместим).
func TestBootstrap_ApplySigilAnchors_Disabled(t *testing.T) {
	// nil source.
	h := &bootstrapHandler{deps: BootstrapDeps{}, logger: slog.New(slog.DiscardHandler)}
	r := &keeperv1.BootstrapReply{}
	h.applySigilAnchors(r)
	if r.GetSigilPubkeyPem() != "" || r.GetSigilPubkeyPemSet() != nil {
		t.Errorf("nil source: single=%q set=%v, want empty", r.GetSigilPubkeyPem(), r.GetSigilPubkeyPemSet())
	}

	// Пустой набор.
	src := &mutableAnchorSource{}
	src.SetAnchors(nil)
	h2 := &bootstrapHandler{deps: BootstrapDeps{SigilAnchorSource: src}, logger: slog.New(slog.DiscardHandler)}
	r2 := &keeperv1.BootstrapReply{}
	h2.applySigilAnchors(r2)
	if r2.GetSigilPubkeyPem() != "" || r2.GetSigilPubkeyPemSet() != nil {
		t.Errorf("empty set: single=%q set=%v, want empty", r2.GetSigilPubkeyPem(), r2.GetSigilPubkeyPemSet())
	}
}
