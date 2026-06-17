package errand

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/souls-guild/soul-stack/keeper/internal/applybus"
	keeperv1 "github.com/souls-guild/soul-stack/proto/gen/go/keeper/v1"
)

// fakeStore — in-memory эмулятор StoreAPI без PG. Все методы под mu для
// race-free доступа из background-горутины и тестового goroutine-а.
type fakeStore struct {
	mu      sync.Mutex
	rows    map[string]*Row
	inserts int
	marks   int
}

func newFakeStore() *fakeStore { return &fakeStore{rows: map[string]*Row{}} }

func (s *fakeStore) Insert(_ context.Context, row Row) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.rows[row.ErrandID]; ok {
		return errors.New("duplicate errand_id")
	}
	cp := row
	s.rows[row.ErrandID] = &cp
	s.inserts++
	return nil
}

func (s *fakeStore) MarkTerminal(_ context.Context, id string, upd TerminalUpdate) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.rows[id]
	if !ok || r.Status != StatusRunning {
		return false, nil
	}
	r.Status = upd.Status
	r.ExitCode = upd.ExitCode
	r.Stdout = upd.Stdout
	r.Stderr = upd.Stderr
	r.StdoutTruncated = upd.StdoutTruncated
	r.StderrTruncated = upd.StderrTruncated
	r.DurationMs = upd.DurationMs
	r.ErrorMessage = upd.ErrorMessage
	r.Output = upd.Output
	now := time.Now().UTC()
	r.FinishedAt = &now
	s.marks++
	return true, nil
}

func (s *fakeStore) SweepOrphanRunning(_ context.Context, kid string, grace time.Duration, reason string) ([]string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var ids []string
	cutoff := time.Now().Add(-grace)
	for _, r := range s.rows {
		if r.Status == StatusRunning && r.StartedByKID == kid && r.StartedAt.Before(cutoff) {
			r.Status = StatusTimedOut
			r.ErrorMessage = reason
			now := time.Now().UTC()
			r.FinishedAt = &now
			ids = append(ids, r.ErrandID)
		}
	}
	return ids, nil
}

func (s *fakeStore) Get(_ context.Context, id string) (*Row, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.rows[id]
	if !ok {
		return nil, ErrNotFound
	}
	cp := *r
	return &cp, nil
}

func (s *fakeStore) snapshot(id string) *Row {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.rows[id]
	if !ok {
		return nil
	}
	cp := *r
	return &cp
}

// fakeOutbound — учёт вызовов SendErrand/PublishErrand/SendCancelErrand/PublishCancelErrand.
type fakeOutbound struct {
	mu            sync.Mutex
	sent          []*keeperv1.ErrandRequest
	sendErr       error
	cancelled     []string // errand_id-ы по SendCancelErrand+PublishCancelErrand вместе.
	cancelSendErr error
}

func (o *fakeOutbound) SendErrand(_ context.Context, _ string, req *keeperv1.ErrandRequest) error {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.sendErr != nil {
		return o.sendErr
	}
	o.sent = append(o.sent, req)
	return nil
}

func (o *fakeOutbound) PublishErrand(ctx context.Context, sid string, req *keeperv1.ErrandRequest) error {
	return o.SendErrand(ctx, sid, req)
}

// firstSentErrandID — errand_id первого отправленного ErrandRequest (через
// SendErrand или PublishErrand). "" пока ничего не отправлено. Нужен guard-
// тестам S1: errand_id генерится внутри Dispatch (ULID), а замер
// Redis-Subscribe ведётся именно по этому id.
func (o *fakeOutbound) firstSentErrandID() string {
	o.mu.Lock()
	defer o.mu.Unlock()
	if len(o.sent) == 0 {
		return ""
	}
	return o.sent[0].GetErrandId()
}

func (o *fakeOutbound) SendCancelErrand(_ context.Context, _ string, errandID string) error {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.cancelSendErr != nil {
		return o.cancelSendErr
	}
	o.cancelled = append(o.cancelled, errandID)
	return nil
}

func (o *fakeOutbound) PublishCancelErrand(ctx context.Context, sid, errandID string) error {
	return o.SendCancelErrand(ctx, sid, errandID)
}

// fakeBus — in-memory pub/sub для одного applyID-канала. Фиксирует последнее
// wantBridge-решение dispatcher-а (lastWantBridge) — guard-тесты S1 проверяют
// holder→bridge маршрутизацию по нему.
type fakeBus struct {
	mu             sync.Mutex
	chs            map[string]chan applybus.Event
	lastWantBridge bool
	bridgeSet      bool // был ли вызван SubscribeWithBridge (а не legacy Subscribe)
}

func newFakeBus() *fakeBus { return &fakeBus{chs: map[string]chan applybus.Event{}} }

func (b *fakeBus) Subscribe(ctx context.Context, applyID string) <-chan applybus.Event {
	return b.SubscribeWithBridge(ctx, applyID, true)
}

func (b *fakeBus) SubscribeWithBridge(_ context.Context, applyID string, wantBridge bool) <-chan applybus.Event {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.lastWantBridge = wantBridge
	b.bridgeSet = true
	ch := make(chan applybus.Event, 8)
	b.chs[applyID] = ch
	return ch
}

// wantBridge возвращает последнее bridge-решение и факт его установки.
func (b *fakeBus) wantBridge() (bool, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.lastWantBridge, b.bridgeSet
}

func (b *fakeBus) publishLatest(ev applybus.Event) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, ch := range b.chs {
		ch <- ev
		return
	}
}

// fakeLease — мокаемый LeaseLookup.
type fakeLease struct {
	mu        sync.Mutex
	holders   map[string]string
	lookupErr error
}

func (l *fakeLease) ReadHolder(_ context.Context, sid string) (string, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.lookupErr != nil {
		return "", l.lookupErr
	}
	return l.holders[sid], nil
}

// buildTestDispatcher собирает Dispatcher с fake-зависимостями без
// NewDispatcher-validate.
func buildTestDispatcher(store StoreAPI, bus ApplyBus, ob OutboundSender, pub RemotePublisher, lease LeaseLookup, cap time.Duration) *Dispatcher {
	return &Dispatcher{deps: Deps{
		Store:       store,
		Outbound:    ob,
		Publisher:   pub,
		LeaseLookup: lease,
		ApplyBus:    bus,
		Logger:      slog.New(slog.NewJSONHandler(io.Discard, nil)),
		KID:         "kid-test",
		ServerCap:   cap,
		Clock:       time.Now,
	}}
}

func ptrInt32(v int32) *int32 { return &v }

// --- тесты ---

func TestDispatch_Sync_Success(t *testing.T) {
	store := newFakeStore()
	bus := newFakeBus()
	ob := &fakeOutbound{}
	lease := &fakeLease{holders: map[string]string{"host.test": "kid-test"}}

	d := buildTestDispatcher(store, bus, ob, ob, lease, 500*time.Millisecond)

	// Параллельно публикуем event с маленькой задержкой, чтобы Subscribe попал
	// в map fakeBus до Publish-а.
	go func() {
		time.Sleep(20 * time.Millisecond)
		bus.publishLatest(applybus.Event{
			Kind: applybus.KindErrandCompleted,
			Payload: ResultEvent{
				Status:   StatusSuccess,
				ExitCode: ptrInt32(0),
				Stdout:   "ok",
			},
		})
	}()

	res, err := d.Dispatch(context.Background(), DispatchRequest{
		SID:          "host.test",
		Module:       "core.cmd.shell",
		TimeoutSec:   5,
		StartedByAID: "archon-alice",
	})
	if err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	if res.Async {
		t.Fatalf("Async=true, want false (sync)")
	}
	if res.Status != StatusSuccess {
		t.Fatalf("status = %q, want success", res.Status)
	}
	if res.Stdout != "ok" {
		t.Fatalf("stdout = %q, want ok", res.Stdout)
	}
	if store.inserts != 1 || store.marks != 1 {
		t.Fatalf("inserts=%d marks=%d, want 1/1", store.inserts, store.marks)
	}
	if len(ob.sent) != 1 {
		t.Fatalf("ob.sent=%d, want 1", len(ob.sent))
	}
}

func TestDispatch_Sync_TimedOut(t *testing.T) {
	store := newFakeStore()
	bus := newFakeBus()
	ob := &fakeOutbound{}
	lease := &fakeLease{holders: map[string]string{"host.test": "kid-test"}}

	// TimeoutSec=1 (>= MinTimeoutSeconds), ServerCap=2s → ждём 1с sync, нет
	// event → terminal=timed_out.
	d := buildTestDispatcher(store, bus, ob, ob, lease, 2*time.Second)

	start := time.Now()
	res, err := d.Dispatch(context.Background(), DispatchRequest{
		SID:          "host.test",
		Module:       "core.cmd.shell",
		TimeoutSec:   1,
		StartedByAID: "archon-alice",
	})
	if err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	if res.Async {
		t.Fatalf("Async=true, want false (sync timed_out)")
	}
	if res.Status != StatusTimedOut {
		t.Fatalf("status = %q, want timed_out", res.Status)
	}
	if elapsed := time.Since(start); elapsed < 900*time.Millisecond {
		t.Fatalf("elapsed = %v, want ≥1s", elapsed)
	}
}

func TestDispatch_AsyncEscalation(t *testing.T) {
	store := newFakeStore()
	bus := newFakeBus()
	ob := &fakeOutbound{}
	lease := &fakeLease{holders: map[string]string{"host.test": "kid-test"}}

	// ServerCap=200ms, TimeoutSec=2 → sync ждёт 200ms, не получает →
	// возвращает Async=true. Goroutine продолжает до полного TimeoutSec.
	d := buildTestDispatcher(store, bus, ob, ob, lease, 200*time.Millisecond)

	res, err := d.Dispatch(context.Background(), DispatchRequest{
		SID:          "host.test",
		Module:       "core.cmd.shell",
		TimeoutSec:   2,
		StartedByAID: "archon-alice",
	})
	if err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	if !res.Async {
		t.Fatalf("Async=false, want true (escalation)")
	}
	if res.Status != StatusRunning {
		t.Fatalf("status = %q, want running", res.Status)
	}

	// Подождём goroutine timed_out (TimeoutSec=2s).
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		r := store.snapshot(res.ErrandID)
		if r != nil && r.Status == StatusTimedOut {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	r := store.snapshot(res.ErrandID)
	var got Status
	if r != nil {
		got = r.Status
	}
	t.Fatalf("async-goroutine status = %q, want timed_out after escalation", got)
}

func TestDispatch_Validate_EmptySID(t *testing.T) {
	d := buildTestDispatcher(newFakeStore(), newFakeBus(), &fakeOutbound{}, &fakeOutbound{}, &fakeLease{}, time.Second)
	_, err := d.Dispatch(context.Background(), DispatchRequest{Module: "core.cmd.shell"})
	if !errors.Is(err, ErrSIDEmpty) {
		t.Fatalf("err = %v, want ErrSIDEmpty", err)
	}
}

func TestDispatch_Validate_EmptyModule(t *testing.T) {
	d := buildTestDispatcher(newFakeStore(), newFakeBus(), &fakeOutbound{}, &fakeOutbound{}, &fakeLease{}, time.Second)
	_, err := d.Dispatch(context.Background(), DispatchRequest{SID: "host.test"})
	if !errors.Is(err, ErrModuleEmpty) {
		t.Fatalf("err = %v, want ErrModuleEmpty", err)
	}
}

func TestDispatch_Validate_TimeoutOutOfRange(t *testing.T) {
	d := buildTestDispatcher(newFakeStore(), newFakeBus(), &fakeOutbound{}, &fakeOutbound{}, &fakeLease{}, time.Second)
	_, err := d.Dispatch(context.Background(), DispatchRequest{
		SID:        "host.test",
		Module:     "core.cmd.shell",
		TimeoutSec: 500, // > MaxTimeoutSeconds
	})
	if !errors.Is(err, ErrTimeoutOutOfRange) {
		t.Fatalf("err = %v, want ErrTimeoutOutOfRange", err)
	}
}

// TestValidateDispatch_TimeoutBoundaries — граница timeout [Min, Max]=[1, 300]
// плюс нормализация 0→default. 0→ok (default), 1→ok, 300→ok, 301→fail,
// отрицательное→fail.
func TestValidateDispatch_TimeoutBoundaries(t *testing.T) {
	cases := []struct {
		name    string
		timeout int
		wantErr bool
		wantSet int // ожидаемый TimeoutSec после нормализации (только при !wantErr)
	}{
		{"zero → default", 0, false, DefaultTimeoutSeconds},
		{"min 1 → ok", MinTimeoutSeconds, false, MinTimeoutSeconds},
		{"max 300 → ok", MaxTimeoutSeconds, false, MaxTimeoutSeconds},
		{"max+1 301 → fail", MaxTimeoutSeconds + 1, true, 0},
		{"negative → fail", -1, true, 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			req := DispatchRequest{SID: "host.test", Module: "core.cmd.shell", TimeoutSec: c.timeout}
			err := validateDispatch(&req)
			if c.wantErr {
				if !errors.Is(err, ErrTimeoutOutOfRange) {
					t.Fatalf("timeout=%d → err %v, want ErrTimeoutOutOfRange", c.timeout, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("timeout=%d → unexpected err %v", c.timeout, err)
			}
			if req.TimeoutSec != c.wantSet {
				t.Errorf("timeout=%d → normalized %d, want %d", c.timeout, req.TimeoutSec, c.wantSet)
			}
		})
	}
}

// TestValidateDispatch_RequiredFields — SID / module non-empty.
func TestValidateDispatch_RequiredFields(t *testing.T) {
	cases := []struct {
		name   string
		req    DispatchRequest
		wantIs error
	}{
		{"empty SID", DispatchRequest{Module: "core.cmd.shell"}, ErrSIDEmpty},
		{"empty module", DispatchRequest{SID: "host.test"}, ErrModuleEmpty},
		{"both set → ok", DispatchRequest{SID: "host.test", Module: "core.cmd.shell"}, nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			req := c.req
			err := validateDispatch(&req)
			if c.wantIs == nil {
				if err != nil {
					t.Fatalf("unexpected err %v", err)
				}
				return
			}
			if !errors.Is(err, c.wantIs) {
				t.Fatalf("err = %v, want %v", err, c.wantIs)
			}
		})
	}
}

func TestDispatch_NotConnected_NoLease(t *testing.T) {
	store := newFakeStore()
	bus := newFakeBus()
	ob := &fakeOutbound{}
	lease := &fakeLease{holders: map[string]string{}} // нет holder-а

	d := buildTestDispatcher(store, bus, ob, ob, lease, time.Second)

	res, err := d.Dispatch(context.Background(), DispatchRequest{
		SID:          "host.unknown",
		Module:       "core.cmd.shell",
		StartedByAID: "archon-alice",
	})
	if !errors.Is(err, ErrSoulNotConnected) {
		t.Fatalf("err = %v, want ErrSoulNotConnected", err)
	}
	if res.Status != StatusFailed {
		t.Fatalf("status = %q, want failed", res.Status)
	}
}

func TestDispatch_RemoteRouting(t *testing.T) {
	store := newFakeStore()
	bus := newFakeBus()
	ob := &fakeOutbound{}
	// holder = другой keeper → должен пойти через Publisher (тот же ob).
	lease := &fakeLease{holders: map[string]string{"host.test": "kid-remote"}}

	d := buildTestDispatcher(store, bus, ob, ob, lease, 200*time.Millisecond)

	go func() {
		time.Sleep(20 * time.Millisecond)
		bus.publishLatest(applybus.Event{
			Kind:    applybus.KindErrandCompleted,
			Payload: ResultEvent{Status: StatusSuccess, ExitCode: ptrInt32(0)},
		})
	}()

	res, err := d.Dispatch(context.Background(), DispatchRequest{
		SID:          "host.test",
		Module:       "core.cmd.shell",
		TimeoutSec:   2,
		StartedByAID: "archon-alice",
	})
	if err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	if res.Status != StatusSuccess {
		t.Fatalf("status = %q, want success", res.Status)
	}
	if len(ob.sent) != 1 {
		t.Fatalf("ob.sent=%d, want 1 (PublishErrand path)", len(ob.sent))
	}
}

// TestDispatch_WantBridgePredicate — прямая таблица wantBridge-предиката
// (S1, holder==self skip). Используем fakeBus.wantBridge() как
// измеритель последнего bridge-решения dispatcher-а. Ловит инверсию условия
// `wantBridge := !(lookupOK && holder == d.deps.KID && holder != "")`.
//
// Покрытие:
//   - holder == self (KID-test) → wantBridge=false (skip — событие придёт
//     local от того же инстанса);
//   - holder == other → true (cross-keeper, bridge нужен);
//   - holder == "" авторитетный (lookupOK=true, пустой lease) → true; здесь
//     дойдёт до ErrSoulNotConnected, но bridge-решение уже снято до send;
//   - lookupErr (ReadHolder вернул error) → true (консервативно);
//   - LeaseLookup == nil → true (single-keeper / holder неизвестен).
func TestDispatch_WantBridgePredicate(t *testing.T) {
	const selfKID = "kid-test"
	const sid = "host.test"

	cases := []struct {
		name        string
		holders     map[string]string // nil + withLease=false → LeaseLookup nil
		lookupErr   error
		withLease   bool // true → fakeLease задан; false → LeaseLookup=nil
		wantBridge  bool
		wantErrSent bool // ожидаем, что Dispatch дойдёт до send (есть Outbound-доставка)
	}{
		{
			name:       "holder==self → skip bridge",
			holders:    map[string]string{sid: selfKID},
			withLease:  true,
			wantBridge: false,
		},
		{
			name:       "holder==other → bridge",
			holders:    map[string]string{sid: "kid-remote"},
			withLease:  true,
			wantBridge: true,
		},
		{
			name:       "holder=='' authoritative → bridge",
			holders:    map[string]string{}, // lookupOK=true, holder==""
			withLease:  true,
			wantBridge: true,
		},
		{
			name:       "lookupErr → conservative bridge",
			holders:    map[string]string{sid: selfKID}, // игнорируется из-за lookupErr
			lookupErr:  errors.New("redis flap"),
			withLease:  true,
			wantBridge: true,
		},
		{
			name:       "LeaseLookup==nil → bridge",
			withLease:  false,
			wantBridge: true,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			store := newFakeStore()
			bus := newFakeBus()
			ob := &fakeOutbound{}

			var lease LeaseLookup
			if c.withLease {
				lease = &fakeLease{holders: c.holders, lookupErr: c.lookupErr}
			}

			// ServerCap мал, TimeoutSec мал, event не публикуем → Dispatch
			// завершится timed_out/failed, но wantBridge-решение снимается ДО
			// wait-а. Главное — что Subscribe вызван и bridgeSet=true.
			d := buildTestDispatcher(store, bus, ob, ob, lease, 50*time.Millisecond)

			// holder=="" authoritative ведёт к ErrSoulNotConnected (send fail) —
			// но Subscribe всё равно вызывается ДО send, bridge-решение снято.
			_, _ = d.Dispatch(context.Background(), DispatchRequest{
				SID:          sid,
				Module:       "core.cmd.shell",
				TimeoutSec:   1,
				StartedByAID: "archon-alice",
			})

			gotBridge, set := bus.wantBridge()
			if !set {
				t.Fatalf("SubscribeWithBridge не вызван — bridge-решение не снято")
			}
			if gotBridge != c.wantBridge {
				t.Fatalf("wantBridge = %v, want %v", gotBridge, c.wantBridge)
			}
		})
	}
}

// TestDispatch_LookupErr_BridgeAndLocalDelivery — ошибка ReadHolder
// (Redis-флап) НЕ ломает доставку: dispatcher выбирает консервативный
// wantBridge=true И падает на local-fallback через Outbound (lookupOK=false →
// SendErrand). Errand доставлен (success-event дочитан). Важно для HA: флап
// Redis-lease не должен ронять exec.
//
// Задействует fakeLease.lookupErr.
func TestDispatch_LookupErr_BridgeAndLocalDelivery(t *testing.T) {
	store := newFakeStore()
	bus := newFakeBus()
	ob := &fakeOutbound{}
	lease := &fakeLease{
		holders:   map[string]string{"host.test": "kid-test"}, // не важно — lookupErr перекрывает
		lookupErr: errors.New("redis: connection refused"),
	}

	d := buildTestDispatcher(store, bus, ob, ob, lease, 2*time.Second)

	go func() {
		time.Sleep(20 * time.Millisecond)
		bus.publishLatest(applybus.Event{
			Kind: applybus.KindErrandCompleted,
			Payload: ResultEvent{
				Status:   StatusSuccess,
				ExitCode: ptrInt32(0),
				Stdout:   "ok",
			},
		})
	}()

	res, err := d.Dispatch(context.Background(), DispatchRequest{
		SID:          "host.test",
		Module:       "core.cmd.shell",
		TimeoutSec:   5,
		StartedByAID: "archon-alice",
	})
	if err != nil {
		t.Fatalf("Dispatch: %v (lookup-флап не должен ронять доставку)", err)
	}

	// Консервативный bridge при ошибке lookup-а.
	if gotBridge, set := bus.wantBridge(); !set || !gotBridge {
		t.Fatalf("wantBridge = %v (set=%v), want true (lookupErr → conservative bridge)", gotBridge, set)
	}
	// Доставка прошла через local-fallback Outbound.
	if res.Status != StatusSuccess {
		t.Fatalf("status = %q, want success (local-fallback delivery on lookupErr)", res.Status)
	}
	if len(ob.sent) != 1 {
		t.Fatalf("ob.sent = %d, want 1 (SendErrand local-fallback path)", len(ob.sent))
	}
}

// TestDispatch_HolderSelf_LocalPublisherSilent_TimesOut — holder==self
// (bridge пропущен), но local publisher МОЛЧИТ (Soul отвалился, ErrandRequest
// не доехал / результат не пришёл). Dispatch завершается timed_out по
// wait-timer-у (НЕ зависание), elapsed ≈ TimeoutSec. Зеркало
// dispatcher_bridge_test.go::HolderFlipAfterSkip, но на fake-bus и без
// публикации вовсе — изолирует «publisher молчит».
func TestDispatch_HolderSelf_LocalPublisherSilent_TimesOut(t *testing.T) {
	store := newFakeStore()
	bus := newFakeBus()
	ob := &fakeOutbound{}
	lease := &fakeLease{holders: map[string]string{"host.test": "kid-test"}} // holder==self

	// TimeoutSec=1, ServerCap=2s → sync-wait 1с, нет event → timed_out (sync).
	d := buildTestDispatcher(store, bus, ob, ob, lease, 2*time.Second)

	start := time.Now()
	res, err := d.Dispatch(context.Background(), DispatchRequest{
		SID:          "host.test",
		Module:       "core.cmd.shell",
		TimeoutSec:   1,
		StartedByAID: "archon-alice",
	})
	if err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	// holder==self → bridge пропущен (но Errand всё равно завершается по timer-у).
	if gotBridge, set := bus.wantBridge(); !set || gotBridge {
		t.Fatalf("wantBridge = %v (set=%v), want false (holder==self skip)", gotBridge, set)
	}
	if res.Async {
		t.Fatalf("Async=true, want sync timed_out")
	}
	if res.Status != StatusTimedOut {
		t.Fatalf("status = %q, want timed_out (publisher silent → timer fires)", res.Status)
	}
	if elapsed := time.Since(start); elapsed < 900*time.Millisecond {
		t.Fatalf("elapsed = %v, want ≥1s (timer must fire, not hang)", elapsed)
	}
}

// TestDispatch_HolderSelf_HappyPath_Fast — holder==self happy-path: result
// приходит быстро через local-bus, Dispatch возвращается БЫСТРО (НЕ ждёт
// sync-timer). Контр-пара к Silent_TimesOut: skip-bridge не вносит задержку
// на happy-path. assert: elapsed заметно меньше TimeoutSec.
func TestDispatch_HolderSelf_HappyPath_Fast(t *testing.T) {
	store := newFakeStore()
	bus := newFakeBus()
	ob := &fakeOutbound{}
	lease := &fakeLease{holders: map[string]string{"host.test": "kid-test"}} // holder==self

	// TimeoutSec=5, ServerCap=5s; event придёт через ~20ms → возврат быстрый.
	d := buildTestDispatcher(store, bus, ob, ob, lease, 5*time.Second)

	go func() {
		time.Sleep(20 * time.Millisecond)
		bus.publishLatest(applybus.Event{
			Kind:    applybus.KindErrandCompleted,
			Payload: ResultEvent{Status: StatusSuccess, ExitCode: ptrInt32(0), Stdout: "fast"},
		})
	}()

	start := time.Now()
	res, err := d.Dispatch(context.Background(), DispatchRequest{
		SID:          "host.test",
		Module:       "core.cmd.shell",
		TimeoutSec:   5,
		StartedByAID: "archon-alice",
	})
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	if gotBridge, set := bus.wantBridge(); !set || gotBridge {
		t.Fatalf("wantBridge = %v (set=%v), want false (holder==self skip)", gotBridge, set)
	}
	if res.Status != StatusSuccess {
		t.Fatalf("status = %q, want success", res.Status)
	}
	if res.Async {
		t.Fatalf("Async=true, want sync success")
	}
	// Быстро: не ждали ни ServerCap (5s), ни TimeoutSec (5s).
	if elapsed > time.Second {
		t.Fatalf("elapsed = %v, want < 1s (happy-path must not wait timer)", elapsed)
	}
}

// TestDispatch_MixedParallel — ≥10 одновременных Dispatch с разными SID/
// errandID: часть holder==self (skip-bridge), часть holder==other (bridge).
// Все завершаются success, refs изолированы по каналам (каждый Dispatch
// получает СВОЙ результат, без перекрёстной доставки). Под -race.
//
// fakeBus здесь — отдельный per-Dispatch (изоляция каналов гарантируется
// разными bus-инстансами, как в проде applyID-ключом). Это guard на то, что
// holder→wantBridge маршрутизация корректна под параллелью и не путает
// результаты.
func TestDispatch_MixedParallel(t *testing.T) {
	const workers = 16

	type job struct {
		sid        string
		holder     string // self или other
		wantBridge bool
	}

	var wg sync.WaitGroup
	wg.Add(workers)
	errs := make([]error, workers)
	statuses := make([]Status, workers)
	bridges := make([]bool, workers)

	for w := 0; w < workers; w++ {
		go func(idx int) {
			defer wg.Done()

			j := job{sid: "host-" + string(rune('a'+idx))}
			if idx%2 == 0 {
				j.holder = "kid-test" // self → skip
				j.wantBridge = false
			} else {
				j.holder = "kid-remote" // other → bridge
				j.wantBridge = true
			}

			store := newFakeStore()
			bus := newFakeBus()
			ob := &fakeOutbound{}
			lease := &fakeLease{holders: map[string]string{j.sid: j.holder}}
			d := buildTestDispatcher(store, bus, ob, ob, lease, 2*time.Second)

			// Каждый воркер публикует СВОЙ результат с уникальным stdout =
			// собственный SID → проверяем изоляцию каналов.
			go func() {
				// Ждём, пока Subscribe зарегистрирует канал на этом bus.
				for i := 0; i < 200; i++ {
					if _, set := bus.wantBridge(); set {
						break
					}
					time.Sleep(2 * time.Millisecond)
				}
				time.Sleep(10 * time.Millisecond)
				bus.publishLatest(applybus.Event{
					Kind:    applybus.KindErrandCompleted,
					Payload: ResultEvent{Status: StatusSuccess, ExitCode: ptrInt32(0), Stdout: j.sid},
				})
			}()

			res, err := d.Dispatch(context.Background(), DispatchRequest{
				SID:          j.sid,
				Module:       "core.cmd.shell",
				TimeoutSec:   2,
				StartedByAID: "archon-alice",
			})
			errs[idx] = err
			statuses[idx] = res.Status
			gotBridge, _ := bus.wantBridge()
			bridges[idx] = gotBridge

			// Изоляция: результат этого Dispatch несёт собственный SID в stdout.
			if err == nil && res.Stdout != j.sid {
				errs[idx] = errors.New("cross-channel leak: stdout=" + res.Stdout + " want " + j.sid)
			}
			if gotBridge != j.wantBridge {
				errs[idx] = errors.New("wantBridge mismatch for " + j.sid)
			}
		}(w)
	}
	wg.Wait()

	for i := 0; i < workers; i++ {
		if errs[i] != nil {
			t.Errorf("worker %d: %v", i, errs[i])
		}
		if statuses[i] != StatusSuccess {
			t.Errorf("worker %d: status = %q, want success", i, statuses[i])
		}
	}
}

func TestReplay_OrphanRunning(t *testing.T) {
	store := newFakeStore()
	// Pre-insert running-Errand этого KID с давним started_at.
	old := time.Now().Add(-30 * time.Minute).UTC()
	_ = store.Insert(context.Background(), Row{
		ErrandID:     "01HAAAAAAAAAAAAAAAAAAAAAAA",
		SID:          "host.test",
		Module:       "core.cmd.shell",
		Status:       StatusRunning,
		StartedByKID: "kid-test",
		StartedAt:    old,
		TTLAt:        old.Add(TTLDefault),
	})

	d := buildTestDispatcher(store, newFakeBus(), &fakeOutbound{}, &fakeOutbound{}, &fakeLease{}, time.Second)

	n, err := d.Replay(context.Background(), ReplayOptions{Grace: 10 * time.Minute})
	if err != nil {
		t.Fatalf("Replay: %v", err)
	}
	if n != 1 {
		t.Fatalf("Replay returned %d, want 1", n)
	}
	r := store.snapshot("01HAAAAAAAAAAAAAAAAAAAAAAA")
	if r == nil || r.Status != StatusTimedOut {
		var got Status
		if r != nil {
			got = r.Status
		}
		t.Fatalf("orphan status = %q, want timed_out", got)
	}
}
