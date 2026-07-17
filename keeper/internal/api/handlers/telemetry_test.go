package handlers

// Guard-тесты host-vitals read-path (NIM-86). Инварианты: stale/missing (ok=false
// → graceful, не 500), stale по возрасту, честная свежесть, RBAC (вне scope /
// not-found → 404 как soulprint), агрегат (пустой флот → hosts:[], multi-host
// маппинг). Реюз fakeSoulPool/fakeScoper из soul_test.go.

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/souls-guild/soul-stack/keeper/internal/api/problem"
	keeperredis "github.com/souls-guild/soul-stack/keeper/internal/redis"
	"github.com/souls-guild/soul-stack/keeper/internal/soul"
)

// fakeUtilReader — стенд [UtilizationReader]. Одиночный режим (snap/ok/err на любой
// sid) + per-sid overrides для агрегата.
type fakeUtilReader struct {
	snap      keeperredis.UtilizationSnapshot
	ok        bool
	err       error
	window    []keeperredis.UtilizationPoint
	windowErr error
	perSID    map[string]utilResult
}

type utilResult struct {
	snap keeperredis.UtilizationSnapshot
	ok   bool
	err  error
}

func (f fakeUtilReader) ReadUtilization(_ context.Context, sid string) (keeperredis.UtilizationSnapshot, bool, error) {
	if f.perSID != nil {
		if r, ok := f.perSID[sid]; ok {
			return r.snap, r.ok, r.err
		}
		return keeperredis.UtilizationSnapshot{}, false, nil
	}
	return f.snap, f.ok, f.err
}

func (f fakeUtilReader) ReadUtilizationWindow(_ context.Context, _ string, _ int) ([]keeperredis.UtilizationPoint, error) {
	return f.window, f.windowErr
}

func telemetrySoul(sid string, covens ...string) *soul.Soul {
	return &soul.Soul{
		SID: sid, Transport: soul.TransportAgent, Status: soul.StatusPending,
		Coven: covens, RegisteredAt: time.Now().UTC(),
	}
}

func newTelemetryHandler(pool *fakeSoulPool, scoper fakeScoper, reader UtilizationReader) *TelemetryHandler {
	return NewTelemetryHandler(reader, NewSoulHandler(pool, scoper, nil, nil), nil)
}

func assertTelemetryProblemErr(t *testing.T, err error, want string) {
	t.Helper()
	if err == nil {
		t.Fatalf("ждали problem %s, получили nil", want)
	}
	d, ok := AsProblemDetails(err)
	if !ok {
		t.Fatalf("err не *problemError: %v", err)
	}
	if d.Type != want {
		t.Errorf("problem type=%q, want %q", d.Type, want)
	}
}

func TestGetTelemetry_Missing_Stale(t *testing.T) {
	th := newTelemetryHandler(
		&fakeSoulPool{existingSoul: telemetrySoul("host-1.example.com", "web")},
		fakeScoper{unrestricted: true},
		fakeUtilReader{ok: false},
	)
	reply, err := th.GetTelemetry(context.Background(), claimsFor("archon-alice"), "host-1.example.com")
	if err != nil {
		t.Fatalf("ok=false → graceful, не ошибка: %v", err)
	}
	if !reply.Stale {
		t.Error("ok=false → Stale=true")
	}
	if reply.Latest != nil {
		t.Error("ok=false → Latest=nil")
	}
	if len(reply.Window) != 0 {
		t.Error("ok=false → Window пуст")
	}
	if reply.SID != "host-1.example.com" {
		t.Errorf("SID=%q", reply.SID)
	}
}

func TestGetTelemetry_ReaderError_GracefulStale(t *testing.T) {
	th := newTelemetryHandler(
		&fakeSoulPool{existingSoul: telemetrySoul("host-1.example.com", "web")},
		fakeScoper{unrestricted: true},
		fakeUtilReader{err: errors.New("redis down")},
	)
	reply, err := th.GetTelemetry(context.Background(), claimsFor("archon-alice"), "host-1.example.com")
	if err != nil {
		t.Fatalf("ошибка ридера → деградация в stale, не 500: %v", err)
	}
	if !reply.Stale || reply.Latest != nil {
		t.Error("ошибка ридера → Stale=true, Latest=nil")
	}
}

func TestGetTelemetry_StaleByAge(t *testing.T) {
	old := time.Now().Add(-2 * keeperredis.UtilizationTTL).UTC()
	th := newTelemetryHandler(
		&fakeSoulPool{existingSoul: telemetrySoul("host-1.example.com", "web")},
		fakeScoper{unrestricted: true},
		fakeUtilReader{ok: true, snap: keeperredis.UtilizationSnapshot{ReceivedAt: old, CollectedAt: old, CPUPct: 10}},
	)
	reply, err := th.GetTelemetry(context.Background(), claimsFor("archon-alice"), "host-1.example.com")
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if !reply.Stale {
		t.Error("возраст > TTL → Stale=true")
	}
	if reply.Latest == nil || reply.Latest.CpuPct != 10 {
		t.Errorf("данные есть (пусть старые) → Latest проброшен: %+v", reply.Latest)
	}
}

func TestGetTelemetry_Fresh(t *testing.T) {
	now := time.Now().UTC()
	snap := keeperredis.UtilizationSnapshot{
		CollectedAt: now, ReceivedAt: now,
		CPUPct: 12.5, Load1: 0.5, Load5: 0.4, Load15: 0.3,
		MemUsedMB: 1024, MemTotalMB: 2048, SwapUsedMB: 0, UptimeSec: 3600,
		Disks: []keeperredis.DiskUsage{{Mount: "/", UsedMB: 10, TotalMB: 100}},
	}
	win := []keeperredis.UtilizationPoint{{CollectedAt: now, CPUPct: 12.5, Load1: 0.5, MemUsedMB: 1024, MemTotalMB: 2048}}
	th := newTelemetryHandler(
		&fakeSoulPool{existingSoul: telemetrySoul("host-1.example.com", "web")},
		fakeScoper{unrestricted: true},
		fakeUtilReader{ok: true, snap: snap, window: win},
	)
	reply, err := th.GetTelemetry(context.Background(), claimsFor("archon-alice"), "host-1.example.com")
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if reply.Stale {
		t.Error("свежий снимок → Stale=false")
	}
	if reply.Latest == nil {
		t.Fatal("Latest nil")
	}
	if reply.Latest.CpuPct != 12.5 || reply.Latest.MemUsedMb != 1024 || reply.Latest.UptimeSec != 3600 {
		t.Errorf("поля latest не проброшены: %+v", reply.Latest)
	}
	if len(reply.Latest.Disks) != 1 || reply.Latest.Disks[0].Mount != "/" || reply.Latest.Disks[0].UsedMb != 10 {
		t.Errorf("disks не проброшены: %+v", reply.Latest.Disks)
	}
	if reply.CollectedAt == nil || reply.ReceivedAt == nil {
		t.Error("collected_at/received_at должны присутствовать")
	}
	if len(reply.Window) != 1 || reply.Window[0].CpuPct != 12.5 {
		t.Errorf("window не проброшено: %+v", reply.Window)
	}
}

func TestGetTelemetry_BadSID_422(t *testing.T) {
	th := newTelemetryHandler(&fakeSoulPool{}, fakeScoper{unrestricted: true}, fakeUtilReader{})
	_, err := th.GetTelemetry(context.Background(), claimsFor("archon-alice"), "BAD_SID")
	assertTelemetryProblemErr(t, err, problem.TypeValidationFailed)
}

func TestGetTelemetry_NotFound_404(t *testing.T) {
	th := newTelemetryHandler(&fakeSoulPool{existingSoul: nil}, fakeScoper{unrestricted: true}, fakeUtilReader{})
	_, err := th.GetTelemetry(context.Background(), claimsFor("archon-alice"), "host-1.example.com")
	assertTelemetryProblemErr(t, err, problem.TypeNotFound)
}

func TestGetTelemetry_OutOfScope_404(t *testing.T) {
	// host в ковене prod, scope оператора — только dev → InScope false → 404
	// (как soulprint: не палим существование чужого хоста).
	th := newTelemetryHandler(
		&fakeSoulPool{existingSoul: telemetrySoul("host-1.example.com", "prod")},
		fakeScoper{covens: []string{"dev"}},
		fakeUtilReader{ok: true, snap: keeperredis.UtilizationSnapshot{ReceivedAt: time.Now()}},
	)
	_, err := th.GetTelemetry(context.Background(), claimsFor("archon-alice"), "host-1.example.com")
	assertTelemetryProblemErr(t, err, problem.TypeNotFound)
}

func TestAggregate_Empty_HostsSlice(t *testing.T) {
	th := newTelemetryHandler(&fakeSoulPool{listSouls: nil}, fakeScoper{unrestricted: true}, fakeUtilReader{})
	reply, err := th.AggregateByIncarnation(context.Background(), claimsFor("archon-alice"), "redis-prod")
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if reply.Incarnation != "redis-prod" {
		t.Errorf("incarnation=%q", reply.Incarnation)
	}
	if reply.Hosts == nil {
		t.Error("Hosts non-nil ([] не null)")
	}
	if len(reply.Hosts) != 0 {
		t.Errorf("hosts len=%d, want 0", len(reply.Hosts))
	}
}

func TestAggregate_MultiHost(t *testing.T) {
	now := time.Now().UTC()
	old := now.Add(-2 * keeperredis.UtilizationTTL)
	pool := &fakeSoulPool{listSouls: []*soul.Soul{
		telemetrySoul("h1.example.com", "redis-prod"),
		telemetrySoul("h2.example.com", "redis-prod"),
		telemetrySoul("h3.example.com", "redis-prod"),
	}}
	reader := fakeUtilReader{perSID: map[string]utilResult{
		"h1.example.com": {ok: true, snap: keeperredis.UtilizationSnapshot{ReceivedAt: now, CollectedAt: now, CPUPct: 5}},
		"h2.example.com": {ok: true, snap: keeperredis.UtilizationSnapshot{ReceivedAt: old, CollectedAt: old, CPUPct: 9}},
		"h3.example.com": {ok: false},
	}}
	th := newTelemetryHandler(pool, fakeScoper{unrestricted: true}, reader)
	reply, err := th.AggregateByIncarnation(context.Background(), claimsFor("archon-alice"), "redis-prod")
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if len(reply.Hosts) != 3 {
		t.Fatalf("hosts=%d, want 3", len(reply.Hosts))
	}
	byID := map[string]HostTelemetry{}
	for _, h := range reply.Hosts {
		byID[h.SID] = h
	}
	if byID["h1.example.com"].Stale || byID["h1.example.com"].Latest == nil {
		t.Error("h1 свежий → Stale=false, Latest есть")
	}
	if !byID["h2.example.com"].Stale || byID["h2.example.com"].Latest == nil {
		t.Error("h2 stale-by-age → Stale=true, но Latest (старый) есть")
	}
	if !byID["h3.example.com"].Stale || byID["h3.example.com"].Latest != nil {
		t.Error("h3 нет данных → Stale=true, Latest=nil")
	}
}

func TestAggregate_OutOfScope_Empty(t *testing.T) {
	pool := &fakeSoulPool{listSouls: []*soul.Soul{telemetrySoul("h1.example.com", "redis-prod")}}
	th := newTelemetryHandler(pool, fakeScoper{empty: true}, fakeUtilReader{})
	reply, err := th.AggregateByIncarnation(context.Background(), claimsFor("archon-alice"), "redis-prod")
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if len(reply.Hosts) != 0 {
		t.Errorf("empty scope → hosts:[], got %d", len(reply.Hosts))
	}
}

// TestGetTelemetry_StaleBoundary — граница свежести вокруг TTL (тикет: тест на
// границу возраста). received = now-(TTL∓1s).
func TestGetTelemetry_StaleBoundary(t *testing.T) {
	now := time.Now()
	cases := []struct {
		name  string
		recv  time.Time
		stale bool
	}{
		{"свежий_TTL-1s", now.Add(-(keeperredis.UtilizationTTL - time.Second)), false},
		{"протух_TTL+1s", now.Add(-(keeperredis.UtilizationTTL + time.Second)), true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			th := newTelemetryHandler(
				&fakeSoulPool{existingSoul: telemetrySoul("host-1.example.com", "web")},
				fakeScoper{unrestricted: true},
				fakeUtilReader{ok: true, snap: keeperredis.UtilizationSnapshot{ReceivedAt: c.recv, CollectedAt: c.recv, CPUPct: 3}},
			)
			reply, err := th.GetTelemetry(context.Background(), claimsFor("archon-alice"), "host-1.example.com")
			if err != nil {
				t.Fatalf("unexpected err: %v", err)
			}
			if reply.Stale != c.stale {
				t.Errorf("Stale=%v, want %v", reply.Stale, c.stale)
			}
		})
	}
}

// TestAggregate_HostReadError_StaleNotDropped — ошибка чтения одного хоста не
// роняет агрегат: хост остаётся в списке как stale, Latest=nil.
func TestAggregate_HostReadError_StaleNotDropped(t *testing.T) {
	now := time.Now().UTC()
	pool := &fakeSoulPool{listSouls: []*soul.Soul{
		telemetrySoul("h1.example.com", "redis-prod"),
		telemetrySoul("h2.example.com", "redis-prod"),
	}}
	reader := fakeUtilReader{perSID: map[string]utilResult{
		"h1.example.com": {ok: true, snap: keeperredis.UtilizationSnapshot{ReceivedAt: now, CollectedAt: now, CPUPct: 5}},
		"h2.example.com": {err: errors.New("redis read failed")},
	}}
	th := newTelemetryHandler(pool, fakeScoper{unrestricted: true}, reader)
	reply, err := th.AggregateByIncarnation(context.Background(), claimsFor("archon-alice"), "redis-prod")
	if err != nil {
		t.Fatalf("ошибка одного хоста не роняет агрегат: %v", err)
	}
	if len(reply.Hosts) != 2 {
		t.Fatalf("hosts=%d, want 2 (хост с ошибкой не выпадает)", len(reply.Hosts))
	}
	byID := map[string]HostTelemetry{}
	for _, h := range reply.Hosts {
		byID[h.SID] = h
	}
	if !byID["h2.example.com"].Stale || byID["h2.example.com"].Latest != nil {
		t.Error("h2 ошибка чтения → Stale=true, Latest=nil, но хост в списке")
	}
	if byID["h1.example.com"].Stale {
		t.Error("h1 свежий → Stale=false")
	}
}

// TestSIDsInCovenInScope_TruncatedFlag — cap усекает список, флаг truncated не
// «молчит»: len(флот) >= cap → true, cap > флот → false.
func TestSIDsInCovenInScope_TruncatedFlag(t *testing.T) {
	pool := &fakeSoulPool{listSouls: []*soul.Soul{
		telemetrySoul("h1.example.com", "redis-prod"),
		telemetrySoul("h2.example.com", "redis-prod"),
		telemetrySoul("h3.example.com", "redis-prod"),
	}}
	sh := NewSoulHandler(pool, fakeScoper{unrestricted: true}, nil, nil)
	sids, truncated, err := sh.SIDsInCovenInScope(context.Background(), claimsFor("archon-alice"), "redis-prod", 3)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if !truncated {
		t.Error("len(флот) >= cap → truncated=true")
	}
	if len(sids) != 3 {
		t.Errorf("sids=%d, want 3", len(sids))
	}
	if _, truncated2, err := sh.SIDsInCovenInScope(context.Background(), claimsFor("archon-alice"), "redis-prod", 4); err != nil {
		t.Fatalf("unexpected err: %v", err)
	} else if truncated2 {
		t.Error("cap > флот → truncated=false")
	}
}
