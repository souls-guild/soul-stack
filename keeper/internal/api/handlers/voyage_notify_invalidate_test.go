package handlers

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
)

// spyTidingInvalidator — a counter for two-level invalidation (in-process +
// cross-keeper). Implements [TidingInvalidator]. Mimics
// herald.Service.InvalidateTidings, which internally triggers both levels;
// the test checks specifically that persist calls it after commit (race fix
// ADR-052(g)). We keep separate in-process / publish counters so the guard
// stays meaningful even if the interface gets split in the future.
type spyTidingInvalidator struct {
	mu          sync.Mutex
	inProcess   int // equivalent to InvalidateRules
	crossKeeper int // equivalent to PublishHeraldInvalidate
	lastName    string
}

func (s *spyTidingInvalidator) InvalidateTidings(_ context.Context, name string) {
	s.mu.Lock()
	s.inProcess++
	s.crossKeeper++
	s.lastName = name
	s.mu.Unlock()
}

func (s *spyTidingInvalidator) counts() (inProc, cross int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.inProcess, s.crossKeeper
}

// newVoyageHandlerWithInvalidator — a handler with a spy invalidator + an enforcer
// that allows incarnation.run/errand.run/herald.read (notify requires herald.read
// on the channel, ADR-052(g)). heraldExists=true is set by the calling test
// (notify-existence-check).
func newVoyageHandlerWithInvalidator(store *fakeVoyageStore, sc VoyageScenarioResolver, cmd VoyageCommandResolver, inv TidingInvalidator) *VoyageHandler {
	enf := &fakeVoyageEnforcer{allow: map[string]bool{
		"incarnation.run": true,
		"errand.run":      true,
		"herald.read":     true,
	}}
	return NewVoyageHandler(store, sc, cmd, nil /*incReader*/, enf, nil /*scoper*/, nil /*auditW*/, inv, 0, 0, nil)
}

// TestVoyageNotify_InvalidatesAfterCommit — guard A (race fix ADR-052(g)):
// the ephemeral Tiding from notify is inserted via a direct InsertTiding in the voyage tx,
// bypassing herald.Service invalidation; persist must EXPLICITLY invalidate the
// dispatcher's snapshot AFTER commit, otherwise a fast run dispatches the terminal against
// a stale TTL snapshot and the notification silently misses.
//
// This test is RED without the fix (the handler never called invalidation at all).
func TestVoyageNotify_InvalidatesAfterCommit(t *testing.T) {
	store := &fakeVoyageStore{heraldExists: true}
	inv := &spyTidingInvalidator{}
	h := newVoyageHandlerWithInvalidator(store, &fakeVoyageScenarioResolver{out: []string{"inc-a"}}, &fakeVoyageCommandResolver{}, inv)

	rec := httptest.NewRecorder()
	h.Create(rec, voyageReq(http.MethodPost, "/v1/voyages",
		`{"kind":"scenario","scenario_name":"deploy","target":{"service":"web"},`+
			`"notify":[{"herald":"ops-webhook","on":["completed"]}]}`))

	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202; body=%s", rec.Code, rec.Body.String())
	}
	if !store.committed {
		t.Fatal("tx not committed — test invariant broken")
	}
	if store.insertTidings != 1 {
		t.Fatalf("insertTidings = %d, want 1 (one notify element)", store.insertTidings)
	}
	inProc, cross := inv.counts()
	if inProc != 1 {
		t.Errorf("in-process InvalidateRules called %d times, want exactly 1", inProc)
	}
	if cross != 1 {
		t.Errorf("cross-keeper publish called %d times, want exactly 1", cross)
	}
}

// TestVoyageNotify_NoNotify_NoInvalidate — negative guard: without a notify block
// there's nothing to invalidate (no ephemeral rules were created) — invalidate is NOT called.
func TestVoyageNotify_NoNotify_NoInvalidate(t *testing.T) {
	store := &fakeVoyageStore{}
	inv := &spyTidingInvalidator{}
	h := newVoyageHandlerWithInvalidator(store, &fakeVoyageScenarioResolver{out: []string{"inc-a"}}, &fakeVoyageCommandResolver{}, inv)

	rec := httptest.NewRecorder()
	h.Create(rec, voyageReq(http.MethodPost, "/v1/voyages",
		`{"kind":"scenario","scenario_name":"deploy","target":{"service":"web"}}`))

	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202; body=%s", rec.Code, rec.Body.String())
	}
	if store.insertTidings != 0 {
		t.Fatalf("insertTidings = %d, want 0", store.insertTidings)
	}
	if inProc, cross := inv.counts(); inProc != 0 || cross != 0 {
		t.Errorf("invalidate called (in=%d cross=%d), want 0/0 — no notify means no rules", inProc, cross)
	}
}

// TestVoyageNotify_TidingInsertFails_NoInvalidate — the key "only after commit"
// invariant: an INSERT INTO tidings failure rolls back the tx (the Voyage isn't
// created), invalidate is NOT called (nothing to invalidate — no rules in the DB).
func TestVoyageNotify_TidingInsertFails_NoInvalidate(t *testing.T) {
	store := &fakeVoyageStore{heraldExists: true, insertTidingErr: context.DeadlineExceeded}
	inv := &spyTidingInvalidator{}
	h := newVoyageHandlerWithInvalidator(store, &fakeVoyageScenarioResolver{out: []string{"inc-a"}}, &fakeVoyageCommandResolver{}, inv)

	rec := httptest.NewRecorder()
	h.Create(rec, voyageReq(http.MethodPost, "/v1/voyages",
		`{"kind":"scenario","scenario_name":"deploy","target":{"service":"web"},`+
			`"notify":[{"herald":"ops-webhook","on":["completed"]}]}`))

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500 (insert tiding failed); body=%s", rec.Code, rec.Body.String())
	}
	if store.committed {
		t.Fatal("tx committed despite InsertTiding failure — atomicity violated")
	}
	if inProc, cross := inv.counts(); inProc != 0 || cross != 0 {
		t.Errorf("invalidate called on rollback (in=%d cross=%d), want 0/0 — the \"only after commit\" invariant is violated", inProc, cross)
	}
}
