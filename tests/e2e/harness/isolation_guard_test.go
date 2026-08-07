//go:build e2e

package harness

import (
	"net"
	"strings"
	"sync"
	"testing"
)

// Guards for the three ways one L3a test could reach into another (NIM-469).
//
// The failures these prevent were never reported as isolation failures. They
// arrived as a 401 on an operator call, an incarnation-state assert reading an
// untouched database, and a panic naming a test that had done nothing wrong —
// three families, a different test each run, every one of them green when run
// alone. That is what makes them worth pinning: each fix is a few lines that a
// later reader can easily mistake for ceremony and simplify away, and nothing
// else in the tree would go red when they did.
//
// No Stack and no containers here, so these run in the e2e job at unit-test
// speed, alongside not_registered_test.go.

// recordingLogger stands in for a *testing.T that has already finished — the
// state a test cannot legally construct for itself.
type recordingLogger struct {
	mu    sync.Mutex
	lines []string
}

func (r *recordingLogger) Logf(format string, args ...any) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.lines = append(r.lines, format)
}

func (r *recordingLogger) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.lines)
}

// TestLogWriter_StopDetachesFromTest pins the guard that keeps a slow keeper
// from failing somebody else's test.
//
// Cleanup gives the keeper 15s to exit, then Kills it and returns — while
// cmd.Wait() is still draining its pipes into this writer. Without stop(), that
// late output reaches a *testing.T whose test has completed, and Go's answer is
// to panic in the name of whichever test is running by then. The bystander gets
// the blame and the real culprit leaves no trace.
func TestLogWriter_StopDetachesFromTest(t *testing.T) {
	rec := &recordingLogger{}
	w := &testLogWriter{t: rec, prefix: "keeper-stderr"}

	if _, err := w.Write([]byte("before cleanup\n")); err != nil {
		t.Fatalf("Write before stop: %v", err)
	}
	if got := rec.count(); got != 1 {
		t.Fatalf("live writer should log: got %d lines, want 1", got)
	}

	w.stop()

	// Exactly the write that used to panic the run: output arriving after the
	// test it belonged to is gone.
	if _, err := w.Write([]byte("keeper still shutting down\nand still\n")); err != nil {
		t.Fatalf("Write after stop: %v", err)
	}
	if got := rec.count(); got != 1 {
		t.Fatalf("writer touched the test after stop(): got %d lines, want 1 — "+
			"a keeper outliving its cleanup will panic the run in another test's name", got)
	}
}

// TestAllocLoopback_HoldsThePortUntilReleased pins the reservation itself.
//
// The point of returning the listener is that the address stays OURS across the
// gap between allocation and the keeper's real bind — a gap that spans a whole
// `keeper init`. If someone restores the old "close it immediately" shape, the
// address goes back to being merely a suggestion, and the two ways of losing it
// are a keeper that cannot bind and a keeper that is not ours answering /readyz.
func TestAllocLoopback_HoldsThePortUntilReleased(t *testing.T) {
	addr, l := allocLoopback(t)
	if l == nil {
		t.Fatal("allocLoopback returned no listener — the reservation is not held")
	}

	// Held: a second bind on the same address must be refused.
	if second, err := net.Listen("tcp", addr); err == nil {
		_ = second.Close()
		_ = l.Close()
		t.Fatalf("%s was bindable while reserved — the reservation does not hold the port", addr)
	}

	// Released: the keeper must be able to take it a moment later.
	if err := l.Close(); err != nil {
		t.Fatalf("close reservation: %v", err)
	}
	after, err := net.Listen("tcp", addr)
	if err != nil {
		t.Fatalf("%s was not bindable after release: %v — the keeper could not start here", addr, err)
	}
	_ = after.Close()
}

// TestStackIdentity_IsPerStack pins that two stacks do not share an identity.
//
// Every stack used to be `keeper-test-01`, so neither a keeper's config nor a
// failure report said which of the forty stacks in a run it came from. Two
// stacks holding two ports must issue under two names.
//
// This is about attribution, not about rejection. A foreign keeper rejects us on
// the signature — its signing key is its own (vault.go, generateHS256Key) and
// the signature is checked before `iss` — so it answers the generic
// `invalid token` either way, and no issuer scheme changes that. assertOwnKeeper
// is what detects the wrong endpoint; this guard only keeps the resulting report
// able to name the stack that produced it.
func TestStackIdentity_IsPerStack(t *testing.T) {
	addrA, lA := allocLoopback(t)
	addrB, lB := allocLoopback(t)
	defer func() { _ = lA.Close(); _ = lB.Close() }()

	if addrA == addrB {
		t.Fatalf("two live reservations returned the same address %s", addrA)
	}

	// Through the production derivation, not a copy of it: a copy would keep
	// passing after someone put the constant back.
	a, b := &Stack{}, &Stack{}
	issuerA := a.assignIdentity("keeper-test", addrA)
	issuerB := b.assignIdentity("keeper-test", addrB)

	if issuerA == issuerB {
		t.Fatalf("two stacks derived the same issuer %q — a failure report from either "+
			"cannot say which stack it came from", issuerA)
	}
	if a.issuer != issuerA || b.issuer != issuerB {
		t.Fatalf("assignIdentity did not record the issuer on the stack: %q/%q vs %q/%q — "+
			"keeper.yml and assertOwnKeeper both read the field", a.issuer, b.issuer, issuerA, issuerB)
	}
	if strings.Contains(issuerA, addrA) {
		t.Fatalf("issuer %q carries the whole address; portOf(%q) did not extract a port",
			issuerA, addrA)
	}
}
