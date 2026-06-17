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

// fakePresence — мок [SoulPresence] для presence-overlay-тестов `GET /v1/souls`.
// alive — множество SID-ов с «живым» Redis SID-lease (online). err != nil →
// эмулирует Redis-сбой (handler деградирует fail-safe на PG-снимок). gotSIDs —
// захват аргумента для проверки, что overlay спрашивает lease ТОЛЬКО про
// presence-снимковые статусы (connected/disconnected), не про lifecycle.
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

// TestSoulList_Reconnect_LeaseFlipsToConnected — корневой live-баг (ADR-006(a)):
// после reaper `mark_disconnected` PG-снимок `souls.status` = disconnected, но
// Soul переподключился (живой Redis SID-lease). До фикса /v1/souls отдавал stale
// disconnected; теперь overlay деривирует status из lease → connected, без
// рестарта/ручных действий и без ожидания следующего тика reaper-а.
func TestSoulList_Reconnect_LeaseFlipsToConnected(t *testing.T) {
	pool := &fakeSoulPool{
		listCount: 2,
		listSouls: []*soul.Soul{
			{SID: "redis-01.example.com", Transport: soul.TransportAgent, Status: soul.StatusDisconnected, RegisteredAt: time.Now().UTC()},
			{SID: "redis-02.example.com", Transport: soul.TransportAgent, Status: soul.StatusDisconnected, RegisteredAt: time.Now().UTC()},
		},
	}
	// redis-01 переподключился (lease жив), redis-02 реально оффлайн (lease мёртв).
	presence := &fakePresence{alive: aliveSet("redis-01.example.com")}
	h := NewSoulHandler(pool, fakeScoper{unrestricted: true}, presence, nil)

	rec := doList(t, h, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}
	items := decodeListStatuses(t, rec.Body.Bytes())
	if items["redis-01.example.com"] != "connected" {
		t.Errorf("redis-01 status = %q, want connected (live lease, реконнект)", items["redis-01.example.com"])
	}
	if items["redis-02.example.com"] != "disconnected" {
		t.Errorf("redis-02 status = %q, want disconnected (lease мёртв)", items["redis-02.example.com"])
	}
}

// TestSoulList_StaleConnectedSnapshot_LeaseFlipsToDisconnected — обратное
// направление: PG-снимок connected, но lease умер (стрим оборвался, reaper ещё
// не сверил). Overlay отдаёт фактический disconnected, не stale connected.
func TestSoulList_StaleConnectedSnapshot_LeaseFlipsToDisconnected(t *testing.T) {
	pool := &fakeSoulPool{
		listCount: 1,
		listSouls: []*soul.Soul{
			{SID: "web-01.example.com", Transport: soul.TransportAgent, Status: soul.StatusConnected, RegisteredAt: time.Now().UTC()},
		},
	}
	presence := &fakePresence{alive: aliveSet()} // lease мёртв.
	h := NewSoulHandler(pool, fakeScoper{unrestricted: true}, presence, nil)

	rec := doList(t, h, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}
	items := decodeListStatuses(t, rec.Body.Bytes())
	if items["web-01.example.com"] != "disconnected" {
		t.Errorf("web-01 status = %q, want disconnected (lease мёртв)", items["web-01.example.com"])
	}
}

// TestSoulList_LifecycleStatusesNotOverlaid — pending/revoked/expired/destroyed —
// НЕ presence-снимки: они описывают онбординг/терминал, не online/offline.
// Overlay их не трогает (не спрашивает про них lease вовсе) и не флипает.
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
	// Даже если бы lease «оказался жив» для этих SID — overlay их пропускает.
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
			t.Errorf("%s status = %q, want %q (lifecycle — не presence)", sid, items[sid], st)
		}
	}
	// Lease НЕ запрашивается ни про один lifecycle-SID (overlay фильтрует до вызова).
	if len(presence.gotSIDs) != 0 {
		t.Errorf("presence.gotSIDs = %v, want [] (lifecycle-статусы не идут в lease-проверку)", presence.gotSIDs)
	}
}

// TestSoulList_PresenceCheckScopedToSnapshotStatuses — в lease-проверку уходят
// ТОЛЬКО connected/disconnected-кандидаты; lifecycle-SID отфильтрованы.
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
		t.Errorf("connected-SID не попал в lease-проверку: %v", presence.gotSIDs)
	}
	if _, ok := got["x-01.example.com"]; !ok {
		t.Errorf("disconnected-SID не попал в lease-проверку: %v", presence.gotSIDs)
	}
	if _, ok := got["p-01.example.com"]; ok {
		t.Errorf("pending-SID просочился в lease-проверку: %v", presence.gotSIDs)
	}
}

// TestSoulList_PresenceFailSafe — Redis-сбой не искажает список: handler
// деградирует на PG-снимок status как есть (fail-safe), без 500.
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
		t.Fatalf("status = %d, want 200 (fail-safe, не 500); body=%s", rec.Code, rec.Body.String())
	}
	items := decodeListStatuses(t, rec.Body.Bytes())
	if items["redis-01.example.com"] != "disconnected" {
		t.Errorf("redis-01 status = %q, want disconnected (PG-снимок, Redis-сбой)", items["redis-01.example.com"])
	}
}

// TestSoulList_NilPresence_SnapshotPassthrough — без presence (single-instance
// dev / unit без Redis) overlay выключен: PG-снимок status отдаётся как есть.
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
		t.Errorf("redis-01 status = %q, want disconnected (presence=nil → PG-снимок)", items["redis-01.example.com"])
	}
}

// TestGetSoul_Reconnect_LeaseFlipsToConnected — overlay в GET /v1/souls/{sid}:
// PG-снимок disconnected + живой lease → connected (тот же live-баг, single-soul).
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
		t.Errorf("status = %v, want connected (live lease, реконнект)", out["status"])
	}
}

// decodeListStatuses парсит /v1/souls items в map sid→status.
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
