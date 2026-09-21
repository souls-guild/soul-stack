package grpc

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	keeperv1 "github.com/souls-guild/soul-stack/proto/gen/go/keeper/v1"
)

// blockingPool — a BootstrapPool whose token pre-check parks until it is
// released, standing in for a pooled connection held for the duration of a
// query. It counts every read that reached it, which is the quantity the guard
// below is about: a read that reached the pool is a connection an authenticated
// caller could not have.
type blockingPool struct {
	entered chan struct{}
	release chan struct{}

	mu    sync.Mutex
	reads int
}

func newBlockingPool(capacity int) *blockingPool {
	return &blockingPool{
		entered: make(chan struct{}, capacity+8),
		release: make(chan struct{}),
	}
}

func (p *blockingPool) QueryRow(_ context.Context, _ string, _ ...any) pgx.Row {
	p.mu.Lock()
	p.reads++
	p.mu.Unlock()
	p.entered <- struct{}{}
	<-p.release
	return scanErrRow{err: pgx.ErrNoRows}
}

func (p *blockingPool) readCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.reads
}

func (p *blockingPool) Begin(_ context.Context) (pgx.Tx, error) { return nil, nil }

func (p *blockingPool) Exec(_ context.Context, _ string, _ ...any) (pgconn.CommandTag, error) {
	return pgconn.CommandTag{}, nil
}

func (p *blockingPool) Query(_ context.Context, _ string, _ ...any) (pgx.Rows, error) {
	return nil, nil
}

// TestBootstrap_PreauthBudgetRefusesBeforeTouchingThePool is the guard for
// NIM-839.
//
// The Bootstrap listener is the one port an unauthenticated caller can reach by
// design, and its first act is a database read — the token pre-check. Nothing
// bounded how many callers could be doing that at once, so the number of pooled
// connections unauthenticated traffic could hold was the number of connections
// it chose to open.
//
// The assertion is on the REFUSAL and on the read count, not on how long
// anything took: with a budget of two, a third caller comes back
// ResourceExhausted and the pool records two reads, not three. Remove the
// budget in [bootstrapHandler.Bootstrap] and the third read reaches the pool —
// which is the count going to three, with or without a timing window.
//
// PreauthWait is set to a token value because the wait is not the subject here;
// the flood case is the one where a slot never frees, and that is the shape a
// blocked pool reproduces. The burst case — a slot that DOES free — is
// [TestBootstrap_PreauthBudgetServesABurstRatherThanRefusingIt].
func TestBootstrap_PreauthBudgetRefusesBeforeTouchingThePool(t *testing.T) {
	const budget = 2

	pool := newBlockingPool(budget)
	h := newBootstrapHandler(BootstrapDeps{
		Pool: pool, VaultClient: &countingSigner{}, AuditWriter: nopAudit{},
		KID: "k1", PKIMount: "pki", PKIRole: "soul-seed",
		PreauthConcurrency: budget,
		PreauthWait:        50 * time.Millisecond,
	}, discardLogger(t))

	req := &keeperv1.BootstrapRequest{
		Sid:            "host.example.com",
		BootstrapToken: "junk-token",
		CsrPem:         makeCSRPEM(t, "host.example.com"),
	}

	var wg sync.WaitGroup
	for range budget {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _ = h.Bootstrap(context.Background(), req)
		}()
	}
	// Both in-budget callers are inside the pool read before the third is made,
	// so the third meets a spent budget and not a race with one.
	for range budget {
		select {
		case <-pool.entered:
		case <-time.After(5 * time.Second):
			t.Fatal("in-budget Bootstrap never reached the pool")
		}
	}

	done := make(chan error, 1)
	go func() {
		_, err := h.Bootstrap(context.Background(), req)
		done <- err
	}()

	var overBudgetErr error
	select {
	case overBudgetErr = <-done:
	case <-time.After(5 * time.Second):
		// Without the budget the third caller parks in the pool read instead of
		// being refused; report what actually happened rather than the timeout.
		t.Fatalf("over-budget Bootstrap neither returned nor was refused; pool reads = %d, want %d",
			pool.readCount(), budget)
	}

	if got := status.Code(overBudgetErr); got != codes.ResourceExhausted {
		t.Errorf("over-budget Bootstrap: code = %v (err %v), want ResourceExhausted", got, overBudgetErr)
	}
	if got := pool.readCount(); got != budget {
		t.Errorf("pool reads = %d, want %d — the over-budget caller reached a pooled connection", got, budget)
	}

	close(pool.release)
	wg.Wait()
}

// TestBootstrap_PreauthBudgetServesABurstRatherThanRefusingIt is the guard on
// the OTHER half of the budget: it must not turn a legitimate mass onboarding
// into a failure.
//
// `soul bootstrap` is a one-shot CLI — it tries each endpoint exactly once and
// does not retry — so a caller refused the instant the budget is full is a host
// that simply does not onboard, with nothing to pick it back up. An operator
// bringing up 40 hosts at once through a budget of 2 is the ordinary case, not
// an attack, and every one of them must be served.
//
// The contention is arranged, not hoped for: the only slot is held by a caller
// parked inside the pool read, confirmed via pool.entered, so the second caller
// is definitively in the wait when the slot is freed. Make the acquire
// non-blocking again and the second caller comes back ResourceExhausted
// instead — red on the assertion, not on a clock.
func TestBootstrap_PreauthBudgetServesABurstRatherThanRefusingIt(t *testing.T) {
	pool := newBlockingPool(2)
	h := newBootstrapHandler(BootstrapDeps{
		Pool: pool, VaultClient: &countingSigner{}, AuditWriter: nopAudit{},
		KID: "k1", PKIMount: "pki", PKIRole: "soul-seed",
		PreauthConcurrency: 1,
		PreauthWait:        10 * time.Second,
	}, discardLogger(t))

	req := &keeperv1.BootstrapRequest{
		Sid:            "host.example.com",
		BootstrapToken: "junk-token",
		CsrPem:         makeCSRPEM(t, "host.example.com"),
	}

	go func() { _, _ = h.Bootstrap(context.Background(), req) }()
	select {
	case <-pool.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("the in-budget caller never reached the pool")
	}

	// The only slot is taken and will not free until the pool is released.
	second := make(chan codes.Code, 1)
	go func() {
		_, err := h.Bootstrap(context.Background(), req)
		second <- status.Code(err)
	}()

	// Give the second caller time to be refused, if refusing is what it does.
	select {
	case got := <-second:
		t.Fatalf("second caller returned %v while the only slot was held; a burst must wait for a slot, not fail", got)
	case <-time.After(200 * time.Millisecond):
	}

	close(pool.release) // the first RPC finishes and frees its slot
	select {
	case got := <-second:
		if got == codes.ResourceExhausted {
			t.Error("the waiting caller was refused after a slot freed")
		}
	case <-time.After(5 * time.Second):
		t.Error("the waiting caller never took the freed slot")
	}
}

// TestBootstrap_PreauthWaitEndsWhenTheCallerGivesUp — a caller whose context is
// already cancelled must not sit out the whole wait, and must hear its OWN
// reason back.
//
// Two things at once. A flood of clients that hang up would otherwise cost a
// goroutine each for the full wait — the resource the budget protects. And
// reporting a hang-up as ResourceExhausted would put it in the one diagnostic
// an operator has for "the keeper is full", which stops meaning anything if
// every disconnect lands in it.
func TestBootstrap_PreauthWaitEndsWhenTheCallerGivesUp(t *testing.T) {
	pool := newBlockingPool(1)
	h := newBootstrapHandler(BootstrapDeps{
		Pool: pool, VaultClient: &countingSigner{}, AuditWriter: nopAudit{},
		KID: "k1", PKIMount: "pki", PKIRole: "soul-seed",
		PreauthConcurrency: 1,
		PreauthWait:        time.Hour, // never the reason this test finishes
	}, discardLogger(t))

	req := &keeperv1.BootstrapRequest{
		Sid:            "host.example.com",
		BootstrapToken: "junk-token",
		CsrPem:         makeCSRPEM(t, "host.example.com"),
	}

	go func() { _, _ = h.Bootstrap(context.Background(), req) }()
	select {
	case <-pool.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("the in-budget caller never reached the pool")
	}

	gone, cancel := context.WithCancel(context.Background())
	cancel()
	done := make(chan codes.Code, 1)
	go func() {
		_, err := h.Bootstrap(gone, req)
		done <- status.Code(err)
	}()

	select {
	case got := <-done:
		if got != codes.Canceled {
			t.Errorf("cancelled caller: code = %v, want Canceled — a hang-up is not a capacity signal", got)
		}
	case <-time.After(5 * time.Second):
		t.Error("a cancelled caller sat out the wait instead of leaving")
	}
	close(pool.release)
}

// TestBootstrap_PreauthWaitNeverEatsTheCallersWorkBudget is the guard on the
// worst thing the wait can do.
//
// `soul bootstrap` wraps dial, handshake and RPC in ONE deadline, so time spent
// queueing here is time the work does not get. The guarantee under test is
// proportional — at most half — and NOT that the remainder suffices; see
// [bootstrapHandler.acquirePreauth] for why no absolute floor is available.
//
// Here the only slot is held and the caller carries a deadline much shorter
// than the configured wait. It must give up after its SHARE of that deadline,
// leaving the rest for the work it came to do. Delete the share arithmetic and
// the caller instead sits for the configured wait, blowing its own deadline —
// red on the elapsed upper bound.
func TestBootstrap_PreauthWaitNeverEatsTheCallersWorkBudget(t *testing.T) {
	const callerBudget = 2 * time.Second

	pool := newBlockingPool(1)
	h := newBootstrapHandler(BootstrapDeps{
		Pool: pool, VaultClient: &countingSigner{}, AuditWriter: nopAudit{},
		KID: "k1", PKIMount: "pki", PKIRole: "soul-seed",
		PreauthConcurrency: 1,
		PreauthWait:        30 * time.Second, // far longer than the caller has
	}, discardLogger(t))

	req := &keeperv1.BootstrapRequest{
		Sid:            "host.example.com",
		BootstrapToken: "junk-token",
		CsrPem:         makeCSRPEM(t, "host.example.com"),
	}

	go func() { _, _ = h.Bootstrap(context.Background(), req) }()
	select {
	case <-pool.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("the in-budget caller never reached the pool")
	}

	tight, cancel := context.WithTimeout(context.Background(), callerBudget)
	defer cancel()

	start := time.Now()
	_, err := h.Bootstrap(tight, req)
	elapsed := time.Since(start)

	if got := status.Code(err); got != codes.ResourceExhausted {
		t.Errorf("code = %v, want ResourceExhausted", got)
	}
	// The share is half, so anything at or past the caller's whole budget means
	// the queue took time the work needed.
	if elapsed >= callerBudget {
		t.Errorf("refused after %v of a %v budget — the queue ate the caller's work time", elapsed, callerBudget)
	}
	if got := pool.readCount(); got != 1 {
		t.Errorf("pool reads = %d, want 1 — the refused caller reached a pooled connection", got)
	}
	close(pool.release)
}

// TestBootstrap_PreauthWaitSurvivesAShortClientTimeout is the guard on the
// coupling that a fixed reserve would have created.
//
// The caller's budget is `keeper.retry.handshake_timeout` in `soul.yml` — an
// operator knob the keeper cannot read, and one `docs/soul/connection.md`
// advises lowering for faster failover. Subtract a fixed 5s from it and a
// timeout of 5s turns every contended onboarding into an instant refusal: the
// NIM-839 outage restored by a setting in a different file, with the whole
// suite still green because the other tests use an unbounded context.
//
// So this one carries a SHORT deadline on purpose and asserts the caller is
// still served once a slot frees. Reintroduce any fixed reserve at or above
// this deadline and it goes red.
func TestBootstrap_PreauthWaitSurvivesAShortClientTimeout(t *testing.T) {
	pool := newBlockingPool(2)
	h := newBootstrapHandler(BootstrapDeps{
		Pool: pool, VaultClient: &countingSigner{}, AuditWriter: nopAudit{},
		KID: "k1", PKIMount: "pki", PKIRole: "soul-seed",
		PreauthConcurrency: 1,
		PreauthWait:        10 * time.Second,
	}, discardLogger(t))

	req := &keeperv1.BootstrapRequest{
		Sid:            "host.example.com",
		BootstrapToken: "junk-token",
		CsrPem:         makeCSRPEM(t, "host.example.com"),
	}

	go func() { _, _ = h.Bootstrap(context.Background(), req) }()
	select {
	case <-pool.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("the in-budget caller never reached the pool")
	}

	// An operator who set handshake_timeout well under the server's wait.
	short, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	second := make(chan codes.Code, 1)
	started := make(chan struct{})
	go func() {
		close(started) // the goroutine is running, not merely created
		_, err := h.Bootstrap(short, req)
		second <- status.Code(err)
	}()
	<-started
	select {
	case got := <-second:
		t.Fatalf("a caller with a short client timeout was refused (%v) while the slot was merely busy", got)
	case <-time.After(200 * time.Millisecond):
	}

	close(pool.release) // the slot frees well inside the caller's share
	select {
	case got := <-second:
		// PermissionDenied is what a junk token earns once it is SERVED. A
		// weaker `!= ResourceExhausted` would also accept DeadlineExceeded —
		// i.e. a caller that waited and then lost to its own deadline, which is
		// the failure this whole guard is about.
		if got != codes.PermissionDenied {
			t.Errorf("code = %v, want PermissionDenied — the waiting caller was not served after a slot freed", got)
		}
	case <-time.After(5 * time.Second):
		t.Error("the waiting caller never took the freed slot")
	}
}

// TestBootstrap_AnExpiredCallerIsNotACapacitySignal — a caller whose deadline
// passed before Keeper read its request must not be counted as "at capacity".
//
// The scenario is ordinary: a network hiccup, or an overloaded Soul, pushes a
// batch of onboardings past `handshake_timeout` in flight. Classifying those as
// capacity refusals tells the operator to add capacity for a problem that is
// entirely client-side — and it is the one diagnostic that separates the two,
// so polluting it costs more than the wrong code on a request nobody will read.
//
// The budget is deliberately contended, so this exercises the path a spent
// budget takes rather than the uncontended fast path.
func TestBootstrap_AnExpiredCallerIsNotACapacitySignal(t *testing.T) {
	pool := newBlockingPool(1)
	h := newBootstrapHandler(BootstrapDeps{
		Pool: pool, VaultClient: &countingSigner{}, AuditWriter: nopAudit{},
		KID: "k1", PKIMount: "pki", PKIRole: "soul-seed",
		PreauthConcurrency: 1,
		PreauthWait:        10 * time.Second,
	}, discardLogger(t))

	req := &keeperv1.BootstrapRequest{
		Sid:            "host.example.com",
		BootstrapToken: "junk-token",
		CsrPem:         makeCSRPEM(t, "host.example.com"),
	}

	go func() { _, _ = h.Bootstrap(context.Background(), req) }()
	select {
	case <-pool.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("the in-budget caller never reached the pool")
	}

	expired, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancel()

	_, err := h.Bootstrap(expired, req)
	if got := status.Code(err); got != codes.DeadlineExceeded {
		t.Errorf("code = %v, want DeadlineExceeded — an expired caller was reported as a capacity refusal", got)
	}
	if got := pool.readCount(); got != 1 {
		t.Errorf("pool reads = %d, want 1 — the expired caller reached a pooled connection", got)
	}
	close(pool.release)
}

// TestBootstrap_RefusalLogReportsTheWaitItTook — the refusal log is the only
// diagnostic that separates "add capacity" from "raise handshake_timeout", and
// it only does that if `waited` is measured rather than assumed.
func TestBootstrap_RefusalLogReportsTheWaitItTook(t *testing.T) {
	pool := newBlockingPool(1)
	h := newBootstrapHandler(BootstrapDeps{
		Pool: pool, VaultClient: &countingSigner{}, AuditWriter: nopAudit{},
		KID: "k1", PKIMount: "pki", PKIRole: "soul-seed",
		PreauthConcurrency: 1,
		PreauthWait:        10 * time.Second,
	}, discardLogger(t))

	// Hold the only slot so the next acquire has to wait it out.
	held, waited, err := h.acquirePreauth(context.Background())
	if err != nil {
		t.Fatalf("first acquire: %v", err)
	}
	if waited != 0 {
		t.Errorf("uncontended acquire reported waited=%v, want 0", waited)
	}
	defer held()

	// A caller whose own deadline is the binding constraint: its share is
	// 200ms, far under the configured 10s.
	short, cancel := context.WithTimeout(context.Background(), 400*time.Millisecond)
	defer cancel()
	_, waited, err = h.acquirePreauth(short)
	if !errors.Is(err, errPreauthBudgetSpent) {
		t.Fatalf("contended acquire: err = %v, want errPreauthBudgetSpent", err)
	}
	if waited >= h.preauthWait {
		t.Errorf("waited = %v — the configured wait was reported instead of the share actually taken", waited)
	}
	if waited <= 0 {
		t.Errorf("waited = %v, want a positive measured duration", waited)
	}
	close(pool.release)
}

// TestBootstrap_PreauthBudgetIsReleased — the budget is a ceiling on work in
// flight, not a quota for the lifetime of the process. Without the deferred
// release a keeper would refuse every Bootstrap after the first N, which is the
// same outage as the one being fixed, arriving on its own.
func TestBootstrap_PreauthBudgetIsReleased(t *testing.T) {
	h := newBootstrapHandler(BootstrapDeps{
		Pool: vaultPoolFake{notFound: true}, VaultClient: &countingSigner{}, AuditWriter: nopAudit{},
		KID: "k1", PKIMount: "pki", PKIRole: "soul-seed",
		PreauthConcurrency: 1,
	}, discardLogger(t))

	req := &keeperv1.BootstrapRequest{
		Sid:            "host.example.com",
		BootstrapToken: "junk-token",
		CsrPem:         makeCSRPEM(t, "host.example.com"),
	}

	for i := range 3 {
		_, err := h.Bootstrap(context.Background(), req)
		// A junk token is PermissionDenied; anything at capacity would be
		// ResourceExhausted, and after a serial call there is nothing in flight.
		if got := status.Code(err); got != codes.PermissionDenied {
			t.Fatalf("attempt %d: code = %v (err %v), want PermissionDenied", i, got, err)
		}
	}
}

// TestBootstrap_PreauthBudgetDefaults — a caller that sets no budget gets the
// default one rather than an unbounded handler. A nil channel would make
// acquirePreauth allow everything, which is the pre-NIM-839 behaviour dressed
// as a configured limit.
func TestBootstrap_PreauthBudgetDefaults(t *testing.T) {
	h := newBootstrapHandler(BootstrapDeps{
		Pool: fakeTxBeginner{}, VaultClient: fakeSigner{}, AuditWriter: nopAudit{},
		KID: "k1", PKIMount: "pki", PKIRole: "soul-seed",
	}, discardLogger(t))

	if h.preauth == nil {
		t.Fatal("preauth budget is nil — the handler is unbounded")
	}
	if got := cap(h.preauth); got != defaultBootstrapPreauthConcurrency {
		t.Errorf("preauth budget = %d, want the default %d", got, defaultBootstrapPreauthConcurrency)
	}
	// A zero wait would make the acquire non-blocking again and turn every
	// burst into a refusal, so the default must be a real duration.
	if h.preauthWait != bootstrapPreauthWait || h.preauthWait <= 0 {
		t.Errorf("preauth wait = %v, want the default %v (positive)", h.preauthWait, bootstrapPreauthWait)
	}
}
