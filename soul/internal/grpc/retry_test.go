package grpc

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"

	keeperv1 "github.com/souls-guild/soul-stack/proto/gen/go/keeper/v1"
)

// codeHandler — handshake-handler, всегда отвергающий Hello заданным gRPC-кодом.
// Моделирует keeper, отдающий non-retriable отказ (auth/contract).
func codeHandler(code codes.Code, msg string) func(*keeperv1.Hello) (*keeperv1.HelloReply, error) {
	return func(_ *keeperv1.Hello) (*keeperv1.HelloReply, error) {
		return nil, status.Errorf(code, "%s", msg)
	}
}

// failThenOKHandler возвращает handler, отдающий `failCount` раз заданный
// retriable-код, затем успешный HelloReply. Счётчик — на замыкании, по числу
// вызовов handler-а (= числу dialOne к этому серверу).
func failThenOKHandler(code codes.Code, failCount int) func(*keeperv1.Hello) (*keeperv1.HelloReply, error) {
	var calls int
	return func(_ *keeperv1.Hello) (*keeperv1.HelloReply, error) {
		calls++
		if calls <= failCount {
			return nil, status.Errorf(code, "transient flake %d", calls)
		}
		return &keeperv1.HelloReply{SessionId: "sess-retry", Kid: "test-kid", ServerTime: timestamppb.Now()}, nil
	}
}

// --- #2 ★КРИТ: регресс-guard 7af8e95 ---
// AlreadyExists (lease-held) — non-retriable: dialOne РОВНО 1 раз на endpoint,
// spray немедленно к следующему. Churn (повтор к занятому lease) не вернулся.
func TestRetry_LeaseHeld_SingleDialOne_NoChurn(t *testing.T) {
	leaseHeld := newMockEventStream(t, alreadyExistsHandler)
	defer leaseHeld.stop()
	alive := newMockEventStreamWithCA(t, nil, leaseHeld)
	defer alive.stop()

	cli, err := NewClient(ClientConfig{
		Endpoints: []Endpoint{
			{Addr: leaseHeld.addr, Priority: 1},
			{Addr: alive.addr, Priority: 2},
		},
		SeedCert:          leaseHeld.clientCert,
		SeedKey:           leaseHeld.clientKey,
		CAPath:            leaseHeld.caPath,
		HandshakeTimeout:  3 * time.Second,
		SID:               "host.example",
		MaxAttempts:       5, // даём запас: lease-held НЕ должен его жечь
		InterAttemptDelay: 10 * time.Millisecond,
	}, nil)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	sess, err := cli.Dial(context.Background())
	if err != nil {
		t.Fatalf("Dial: expected spray to alive, got %v", err)
	}
	defer sess.Close()

	if n := leaseHeld.dialCount(); n != 1 {
		t.Fatalf("lease-held endpoint: dialOne called %d times, want EXACTLY 1 (churn regressed)", n)
	}
	if sess.Priority() != 2 {
		t.Errorf("session.Priority = %d, want 2 (sprayed past lease-held)", sess.Priority())
	}
}

// --- #3: ВСЕ endpoint-ы AlreadyExists → IsLeaseHeld (аккумулятор не сломан) ---
func TestRetry_AllLeaseHeld_AccumulatorIntact(t *testing.T) {
	a := newMockEventStream(t, alreadyExistsHandler)
	defer a.stop()
	b := newMockEventStreamWithCA(t, alreadyExistsHandler, a)
	defer b.stop()

	cli, err := NewClient(ClientConfig{
		Endpoints: []Endpoint{
			{Addr: a.addr, Priority: 1},
			{Addr: b.addr, Priority: 2},
		},
		SeedCert:          a.clientCert,
		SeedKey:           a.clientKey,
		CAPath:            a.caPath,
		HandshakeTimeout:  3 * time.Second,
		SID:               "host.example",
		MaxAttempts:       4,
		InterAttemptDelay: 10 * time.Millisecond,
	}, nil)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	_, err = cli.Dial(context.Background())
	if err == nil {
		t.Fatal("Dial: expected lease-held error, got nil")
	}
	if !IsLeaseHeld(err) {
		t.Fatalf("Dial err=%v, want IsLeaseHeld (accumulator broken by retry-loop)", err)
	}
	// каждый endpoint опрошен ровно один раз (не MaxAttempts).
	if n := a.dialCount(); n != 1 {
		t.Errorf("endpoint a dialOne %d times, want 1", n)
	}
	if n := b.dialCount(); n != 1 {
		t.Errorf("endpoint b dialOne %d times, want 1", n)
	}
}

// --- #5: max_attempts=1 → ровно одна попытка на endpoint (до-фикс поведение) ---
func TestRetry_MaxAttemptsOne_SingleAttempt(t *testing.T) {
	srv := newMockEventStream(t, codeHandler(codes.Unavailable, "down"))
	defer srv.stop()

	cli, err := NewClient(ClientConfig{
		Endpoints:         []Endpoint{{Addr: srv.addr}},
		SeedCert:          srv.clientCert,
		SeedKey:           srv.clientKey,
		CAPath:            srv.caPath,
		HandshakeTimeout:  3 * time.Second,
		SID:               "host.example",
		MaxAttempts:       1,
		InterAttemptDelay: 10 * time.Millisecond,
	}, nil)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	_, err = cli.Dial(context.Background())
	if err == nil {
		t.Fatal("Dial: expected error, got nil")
	}
	if n := srv.dialCount(); n != 1 {
		t.Fatalf("max_attempts=1: dialOne %d times, want EXACTLY 1", n)
	}
}

// --- #1: Unavailable (maxAttempts-1) раз, затем успех → подключился ---
func TestRetry_RetriableThenSuccess(t *testing.T) {
	const maxAttempts = 3
	srv := newMockEventStream(t, failThenOKHandler(codes.Unavailable, maxAttempts-1))
	defer srv.stop()

	cli, err := NewClient(ClientConfig{
		Endpoints:         []Endpoint{{Addr: srv.addr}},
		SeedCert:          srv.clientCert,
		SeedKey:           srv.clientKey,
		CAPath:            srv.caPath,
		HandshakeTimeout:  3 * time.Second,
		SID:               "host.example",
		MaxAttempts:       maxAttempts,
		InterAttemptDelay: 10 * time.Millisecond,
	}, nil)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	sess, err := cli.Dial(context.Background())
	if err != nil {
		t.Fatalf("Dial: expected success on attempt %d, got %v", maxAttempts, err)
	}
	defer sess.Close()
	if n := srv.dialCount(); n != maxAttempts {
		t.Fatalf("dialOne %d times, want %d (retried then connected)", n, maxAttempts)
	}
}

// --- #6: max_attempts=0 (опущено) → резолв в 2 (NewClient), не 0/бесконечность ---
func TestRetry_ZeroResolvesToDefault(t *testing.T) {
	cli, err := NewClient(ClientConfig{
		Endpoints: []Endpoint{{Addr: "k:9443"}},
		SeedCert:  "/c", SeedKey: "/k", CAPath: "/a",
		SID: "host.example",
		// MaxAttempts опущено → 0
	}, nil)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	if cli.cfg.MaxAttempts != defaultMaxAttempts {
		t.Fatalf("MaxAttempts resolved to %d, want %d", cli.cfg.MaxAttempts, defaultMaxAttempts)
	}
	if defaultMaxAttempts != 2 {
		t.Fatalf("defaultMaxAttempts = %d, want 2", defaultMaxAttempts)
	}
}

// --- #7 fail-fast: PermissionDenied / InvalidArgument → dialOne 1 раз, spray ---
func TestRetry_NonRetriableFailFast(t *testing.T) {
	cases := []struct {
		name string
		code codes.Code
	}{
		{"PermissionDenied", codes.PermissionDenied},
		{"InvalidArgument", codes.InvalidArgument},
		{"Unauthenticated", codes.Unauthenticated},
		{"FailedPrecondition", codes.FailedPrecondition},
		{"Unimplemented", codes.Unimplemented},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rejecting := newMockEventStream(t, codeHandler(tc.code, "nope"))
			defer rejecting.stop()
			alive := newMockEventStreamWithCA(t, nil, rejecting)
			defer alive.stop()

			cli, err := NewClient(ClientConfig{
				Endpoints: []Endpoint{
					{Addr: rejecting.addr, Priority: 1},
					{Addr: alive.addr, Priority: 2},
				},
				SeedCert:          rejecting.clientCert,
				SeedKey:           rejecting.clientKey,
				CAPath:            rejecting.caPath,
				HandshakeTimeout:  3 * time.Second,
				SID:               "host.example",
				MaxAttempts:       5,
				InterAttemptDelay: 10 * time.Millisecond,
			}, nil)
			if err != nil {
				t.Fatalf("NewClient: %v", err)
			}
			sess, err := cli.Dial(context.Background())
			if err != nil {
				t.Fatalf("Dial: expected spray to alive, got %v", err)
			}
			defer sess.Close()
			if n := rejecting.dialCount(); n != 1 {
				t.Fatalf("%s: dialOne %d times, want EXACTLY 1 (non-retriable burned attempts)", tc.name, n)
			}
			if sess.Priority() != 2 {
				t.Errorf("session.Priority = %d, want 2 (sprayed)", sess.Priority())
			}
		})
	}
}

// --- #4: endpoint X отдаёт Unavailable maxAttempts раз → переход к Y (успех) ---
func TestRetry_ExhaustsRetriableThenSpray(t *testing.T) {
	const maxAttempts = 3
	x := newMockEventStream(t, codeHandler(codes.Unavailable, "x-down"))
	defer x.stop()
	y := newMockEventStreamWithCA(t, nil, x)
	defer y.stop()

	cli, err := NewClient(ClientConfig{
		Endpoints: []Endpoint{
			{Addr: x.addr, Priority: 1},
			{Addr: y.addr, Priority: 2},
		},
		SeedCert:          x.clientCert,
		SeedKey:           x.clientKey,
		CAPath:            x.caPath,
		HandshakeTimeout:  3 * time.Second,
		SID:               "host.example",
		MaxAttempts:       maxAttempts,
		InterAttemptDelay: 10 * time.Millisecond,
	}, nil)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	sess, err := cli.Dial(context.Background())
	if err != nil {
		t.Fatalf("Dial: expected spray to y, got %v", err)
	}
	defer sess.Close()
	if n := x.dialCount(); n != maxAttempts {
		t.Errorf("endpoint x dialOne %d times, want %d (exhausted retriable)", n, maxAttempts)
	}
	if sess.Priority() != 2 {
		t.Errorf("session.Priority = %d, want 2 (y picked after x exhausted)", sess.Priority())
	}
}

// --- #9: failback (maxPriority>0) → errNoHigherPriority НЕ замаскирован ---
func TestRetry_FailbackNoHigherPriority_NotMasked(t *testing.T) {
	srv := newMockEventStream(t, nil)
	defer srv.stop()

	cli, err := NewClient(ClientConfig{
		Endpoints:         []Endpoint{{Addr: srv.addr, Priority: 2}},
		SeedCert:          srv.clientCert,
		SeedKey:           srv.clientKey,
		CAPath:            srv.caPath,
		HandshakeTimeout:  3 * time.Second,
		SID:               "host.example",
		MaxAttempts:       4,
		InterAttemptDelay: 10 * time.Millisecond,
	}, nil)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	_, err = cli.DialPriority(context.Background(), 2)
	if !IsNoHigherPriority(err) {
		t.Fatalf("DialPriority(maxPriority=2): err=%v, want IsNoHigherPriority (masked by retry-loop?)", err)
	}
	// retry-loop НЕ касается пропущенного endpoint-а: dialOne не вызывался вовсе.
	if n := srv.dialCount(); n != 0 {
		t.Errorf("skipped endpoint dialOne %d times, want 0", n)
	}
}

// --- #10: между попытками к одному endpoint выдержана пауза ≈ initial, не tight-loop ---
func TestRetry_InterAttemptDelayObserved(t *testing.T) {
	const maxAttempts = 3
	const delay = 80 * time.Millisecond
	srv := newMockEventStream(t, codeHandler(codes.Unavailable, "down"))
	defer srv.stop()

	cli, err := NewClient(ClientConfig{
		Endpoints:         []Endpoint{{Addr: srv.addr}},
		SeedCert:          srv.clientCert,
		SeedKey:           srv.clientKey,
		CAPath:            srv.caPath,
		HandshakeTimeout:  3 * time.Second,
		SID:               "host.example",
		MaxAttempts:       maxAttempts,
		InterAttemptDelay: delay,
		// jitter off — детерминированная нижняя граница.
	}, nil)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	start := time.Now()
	_, err = cli.Dial(context.Background())
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("Dial: expected exhaustion error, got nil")
	}
	// maxAttempts попыток = (maxAttempts-1) пауз между ними.
	wantMin := time.Duration(maxAttempts-1) * delay
	if elapsed < wantMin {
		t.Fatalf("elapsed %s < %s (tight-loop: inter-attempt delay not observed)", elapsed, wantMin)
	}
	if n := srv.dialCount(); n != maxAttempts {
		t.Errorf("dialOne %d times, want %d", n, maxAttempts)
	}
}

// --- ctx-cancel прерывает inter-attempt паузу (не висим всё окно) ---
func TestRetry_CtxCancelInterruptsPause(t *testing.T) {
	srv := newMockEventStream(t, codeHandler(codes.Unavailable, "down"))
	defer srv.stop()

	cli, err := NewClient(ClientConfig{
		Endpoints:         []Endpoint{{Addr: srv.addr}},
		SeedCert:          srv.clientCert,
		SeedKey:           srv.clientKey,
		CAPath:            srv.caPath,
		HandshakeTimeout:  3 * time.Second,
		SID:               "host.example",
		MaxAttempts:       10,
		InterAttemptDelay: 10 * time.Second, // длинная пауза — отменим раньше
	}, nil)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(150 * time.Millisecond)
		cancel()
	}()
	start := time.Now()
	_, err = cli.Dial(ctx)
	if err == nil {
		t.Fatal("Dial: expected error on ctx cancel, got nil")
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("Dial blocked %s — ctx cancel did not interrupt inter-attempt pause", elapsed)
	}
}

// --- #11: failover-latency bound на зависшем endpoint (handshake-timeout каждую попытку) ---
// Endpoint с hang-handler сгорает по локальному handshake_timeout на КАЖДУЮ
// попытку (handshake timeout → codes.Unknown → retriable). max_attempts=2 →
// две полных handshake_timeout-паузы + одна inter-attempt пауза, затем spray на
// живой endpoint. Пинует верхнюю границу failover-latency: ловит регресс
// «handshake-timeout стал non-retriable» (тогда 1× вместо 2×) и «кто-то поднял
// default max_attempts» (тогда >2× и dialCount>2). Сравнение относительное (к
// handshake_timeout), допуски щедрые — тайминг не должен быть flaky.
func TestRetry_FailoverLatencyBound_HungEndpoint(t *testing.T) {
	const hsTimeout = 120 * time.Millisecond
	const interAttempt = 40 * time.Millisecond
	const maxAttempts = 2

	hung := newMockEventStreamCtx(t, hangHandler, nil)
	defer hung.stop()
	alive := newMockEventStreamWithCA(t, nil, hung)
	defer alive.stop()

	cli, err := NewClient(ClientConfig{
		Endpoints: []Endpoint{
			{Addr: hung.addr, Priority: 1},
			{Addr: alive.addr, Priority: 2},
		},
		SeedCert:          hung.clientCert,
		SeedKey:           hung.clientKey,
		CAPath:            hung.caPath,
		HandshakeTimeout:  hsTimeout,
		SID:               "host.example",
		MaxAttempts:       maxAttempts,
		InterAttemptDelay: interAttempt,
	}, nil)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	start := time.Now()
	sess, err := cli.Dial(context.Background())
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("Dial: expected spray to alive, got %v", err)
	}
	defer sess.Close()

	if sess.Priority() != 2 {
		t.Errorf("session.Priority = %d, want 2 (sprayed past hung endpoint)", sess.Priority())
	}
	// hung endpoint опрошен РОВНО maxAttempts раз (handshake-timeout retriable,
	// default не подняли).
	if n := hung.dialCount(); n != maxAttempts {
		t.Fatalf("hung endpoint dialOne %d times, want %d (handshake-timeout retriable? default raised?)", n, maxAttempts)
	}
	// Нижняя граница: каждая попытка минимум висит handshake_timeout (handshake
	// не отвечает) → maxAttempts handshake_timeout-ов. Без inter-attempt в
	// нижней границе (jitter/планировщик), он лишь добавляет сверху.
	wantMin := time.Duration(maxAttempts) * hsTimeout
	if elapsed < wantMin {
		t.Fatalf("elapsed %s < %s — hung endpoint не дожидался handshake_timeout каждую попытку", elapsed, wantMin)
	}
	// Верхняя граница: maxAttempts handshake_timeout-ов + (maxAttempts-1)
	// inter-attempt пауз + живой handshake + накладные. Щедрый множитель ловит
	// регресс «retry стал больше maxAttempts» / «handshake_timeout не действует».
	wantMax := time.Duration(maxAttempts)*hsTimeout + time.Duration(maxAttempts-1)*interAttempt + 3*time.Second
	if elapsed > wantMax {
		t.Fatalf("elapsed %s > %s — failover-latency вышла за границу (лишние попытки / поднятый default / non-retriable)", elapsed, wantMax)
	}
}

// --- #12: ctx-cancel ВО ВРЕМЯ зависшего dialOne (не во время паузы) ---
// dialOne висит на handshake (hang-handler), handshake_timeout большой (5s).
// Отменяем ctx через ~150ms. sessCtx производен от ctx → его cancel закрывает
// стрим → handshake-горутина в dialOne получает recv-ошибку и hsDone
// срабатывает БЫСТРО (не по time.After(handshake_timeout)). Пинует неочевидную
// механику: dialOne реагирует на отмену ctx через производный sessCtx, хотя
// локальный select watch-ит time.After, не ctx напрямую. Регресс «sessCtx
// перестал быть производным от ctx» / «handshake перестал прерываться разрывом»
// сделает Dial зависшим на ~handshake_timeout. Отличается от
// TestRetry_CtxCancelInterruptsPause: тот про inter-attempt паузу.
func TestRetry_CtxCancelDuringDialOne(t *testing.T) {
	const hsTimeout = 5 * time.Second
	hung := newMockEventStreamCtx(t, hangHandler, nil)
	defer hung.stop()

	cli, err := NewClient(ClientConfig{
		Endpoints:         []Endpoint{{Addr: hung.addr}},
		SeedCert:          hung.clientCert,
		SeedKey:           hung.clientKey,
		CAPath:            hung.caPath,
		HandshakeTimeout:  hsTimeout,
		SID:               "host.example",
		MaxAttempts:       10,
		InterAttemptDelay: 10 * time.Second, // длинная пауза — отмена должна попасть в dialOne, не в неё
	}, nil)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(150 * time.Millisecond)
		cancel()
	}()
	start := time.Now()
	_, err = cli.Dial(ctx)
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("Dial: expected error on ctx cancel during dialOne, got nil")
	}
	// Вернулись быстро — по разрыву стрима, не по handshake_timeout (5s) и не по
	// inter-attempt паузе (10s). Допуск щедрый (1s), но на порядок ниже обоих.
	if elapsed > 1*time.Second {
		t.Fatalf("Dial blocked %s — ctx cancel не прервал зависший dialOne (упал в handshake_timeout %s?)", elapsed, hsTimeout)
	}
	// dialOne пробовался ровно один раз — отмена попала в первую же попытку, до
	// inter-attempt паузы / второго attempt-а.
	if n := hung.dialCount(); n != 1 {
		t.Errorf("hung endpoint dialOne %d times, want 1 (отмена попала в первую попытку)", n)
	}
}

// --- #13a: mixed errors на одном endpoint: transient → AlreadyExists ---
// Единственный endpoint: attempt 1 = Unavailable (retriable, retry), attempt 2 =
// AlreadyExists (non-retriable, break). dialCount==2. allLeaseHeld учитывает
// ПОСЛЕДНЮЮ ошибку endpoint-а — она AlreadyExists → IsLeaseHeld==true (осознанный
// дизайн «последняя ошибка endpoint-а решает аккумулятор», QA-observation).
func TestRetry_MixedErrors_TransientThenLeaseHeld(t *testing.T) {
	srv := newMockEventStream(t, seqHandler(codes.Unavailable, codes.AlreadyExists))
	defer srv.stop()

	cli, err := NewClient(ClientConfig{
		Endpoints:         []Endpoint{{Addr: srv.addr}},
		SeedCert:          srv.clientCert,
		SeedKey:           srv.clientKey,
		CAPath:            srv.caPath,
		HandshakeTimeout:  3 * time.Second,
		SID:               "host.example",
		MaxAttempts:       5, // запас: break после AlreadyExists, попытки не выжигаются
		InterAttemptDelay: 10 * time.Millisecond,
	}, nil)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	_, err = cli.Dial(context.Background())
	if err == nil {
		t.Fatal("Dial: expected error, got nil")
	}
	// Ровно 2 dialOne: transient ретраился, на 2-й попытке AlreadyExists → break
	// (не дошли до 3..5).
	if n := srv.dialCount(); n != 2 {
		t.Fatalf("dialOne %d times, want EXACTLY 2 (transient retried, AlreadyExists broke before exhausting attempts)", n)
	}
	// Последняя ошибка endpoint-а — AlreadyExists, единственный endpoint → агрегат
	// = lease-held (аккумулятор зависит от ПОСЛЕДНЕЙ ошибки).
	if !IsLeaseHeld(err) {
		t.Fatalf("Dial err=%v, want IsLeaseHeld (last error of endpoint = AlreadyExists, accumulator должен решить по ней)", err)
	}
}

// --- #13b: AlreadyExists на первой же попытке → break сразу (dialCount==1) ---
// Подтверждает контраст с #13a: без transient-преамбулы AlreadyExists ломает
// retry-loop немедленно, без второй попытки. lease-held сохранён.
func TestRetry_MixedErrors_LeaseHeldFirstAttempt(t *testing.T) {
	srv := newMockEventStream(t, seqHandler(codes.AlreadyExists))
	defer srv.stop()

	cli, err := NewClient(ClientConfig{
		Endpoints:         []Endpoint{{Addr: srv.addr}},
		SeedCert:          srv.clientCert,
		SeedKey:           srv.clientKey,
		CAPath:            srv.caPath,
		HandshakeTimeout:  3 * time.Second,
		SID:               "host.example",
		MaxAttempts:       5,
		InterAttemptDelay: 10 * time.Millisecond,
	}, nil)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	_, err = cli.Dial(context.Background())
	if err == nil {
		t.Fatal("Dial: expected error, got nil")
	}
	if n := srv.dialCount(); n != 1 {
		t.Fatalf("dialOne %d times, want EXACTLY 1 (AlreadyExists на 1-й попытке → break сразу)", n)
	}
	if !IsLeaseHeld(err) {
		t.Fatalf("Dial err=%v, want IsLeaseHeld", err)
	}
}

// --- #14a: failback (maxPriority>0), higher-prio СУЩЕСТВУЕТ но retriable-падает ---
// Higher-prio endpoint (priority=1) отдаёт Unavailable (retriable) каждую
// попытку → retry-loop отрабатывает max_attempts на нём, затем DialPriority
// возвращает НАСТОЯЩИЙ агрегат «all endpoints failed», НЕ errNoHigherPriority
// (#9 покрывал только skipped-endpoint, где dialOne не вызывался вовсе).
// Регресс «падение higher-prio маскируется под NoHigherPriority» был бы тихим:
// failback-loop счёл бы «возвращаться некуда» вместо «higher-prio временно лёг».
func TestRetry_Failback_HigherPrioRetriableFails_NotMasked(t *testing.T) {
	const maxAttempts = 3
	higher := newMockEventStream(t, codeHandler(codes.Unavailable, "higher-down"))
	defer higher.stop()

	cli, err := NewClient(ClientConfig{
		Endpoints:         []Endpoint{{Addr: higher.addr, Priority: 1}},
		SeedCert:          higher.clientCert,
		SeedKey:           higher.clientKey,
		CAPath:            higher.caPath,
		HandshakeTimeout:  3 * time.Second,
		SID:               "host.example",
		MaxAttempts:       maxAttempts,
		InterAttemptDelay: 10 * time.Millisecond,
	}, nil)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	// maxPriority=2 → priority=1 endpoint в скоупе (1 < 2), но падает.
	_, err = cli.DialPriority(context.Background(), 2)
	if err == nil {
		t.Fatal("DialPriority: expected error (higher-prio retriable-down), got nil")
	}
	// НЕ замаскировано под NoHigherPriority: endpoint реально пробовался и упал.
	if IsNoHigherPriority(err) {
		t.Fatalf("DialPriority err=%v classified NoHigherPriority — падение higher-prio замаскировано (failback счёл «некуда возвращаться»)", err)
	}
	// retry-loop отработал max_attempts на higher-prio (Unavailable retriable).
	if n := higher.dialCount(); n != maxAttempts {
		t.Fatalf("higher-prio dialOne %d times, want %d (exhausted retriable in failback)", n, maxAttempts)
	}
}

// --- #14b: failback (maxPriority>0), higher-prio lease-held → IsLeaseHeld не потерян ---
// Higher-prio endpoint отдаёт AlreadyExists → агрегат всё равно несёт lease-held
// sentinel в failback-режиме (maxPriority>0 не глотает его в errNoHigherPriority).
// Регресс «failback теряет lease-held sentinel» лишил бы reconnect-loop модест-
// cap-а при попытке вернуться на занятый higher-prio.
func TestRetry_Failback_HigherPrioLeaseHeld_SentinelKept(t *testing.T) {
	higher := newMockEventStream(t, alreadyExistsHandler)
	defer higher.stop()

	cli, err := NewClient(ClientConfig{
		Endpoints:         []Endpoint{{Addr: higher.addr, Priority: 1}},
		SeedCert:          higher.clientCert,
		SeedKey:           higher.clientKey,
		CAPath:            higher.caPath,
		HandshakeTimeout:  3 * time.Second,
		SID:               "host.example",
		MaxAttempts:       5, // lease-held не должен жечь попытки
		InterAttemptDelay: 10 * time.Millisecond,
	}, nil)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	_, err = cli.DialPriority(context.Background(), 2)
	if err == nil {
		t.Fatal("DialPriority: expected error (higher-prio lease-held), got nil")
	}
	if IsNoHigherPriority(err) {
		t.Fatalf("DialPriority err=%v classified NoHigherPriority — lease-held higher-prio замаскирован", err)
	}
	if !IsLeaseHeld(err) {
		t.Fatalf("DialPriority err=%v, want IsLeaseHeld (sentinel потерян в failback-режиме)", err)
	}
	// lease-held → ровно 1 dialOne (non-retriable), попытки не выжжены.
	if n := higher.dialCount(); n != 1 {
		t.Fatalf("higher-prio dialOne %d times, want 1 (lease-held non-retriable)", n)
	}
}

// --- unit: классификатор isRetriablePerEndpoint (матрица architect) ---
func TestIsRetriablePerEndpoint_Matrix(t *testing.T) {
	t.Parallel()
	nonRetriable := []codes.Code{
		codes.AlreadyExists,
		codes.Unauthenticated,
		codes.PermissionDenied,
		codes.InvalidArgument,
		codes.FailedPrecondition,
		codes.Unimplemented,
	}
	for _, c := range nonRetriable {
		if isRetriablePerEndpoint(status.Error(c, "x")) {
			t.Errorf("code %s classified retriable, want NON-retriable", c)
		}
	}
	retriable := []codes.Code{
		codes.Unavailable,
		codes.DeadlineExceeded,
		codes.Internal,
		codes.Unknown,
		codes.Aborted,
	}
	for _, c := range retriable {
		if !isRetriablePerEndpoint(status.Error(c, "x")) {
			t.Errorf("code %s classified non-retriable, want RETRIABLE", c)
		}
	}
	// handshake-timeout — локальный fmt.Errorf (не gRPC-status) → codes.Unknown → retriable.
	if !isRetriablePerEndpoint(fmt.Errorf("handshake timeout 10s")) {
		t.Error("handshake-timeout (plain error) classified non-retriable, want RETRIABLE")
	}
	// неклассифицированный кастомный wrap → retriable (консервативно).
	if !isRetriablePerEndpoint(errors.New("some transport flake")) {
		t.Error("unclassified error classified non-retriable, want RETRIABLE (conservative)")
	}
}
