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

// codeHandler is a handshake handler that always rejects Hello with the given
// gRPC code. Models a keeper returning a non-retriable failure (auth/contract).
func codeHandler(code codes.Code, msg string) func(*keeperv1.Hello) (*keeperv1.HelloReply, error) {
	return func(_ *keeperv1.Hello) (*keeperv1.HelloReply, error) {
		return nil, status.Errorf(code, "%s", msg)
	}
}

// failThenOKHandler returns a handler that returns the given retriable code
// `failCount` times, then a successful HelloReply. Counter is closure-scoped,
// tracking handler calls (= dialOne attempts to this server).
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

// Regression guard (7af8e95): AlreadyExists (lease-held) is non-retriable —
// dialOne fires EXACTLY once per endpoint, then sprays to the next. Churn
// (retrying a held lease) must not return.
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
		MaxAttempts:       5, // headroom: lease-held must not burn attempts
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

// ALL endpoints AlreadyExists → IsLeaseHeld (accumulator not broken).
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
	// each endpoint dialed exactly once (not MaxAttempts).
	if n := a.dialCount(); n != 1 {
		t.Errorf("endpoint a dialOne %d times, want 1", n)
	}
	if n := b.dialCount(); n != 1 {
		t.Errorf("endpoint b dialOne %d times, want 1", n)
	}
}

// max_attempts=1 → exactly one attempt per endpoint (pre-fix behavior).
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

// Unavailable (maxAttempts-1) times, then success → connected.
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

// max_attempts=0 (omitted) → resolves to 2 (NewClient), not 0/infinite.
func TestRetry_ZeroResolvesToDefault(t *testing.T) {
	cli, err := NewClient(ClientConfig{
		Endpoints: []Endpoint{{Addr: "k:9443"}},
		SeedCert:  "/c", SeedKey: "/k", CAPath: "/a",
		SID: "host.example",
		// MaxAttempts omitted → 0
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

// fail-fast: PermissionDenied / InvalidArgument → dialOne once, then spray.
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

// endpoint X returns Unavailable maxAttempts times → falls over to Y (success).
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

// failback (maxPriority>0) → errNoHigherPriority is NOT masked.
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
	// retry-loop never touches the skipped endpoint: dialOne wasn't called at all.
	if n := srv.dialCount(); n != 0 {
		t.Errorf("skipped endpoint dialOne %d times, want 0", n)
	}
}

// pause between attempts on the same endpoint is honored, not a tight loop.
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
		// jitter off — deterministic lower bound.
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
	// maxAttempts attempts = (maxAttempts-1) pauses between them.
	wantMin := time.Duration(maxAttempts-1) * delay
	if elapsed < wantMin {
		t.Fatalf("elapsed %s < %s (tight-loop: inter-attempt delay not observed)", elapsed, wantMin)
	}
	if n := srv.dialCount(); n != maxAttempts {
		t.Errorf("dialOne %d times, want %d", n, maxAttempts)
	}
}

// --- ctx cancel interrupts the inter-attempt pause (doesn't block the full window) ---
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
		InterAttemptDelay: 10 * time.Second, // long pause — cancel fires first
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

// failover-latency bound on a hung endpoint (handshake-timeout every attempt).
// A hang-handler endpoint times out via local handshake_timeout on EVERY
// attempt (handshake timeout → codes.Unknown → retriable). max_attempts=2 →
// two full handshake_timeout pauses + one inter-attempt pause, then spray to
// the live endpoint. Pins the upper bound of failover latency: catches
// regressions where "handshake-timeout became non-retriable" (1x instead of
// 2x) or "default max_attempts got bumped" (>2x, dialCount>2). Comparison is
// relative to handshake_timeout with generous tolerance to avoid flakiness.
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
	// hung endpoint dialed EXACTLY maxAttempts times (handshake-timeout retriable,
	// default not raised).
	if n := hung.dialCount(); n != maxAttempts {
		t.Fatalf("hung endpoint dialOne %d times, want %d (handshake-timeout retriable? default raised?)", n, maxAttempts)
	}
	// Lower bound: each attempt blocks at least handshake_timeout (handshake
	// never responds) → maxAttempts handshake_timeouts. Inter-attempt delay is
	// excluded from the lower bound (jitter/scheduler); it only adds on top.
	wantMin := time.Duration(maxAttempts) * hsTimeout
	if elapsed < wantMin {
		t.Fatalf("elapsed %s < %s - hung endpoint did not wait out handshake_timeout each attempt", elapsed, wantMin)
	}
	// Upper bound: maxAttempts handshake_timeouts + (maxAttempts-1) inter-attempt
	// pauses + one live handshake + overhead. Generous multiplier catches
	// regressions where retry exceeds maxAttempts or handshake_timeout stops
	// working.
	wantMax := time.Duration(maxAttempts)*hsTimeout + time.Duration(maxAttempts-1)*interAttempt + 3*time.Second
	if elapsed > wantMax {
		t.Fatalf("elapsed %s > %s - failover latency exceeded the bound (extra attempts / raised default / non-retriable)", elapsed, wantMax)
	}
}

// ctx cancel DURING a hung dialOne (not during the pause).
// dialOne blocks on handshake (hang-handler), handshake_timeout is large (5s).
// We cancel ctx after ~150ms. sessCtx derives from ctx → its cancel closes the
// stream → the handshake goroutine in dialOne gets a recv error and hsDone
// fires FAST (not via time.After(handshake_timeout)). Pins a non-obvious
// mechanism: dialOne reacts to ctx cancellation through the derived sessCtx,
// even though the local select watches time.After, not ctx directly. A
// regression where sessCtx stops deriving from ctx, or handshake stops being
// interrupted by the stream closing, would hang Dial for ~handshake_timeout.
// Differs from TestRetry_CtxCancelInterruptsPause, which covers the
// inter-attempt pause.
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
		InterAttemptDelay: 10 * time.Second, // long pause — cancel must land inside dialOne, not here
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
	// Returned fast via stream teardown, not handshake_timeout (5s) or the
	// inter-attempt pause (10s). Tolerance is generous (1s) but an order of
	// magnitude below both.
	if elapsed > 1*time.Second {
		t.Fatalf("Dial blocked %s - ctx cancel did not interrupt the hung dialOne (fell through to handshake_timeout %s?)", elapsed, hsTimeout)
	}
	// dialOne attempted exactly once — cancel landed on the first attempt,
	// before the inter-attempt pause / second attempt.
	if n := hung.dialCount(); n != 1 {
		t.Errorf("hung endpoint dialOne %d times, want 1 (cancel landed on the first attempt)", n)
	}
}

// mixed errors on one endpoint: transient → AlreadyExists.
// Single endpoint: attempt 1 = Unavailable (retriable, retry), attempt 2 =
// AlreadyExists (non-retriable, break). dialCount==2. allLeaseHeld considers
// the endpoint's LAST error — AlreadyExists → IsLeaseHeld==true (deliberate
// design: "the endpoint's last error decides the accumulator", per QA
// observation).
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
		MaxAttempts:       5, // headroom: break after AlreadyExists, attempts aren't burned
		InterAttemptDelay: 10 * time.Millisecond,
	}, nil)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	_, err = cli.Dial(context.Background())
	if err == nil {
		t.Fatal("Dial: expected error, got nil")
	}
	// Exactly 2 dialOne calls: transient retried, 2nd attempt AlreadyExists → break
	// (never reached 3..5).
	if n := srv.dialCount(); n != 2 {
		t.Fatalf("dialOne %d times, want EXACTLY 2 (transient retried, AlreadyExists broke before exhausting attempts)", n)
	}
	// The endpoint's last error is AlreadyExists, single endpoint → aggregate
	// = lease-held (accumulator depends on the LAST error).
	if !IsLeaseHeld(err) {
		t.Fatalf("Dial err=%v, want IsLeaseHeld (last error of endpoint = AlreadyExists, accumulator must decide by it)", err)
	}
}

// AlreadyExists on the very first attempt → break immediately (dialCount==1).
// Confirms the contrast with the previous test: without a transient preamble,
// AlreadyExists breaks the retry loop right away, no second attempt.
// lease-held is preserved.
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
		t.Fatalf("dialOne %d times, want EXACTLY 1 (AlreadyExists on the 1st attempt -> break immediately)", n)
	}
	if !IsLeaseHeld(err) {
		t.Fatalf("Dial err=%v, want IsLeaseHeld", err)
	}
}

// failback (maxPriority>0), higher-prio endpoint EXISTS but fails retriably.
// The higher-prio endpoint (priority=1) returns Unavailable (retriable) on
// every attempt → the retry loop exhausts max_attempts on it, then
// DialPriority returns the REAL "all endpoints failed" aggregate, not
// errNoHigherPriority (TestRetry_FailbackNoHigherPriority_NotMasked only
// covered the skipped-endpoint case, where dialOne is never called).
// A regression masking a higher-prio failure as NoHigherPriority would be
// silent: the failback loop would conclude "nothing to fall back to" instead
// of "higher-prio is temporarily down".
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
	// maxPriority=2 → priority=1 endpoint is in scope (1 < 2), but fails.
	_, err = cli.DialPriority(context.Background(), 2)
	if err == nil {
		t.Fatal("DialPriority: expected error (higher-prio retriable-down), got nil")
	}
	// NOT masked as NoHigherPriority: the endpoint was actually tried and failed.
	if IsNoHigherPriority(err) {
		t.Fatalf("DialPriority err=%v classified NoHigherPriority - higher-prio failure was masked (failback thought there was nowhere to return to)", err)
	}
	// retry loop exhausted max_attempts on higher-prio (Unavailable retriable).
	if n := higher.dialCount(); n != maxAttempts {
		t.Fatalf("higher-prio dialOne %d times, want %d (exhausted retriable in failback)", n, maxAttempts)
	}
}

// failback (maxPriority>0), higher-prio lease-held → IsLeaseHeld not lost.
// Higher-prio endpoint returns AlreadyExists → the aggregate still carries the
// lease-held sentinel in failback mode (maxPriority>0 doesn't swallow it into
// errNoHigherPriority). A regression where failback loses the lease-held
// sentinel would strip the reconnect loop's backoff cap when it tries to fall
// back to a held higher-prio endpoint.
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
		MaxAttempts:       5, // lease-held must not burn attempts
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
		t.Fatalf("DialPriority err=%v classified NoHigherPriority - lease-held higher-prio was masked", err)
	}
	if !IsLeaseHeld(err) {
		t.Fatalf("DialPriority err=%v, want IsLeaseHeld (sentinel lost in failback mode)", err)
	}
	// lease-held → exactly 1 dialOne (non-retriable), attempts not burned.
	if n := higher.dialCount(); n != 1 {
		t.Fatalf("higher-prio dialOne %d times, want 1 (lease-held non-retriable)", n)
	}
}

// --- unit: isRetriablePerEndpoint classifier (architect's matrix) ---
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
	// handshake-timeout is a plain fmt.Errorf (not a gRPC status) → codes.Unknown → retriable.
	if !isRetriablePerEndpoint(fmt.Errorf("handshake timeout 10s")) {
		t.Error("handshake-timeout (plain error) classified non-retriable, want RETRIABLE")
	}
	// unclassified custom wrap → retriable (conservative default).
	if !isRetriablePerEndpoint(errors.New("some transport flake")) {
		t.Error("unclassified error classified non-retriable, want RETRIABLE (conservative)")
	}
}
