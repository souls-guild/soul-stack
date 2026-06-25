package soul_test

import (
	"context"
	"errors"
	"reflect"
	"sort"
	"sync"
	"testing"

	"github.com/souls-guild/soul-stack/keeper/internal/coremod/internaltest"
	coremodsoul "github.com/souls-guild/soul-stack/keeper/internal/coremod/soul"

	pluginv1 "github.com/souls-guild/soul-stack/proto/plugin/gen/go/v1"
)

// fakePresence — детерминированный PresenceChecker для guard-тестов барьера
// онбординга. online — множество SID, которые «уже online»; aliveAfter —
// число опросов, после которого SID начинает считаться online (модель
// постепенного онбординга). err — инъекция Redis-сбоя.
type fakePresence struct {
	mu         sync.Mutex
	online     map[string]struct{}
	aliveAfter map[string]int // sid → сколько SoulsStreamAlive-вызовов до online
	calls      int
	err        error
}

func newFakePresence() *fakePresence {
	return &fakePresence{online: map[string]struct{}{}, aliveAfter: map[string]int{}}
}

func (f *fakePresence) SoulsStreamAlive(_ context.Context, sids []string) (map[string]struct{}, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return nil, f.err
	}
	f.calls++
	res := map[string]struct{}{}
	for _, sid := range sids {
		if _, ok := f.online[sid]; ok {
			res[sid] = struct{}{}
			continue
		}
		if after, ok := f.aliveAfter[sid]; ok && f.calls >= after {
			res[sid] = struct{}{}
		}
	}
	return res, nil
}

// newAwaitModule — модуль с presence-checker и тестовым потолком timeout.
func newAwaitModule(t *testing.T, fs coremodsoul.Store, p coremodsoul.PresenceChecker, maxTimeout string) *coremodsoul.Module {
	t.Helper()
	return coremodsoul.New(fs).WithPresence(p, func() string { return maxTimeout })
}

func sortedOut(out map[string]any, key string) []string {
	raw, _ := out[key].([]any)
	xs := make([]string, 0, len(raw))
	for _, v := range raw {
		if s, ok := v.(string); ok {
			xs = append(xs, s)
		}
	}
	sort.Strings(xs)
	return xs
}

// TestAwait_WaitsUntilOnline — барьер блокирует и завершается успехом, как
// только все регистрируемые SID становятся online (источник — presence-poll).
func TestAwait_WaitsUntilOnline(t *testing.T) {
	fs := newFakeStore()
	p := newFakePresence()
	// h1 online со 2-го опроса, h2 — с 3-го (постепенный онбординг).
	p.aliveAfter["h1.example.com"] = 2
	p.aliveAfter["h2.example.com"] = 3

	m := newAwaitModule(t, fs, p, "")
	stream := internaltest.NewApplyStream()
	err := m.Apply(&pluginv1.ApplyRequest{
		State: "registered",
		Params: mustStruct(t, map[string]any{
			"sid":                 []any{"h1.example.com", "h2.example.com"},
			"coven":               []any{"redis", "prod"},
			"await_online":        true,
			"await_timeout":       "5s",
			"await_poll_interval": "1ms",
		}),
	}, stream)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	ev := stream.Last()
	if ev == nil || ev.Failed {
		t.Fatalf("expected success, got %+v", ev)
	}
	out := ev.Output.AsMap()
	if out["satisfied"] != true {
		t.Errorf("satisfied=%v, want true", out["satisfied"])
	}
	if got := sortedOut(out, "online"); !reflect.DeepEqual(got, []string{"h1.example.com", "h2.example.com"}) {
		t.Errorf("online=%v, want both hosts", got)
	}
	if got := sortedOut(out, "pending"); len(got) != 0 {
		t.Errorf("pending=%v, want empty", got)
	}
	if p.calls < 3 {
		t.Errorf("expected at least 3 presence polls (gradual onboarding), got %d", p.calls)
	}
}

// TestAwait_B1Timeout_Failed — online < min_count к таймауту → шаг failed
// (B1-strict fail-stop). output несёт pending для диагностики.
func TestAwait_B1Timeout_Failed(t *testing.T) {
	fs := newFakeStore()
	p := newFakePresence()
	p.online["h1.example.com"] = struct{}{} // только h1 поднялся, h2 — никогда

	m := newAwaitModule(t, fs, p, "")
	stream := internaltest.NewApplyStream()
	if err := m.Apply(&pluginv1.ApplyRequest{
		State: "registered",
		Params: mustStruct(t, map[string]any{
			"sid":                 []any{"h1.example.com", "h2.example.com"},
			"coven":               []any{"redis"},
			"await_online":        true,
			"await_timeout":       "30ms",
			"await_poll_interval": "1ms",
		}),
	}, stream); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	ev := stream.Last()
	if !ev.Failed {
		t.Fatalf("expected failed=true on B1 timeout, got %+v", ev)
	}
	// Диагностика в message: сколько онлайн / сколько ждали.
	if ev.Message == "" {
		t.Error("expected diagnostic message on B1 timeout")
	}
}

// TestAwait_MinCountSatisfied_OK — кворум `await_min_count` достигнут раньше,
// чем все хосты online → успех, не ждём остальных.
func TestAwait_MinCountSatisfied_OK(t *testing.T) {
	fs := newFakeStore()
	p := newFakePresence()
	p.online["h1.example.com"] = struct{}{}
	p.online["h2.example.com"] = struct{}{} // h3 не поднимется — и не нужен

	m := newAwaitModule(t, fs, p, "")
	stream := internaltest.NewApplyStream()
	if err := m.Apply(&pluginv1.ApplyRequest{
		State: "registered",
		Params: mustStruct(t, map[string]any{
			"sid":                 []any{"h1.example.com", "h2.example.com", "h3.example.com"},
			"coven":               []any{"redis"},
			"await_online":        true,
			"await_timeout":       "5s",
			"await_min_count":     2,
			"await_poll_interval": "1ms",
		}),
	}, stream); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	ev := stream.Last()
	if ev.Failed {
		t.Fatalf("expected success at min_count=2, got %+v", ev)
	}
	out := ev.Output.AsMap()
	if out["satisfied"] != true {
		t.Errorf("satisfied=%v, want true", out["satisfied"])
	}
	if got := sortedOut(out, "online"); len(got) < 2 {
		t.Errorf("online=%v, want >= 2", got)
	}
}

// TestAwait_SourceIsLease_NotPGStatus — presence решает Redis-lease-checker, а
// НЕ PG souls.status. SID существует в Store со status=connected, но lease его
// НЕ видит → барьер не считает хост online → B1 timeout.
func TestAwait_SourceIsLease_NotPGStatus(t *testing.T) {
	fs := newFakeStore()
	// PG говорит «connected», но presence-checker (lease) его не вернёт.
	p := newFakePresence() // online пуст — lease ничего не видит

	m := newAwaitModule(t, fs, p, "")
	stream := internaltest.NewApplyStream()
	if err := m.Apply(&pluginv1.ApplyRequest{
		State: "registered",
		Params: mustStruct(t, map[string]any{
			"sid":                 "h1.example.com",
			"coven":               []any{"redis"},
			"await_online":        true,
			"await_timeout":       "20ms",
			"await_poll_interval": "1ms",
		}),
	}, stream); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if !stream.Last().Failed {
		t.Fatal("expected failed: lease (not PG status) is the online source; lease saw nothing")
	}
	if p.calls == 0 {
		t.Error("expected presence-checker (lease) to be polled — it is the source of truth")
	}
}

// TestAwait_TimeoutCeiling_Failed — await_timeout > потолка keeper.yml
// (max_await_timeout) → шаг failed (fail-closed DoS-guard, НЕ тихое обрезание).
func TestAwait_TimeoutCeiling_Failed(t *testing.T) {
	fs := newFakeStore()
	p := newFakePresence()
	p.online["h1.example.com"] = struct{}{} // хост online — failure только из-за потолка

	m := newAwaitModule(t, fs, p, "1s") // потолок 1s
	stream := internaltest.NewApplyStream()
	if err := m.Apply(&pluginv1.ApplyRequest{
		State: "registered",
		Params: mustStruct(t, map[string]any{
			"sid":           "h1.example.com",
			"coven":         []any{"redis"},
			"await_online":  true,
			"await_timeout": "2h", // превышает потолок 1s
		}),
	}, stream); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	ev := stream.Last()
	if !ev.Failed {
		t.Fatalf("expected failed: await_timeout exceeds max_await_timeout ceiling, got %+v", ev)
	}
	if p.calls != 0 {
		t.Error("ceiling check must reject before any polling")
	}
}

// TestAwait_RequiredTimeout — await_online: true без await_timeout → ошибка
// валидации (барьер не должен висеть вечно).
func TestAwait_RequiredTimeout(t *testing.T) {
	fs := newFakeStore()
	p := newFakePresence()
	m := newAwaitModule(t, fs, p, "")
	stream := internaltest.NewApplyStream()
	if err := m.Apply(&pluginv1.ApplyRequest{
		State: "registered",
		Params: mustStruct(t, map[string]any{
			"sid":          "h1.example.com",
			"coven":        []any{"redis"},
			"await_online": true,
		}),
	}, stream); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if !stream.Last().Failed {
		t.Fatal("expected failed: await_online requires await_timeout")
	}
}

// TestAwait_NoAwait_RegistersWithoutBlocking — без await_online шаг ведёт себя
// как до ADR-061 (регистрация без барьера), presence-checker не дёргается.
func TestAwait_NoAwait_RegistersWithoutBlocking(t *testing.T) {
	fs := newFakeStore()
	p := newFakePresence()
	m := newAwaitModule(t, fs, p, "")
	stream := internaltest.NewApplyStream()
	if err := m.Apply(&pluginv1.ApplyRequest{
		State: "registered",
		Params: mustStruct(t, map[string]any{
			"sid":   []any{"h1.example.com", "h2.example.com"},
			"coven": []any{"redis"},
		}),
	}, stream); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if stream.Last().Failed {
		t.Fatalf("expected success without await, got %+v", stream.Last())
	}
	if p.calls != 0 {
		t.Errorf("presence must not be polled without await_online, got %d calls", p.calls)
	}
	// Оба SID зарегистрированы (list-форма).
	if fs.insertCalls != 2 {
		t.Errorf("expected 2 inserts for 2-SID list, got %d", fs.insertCalls)
	}
}

// TestAwait_ListSID_RegistersAll — list-форма sid регистрирует все хосты с
// общим набором coven, барьер агрегирует presence по всем.
func TestAwait_ListSID_RegistersAll(t *testing.T) {
	fs := newFakeStore()
	p := newFakePresence()
	p.online["a.example.com"] = struct{}{}
	p.online["b.example.com"] = struct{}{}
	p.online["c.example.com"] = struct{}{}

	m := newAwaitModule(t, fs, p, "")
	stream := internaltest.NewApplyStream()
	if err := m.Apply(&pluginv1.ApplyRequest{
		State: "registered",
		Params: mustStruct(t, map[string]any{
			"sid":                 []any{"a.example.com", "b.example.com", "c.example.com"},
			"coven":               []any{"redis", "shard-1"},
			"await_online":        true,
			"await_timeout":       "5s",
			"await_poll_interval": "1ms",
		}),
	}, stream); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	ev := stream.Last()
	if ev.Failed {
		t.Fatalf("expected success, got %+v", ev)
	}
	if fs.insertCalls != 3 {
		t.Errorf("expected 3 inserts, got %d", fs.insertCalls)
	}
	// coven применён ко всем (последний UpdateCoven — общий набор).
	got := append([]string(nil), fs.lastCoven...)
	sort.Strings(got)
	if !reflect.DeepEqual(got, []string{"redis", "shard-1"}) {
		t.Errorf("coven=%v, want [redis shard-1] applied to all", got)
	}
}

// TestAwait_PresenceError_Failed — ошибка Redis-проверки во время барьера →
// шаг failed (presence-источник недоступен, B1-strict не может подтвердить
// кворум → fail, не молчаливый success).
func TestAwait_PresenceError_Failed(t *testing.T) {
	fs := newFakeStore()
	p := newFakePresence()
	p.err = errors.New("redis down")

	m := newAwaitModule(t, fs, p, "")
	stream := internaltest.NewApplyStream()
	if err := m.Apply(&pluginv1.ApplyRequest{
		State: "registered",
		Params: mustStruct(t, map[string]any{
			"sid":                 "h1.example.com",
			"coven":               []any{"redis"},
			"await_online":        true,
			"await_timeout":       "30ms",
			"await_poll_interval": "1ms",
		}),
	}, stream); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if !stream.Last().Failed {
		t.Fatal("expected failed on persistent presence error")
	}
}

// TestAwait_NoChecker_Failed — await_online запрошен, но presence-checker не
// сконфигурирован (nil) → failed (барьер не может работать без источника
// presence; молчаливый success недопустим).
func TestAwait_NoChecker_Failed(t *testing.T) {
	fs := newFakeStore()
	m := coremodsoul.New(fs) // без WithPresence
	stream := internaltest.NewApplyStream()
	if err := m.Apply(&pluginv1.ApplyRequest{
		State: "registered",
		Params: mustStruct(t, map[string]any{
			"sid":           "h1.example.com",
			"coven":         []any{"redis"},
			"await_online":  true,
			"await_timeout": "5s",
		}),
	}, stream); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if !stream.Last().Failed {
		t.Fatal("expected failed: await_online without presence-checker configured")
	}
}

// TestAwait_MinCountTooHigh_Validation — await_min_count > числа SID →
// валидационная ошибка (недостижимый кворум).
func TestAwait_MinCountTooHigh_Validation(t *testing.T) {
	fs := newFakeStore()
	p := newFakePresence()
	m := newAwaitModule(t, fs, p, "")
	stream := internaltest.NewApplyStream()
	if err := m.Apply(&pluginv1.ApplyRequest{
		State: "registered",
		Params: mustStruct(t, map[string]any{
			"sid":             []any{"h1.example.com", "h2.example.com"},
			"coven":           []any{"redis"},
			"await_online":    true,
			"await_timeout":   "5s",
			"await_min_count": 5, // больше, чем 2 SID
		}),
	}, stream); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if !stream.Last().Failed {
		t.Fatal("expected failed: await_min_count exceeds number of SIDs")
	}
}
