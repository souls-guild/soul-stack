package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/souls-guild/soul-stack/keeper/internal/soul"
)

// fakePresence — mock [SoulPresence] for presence-overlay tests of `GET /v1/souls`.
// alive — the set of SIDs holding a "live" Redis SID lease (online). err != nil →
// emulates a Redis outage (the handler degrades fail-safe to the PG snapshot). gotSIDs —
// captures the argument to verify the overlay asks about the lease ONLY for
// presence-snapshot statuses (connected/disconnected), never lifecycle ones.
type fakePresence struct {
	alive   map[string]struct{}
	err     error
	gotSIDs []string
}

func (f *fakePresence) SoulsStreamAlive(_ context.Context, sids []string) (map[string]struct{}, error) {
	f.gotSIDs = append([]string(nil), sids...)
	if f.err != nil {
		return nil, f.err
	}
	return f.alive, nil
}

func aliveSet(sids ...string) map[string]struct{} {
	m := make(map[string]struct{}, len(sids))
	for _, s := range sids {
		m[s] = struct{}{}
	}
	return m
}

// TestSoulList_Reconnect_LeaseFlipsToConnected — the root live bug (ADR-006(a)):
// after the reaper's `mark_disconnected`, the PG snapshot `souls.status` = disconnected, but
// the Soul reconnected (a live Redis SID lease). Before the fix /v1/souls returned a stale
// disconnected; now the overlay derives status from the lease → connected, without
// a restart/manual action and without waiting for the reaper's next tick.
func TestSoulList_Reconnect_LeaseFlipsToConnected(t *testing.T) {
	pool := &fakeSoulPool{
		listCount: 2,
		listSouls: []*soul.Soul{
			{SID: "redis-01.example.com", Transport: soul.TransportAgent, Status: soul.StatusDisconnected, RegisteredAt: time.Now().UTC()},
			{SID: "redis-02.example.com", Transport: soul.TransportAgent, Status: soul.StatusDisconnected, RegisteredAt: time.Now().UTC()},
		},
	}
	// redis-01 reconnected (lease alive), redis-02 is genuinely offline (lease dead).
	presence := &fakePresence{alive: aliveSet("redis-01.example.com")}
	h := NewSoulHandler(pool, fakeScoper{unrestricted: true}, presence, nil)

	rec := doList(t, h, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}
	items := decodeListStatuses(t, rec.Body.Bytes())
	if items["redis-01.example.com"] != "connected" {
		t.Errorf("redis-01 status = %q, want connected (live lease, reconnect)", items["redis-01.example.com"])
	}
	if items["redis-02.example.com"] != "disconnected" {
		t.Errorf("redis-02 status = %q, want disconnected (lease dead)", items["redis-02.example.com"])
	}
}

// TestSoulList_StaleConnectedSnapshot_LeaseFlipsToDisconnected — the reverse
// direction: the PG snapshot is connected, but the lease has died (the stream broke, the reaper
// hasn't reconciled yet). The overlay returns the real disconnected, not a stale connected.
func TestSoulList_StaleConnectedSnapshot_LeaseFlipsToDisconnected(t *testing.T) {
	pool := &fakeSoulPool{
		listCount: 1,
		listSouls: []*soul.Soul{
			{SID: "web-01.example.com", Transport: soul.TransportAgent, Status: soul.StatusConnected, RegisteredAt: time.Now().UTC()},
		},
	}
	presence := &fakePresence{alive: aliveSet()} // lease dead.
	h := NewSoulHandler(pool, fakeScoper{unrestricted: true}, presence, nil)

	rec := doList(t, h, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}
	items := decodeListStatuses(t, rec.Body.Bytes())
	if items["web-01.example.com"] != "disconnected" {
		t.Errorf("web-01 status = %q, want disconnected (lease dead)", items["web-01.example.com"])
	}
}

// TestSoulList_LifecycleStatusesNotOverlaid — pending/revoked/expired/destroyed —
// are NOT presence snapshots: they describe onboarding/terminal state, not online/offline.
// The overlay leaves them alone (doesn't ask about their lease at all) and never flips them.
func TestSoulList_LifecycleStatusesNotOverlaid(t *testing.T) {
	now := time.Now().UTC()
	pool := &fakeSoulPool{
		listCount: 4,
		listSouls: []*soul.Soul{
			{SID: "p-01.example.com", Transport: soul.TransportAgent, Status: soul.StatusPending, RegisteredAt: now},
			{SID: "r-01.example.com", Transport: soul.TransportAgent, Status: soul.StatusRevoked, RegisteredAt: now},
			{SID: "e-01.example.com", Transport: soul.TransportAgent, Status: soul.StatusExpired, RegisteredAt: now},
			{SID: "d-01.example.com", Transport: soul.TransportAgent, Status: soul.StatusDestroyed, RegisteredAt: now},
		},
	}
	// Even if the lease "turned out to be alive" for these SIDs — the overlay skips them.
	presence := &fakePresence{alive: aliveSet("p-01.example.com", "r-01.example.com", "e-01.example.com", "d-01.example.com")}
	h := NewSoulHandler(pool, fakeScoper{unrestricted: true}, presence, nil)

	rec := doList(t, h, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}
	items := decodeListStatuses(t, rec.Body.Bytes())
	want := map[string]string{
		"p-01.example.com": "pending",
		"r-01.example.com": "revoked",
		"e-01.example.com": "expired",
		"d-01.example.com": "destroyed",
	}
	for sid, st := range want {
		if items[sid] != st {
			t.Errorf("%s status = %q, want %q (lifecycle - not presence)", sid, items[sid], st)
		}
	}
	// The lease is NOT queried for any lifecycle SID (the overlay filters before the call).
	if len(presence.gotSIDs) != 0 {
		t.Errorf("presence.gotSIDs = %v, want [] (lifecycle statuses do not go into the lease check)", presence.gotSIDs)
	}
}

// TestSoulList_PresenceCheckScopedToSnapshotStatuses — only connected/disconnected
// candidates go into the lease check; lifecycle SIDs are filtered out.
func TestSoulList_PresenceCheckScopedToSnapshotStatuses(t *testing.T) {
	now := time.Now().UTC()
	pool := &fakeSoulPool{
		listCount: 3,
		listSouls: []*soul.Soul{
			{SID: "c-01.example.com", Transport: soul.TransportAgent, Status: soul.StatusConnected, RegisteredAt: now},
			{SID: "x-01.example.com", Transport: soul.TransportAgent, Status: soul.StatusDisconnected, RegisteredAt: now},
			{SID: "p-01.example.com", Transport: soul.TransportAgent, Status: soul.StatusPending, RegisteredAt: now},
		},
	}
	presence := &fakePresence{alive: aliveSet("c-01.example.com")}
	h := NewSoulHandler(pool, fakeScoper{unrestricted: true}, presence, nil)

	if rec := doList(t, h, ""); rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}
	got := aliveSet(presence.gotSIDs...)
	if _, ok := got["c-01.example.com"]; !ok {
		t.Errorf("connected-SID did not make it into the lease check: %v", presence.gotSIDs)
	}
	if _, ok := got["x-01.example.com"]; !ok {
		t.Errorf("disconnected-SID did not make it into the lease check: %v", presence.gotSIDs)
	}
	if _, ok := got["p-01.example.com"]; ok {
		t.Errorf("pending-SID leaked into the lease check: %v", presence.gotSIDs)
	}
}

// TestSoulList_PresenceFailSafe — a Redis outage doesn't corrupt the list: the handler
// degrades to the PG snapshot status as-is (fail-safe), without a 500.
func TestSoulList_PresenceFailSafe(t *testing.T) {
	pool := &fakeSoulPool{
		listCount: 1,
		listSouls: []*soul.Soul{
			{SID: "redis-01.example.com", Transport: soul.TransportAgent, Status: soul.StatusDisconnected, RegisteredAt: time.Now().UTC()},
		},
	}
	presence := &fakePresence{err: errors.New("redis down")}
	h := NewSoulHandler(pool, fakeScoper{unrestricted: true}, presence, nil)

	rec := doList(t, h, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (fail-safe, not 500); body=%s", rec.Code, rec.Body.String())
	}
	items := decodeListStatuses(t, rec.Body.Bytes())
	if items["redis-01.example.com"] != "disconnected" {
		t.Errorf("redis-01 status = %q, want disconnected (PG snapshot, Redis failure)", items["redis-01.example.com"])
	}
}

// TestSoulList_NilPresence_SnapshotPassthrough — without presence (single-instance
// dev / unit without Redis) the overlay is disabled: the PG snapshot status is returned as-is.
func TestSoulList_NilPresence_SnapshotPassthrough(t *testing.T) {
	pool := &fakeSoulPool{
		listCount: 1,
		listSouls: []*soul.Soul{
			{SID: "redis-01.example.com", Transport: soul.TransportAgent, Status: soul.StatusDisconnected, RegisteredAt: time.Now().UTC()},
		},
	}
	h := NewSoulHandler(pool, fakeScoper{unrestricted: true}, nil, nil) // presence=nil.

	rec := doList(t, h, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}
	items := decodeListStatuses(t, rec.Body.Bytes())
	if items["redis-01.example.com"] != "disconnected" {
		t.Errorf("redis-01 status = %q, want disconnected (presence=nil -> PG snapshot)", items["redis-01.example.com"])
	}
}

// TestGetSoul_Reconnect_LeaseFlipsToConnected — overlay in GET /v1/souls/{sid}:
// PG snapshot disconnected + a live lease → connected (the same live bug, single-soul).
func TestGetSoul_Reconnect_LeaseFlipsToConnected(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	pool := &fakeReadPool{soul: &soul.Soul{
		SID:          "soul.example.com",
		Transport:    soul.TransportAgent,
		Status:       soul.StatusDisconnected,
		Coven:        []string{"dev"},
		RegisteredAt: now,
	}}
	presence := &fakePresence{alive: aliveSet("soul.example.com")}
	h := NewSoulHandler(pool, fakeScoper{unrestricted: true}, presence, nil)

	rec := doGetSoulScoped(t, h, "soul.example.com", "archon-alice")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}
	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out["status"] != "connected" {
		t.Errorf("status = %v, want connected (live lease, reconnect)", out["status"])
	}
}

// decodeListStatuses parses /v1/souls items into a map sid→status.
func decodeListStatuses(t *testing.T, body []byte) map[string]string {
	t.Helper()
	var out struct {
		Items []struct {
			SID    string `json:"sid"`
			Status string `json:"status"`
		} `json:"items"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	m := make(map[string]string, len(out.Items))
	for _, it := range out.Items {
		m[it.SID] = it.Status
	}
	return m
}
