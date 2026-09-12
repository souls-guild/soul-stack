package grpc

import (
	"context"
	"errors"
	"strconv"
	"testing"
	"time"

	keeperv1 "github.com/souls-guild/soul-stack/proto/gen/go/keeper/v1"
)

// rotationHandler builds an EventStream handler whose rotation path reaches a
// counting signer and nothing else — the transaction is never entered, because
// every assertion here is about whether Vault was asked at all.
func rotationHandler(t *testing.T, limit SeedRotationLimit) (*eventStreamHandler, *countingSigner) {
	t.Helper()
	signer := &countingSigner{err: errVaultUnavailableForTest}
	h := newEventStreamHandler(EventStreamDeps{
		Metrics: nil,
		SeedRotation: &SeedRotationDeps{
			Pool: fakeTxBeginner{}, VaultClient: signer, AuditWriter: nopAudit{},
			Outbound: &Outbound{}, KID: "k1", PKIMount: "pki", PKIRole: "soul-seed",
		},
		SeedRotationLimit: limit,
	}, discardLogger(t))
	return h, signer
}

// TestSeedRotation_OverBudgetIsRefusedNotServed is the guard for NIM-840.
//
// `SeedRotationRequest` was the one Soul-driven message with no limiter, and it
// is the most expensive of them: one Vault PKI signature and one PG transaction
// each. Dispatch runs it inline, so a stream is serialised and its concurrency
// was already bounded — rate was not, and rate is what costs.
//
// The assertion is on the SIGNER's call count, which is the resource: over
// budget the request must not reach Vault. Nothing here measures duration, and
// the interval is an hour so no amount of machine slowness turns a refusal back
// into a grant.
func TestSeedRotation_OverBudgetIsRefusedNotServed(t *testing.T) {
	const burst = 2
	h, signer := rotationHandler(t, SeedRotationLimit{
		PerSIDInterval: time.Hour,
		PerSIDBurst:    burst,
		GlobalRate:     1000,
		GlobalBurst:    1000,
	})

	req := &keeperv1.SeedRotationRequest{CsrPem: makeCSRPEM(t, "host.example.com")}
	for range burst + 3 {
		h.handleSeedRotationRequest(context.Background(), "host.example.com", "sess-1", req)
	}

	if signer.calls != burst {
		t.Errorf("SignCSR calls = %d, want %d — requests over the per-SID budget were served", signer.calls, burst)
	}
}

// TestSeedRotation_GlobalBudgetBoundsTheFleet — the per-SID budget alone leaves
// the failure the ticket describes: N hosts each inside their own budget still
// saturate the Vault PKI mount that onboarding issues from, so the damage is
// cluster-wide rather than confined to the host causing it.
func TestSeedRotation_GlobalBudgetBoundsTheFleet(t *testing.T) {
	const globalBurst = 3
	h, signer := rotationHandler(t, SeedRotationLimit{
		PerSIDInterval: time.Hour,
		PerSIDBurst:    10,
		GlobalRate:     0.0001, // effectively no refill for the duration of the test
		GlobalBurst:    globalBurst,
	})

	for i := range globalBurst + 5 {
		// A distinct SID each time: every one of them is inside its own budget.
		sid := "host-" + string(rune('a'+i)) + ".example.com"
		h.handleSeedRotationRequest(context.Background(), sid,
			"sess", &keeperv1.SeedRotationRequest{CsrPem: makeCSRPEM(t, sid)})
	}

	if signer.calls != globalBurst {
		t.Errorf("SignCSR calls = %d, want %d — the fleet was not bounded globally", signer.calls, globalBurst)
	}
}

// TestSeedRotation_MalformedRequestSpendsNoBudget — the limiter sits after the
// free checks. A stream sending garbage would otherwise drain the budget a
// legitimate rotation needs, which turns a limiter into a cheaper attack than
// the one it was added for.
func TestSeedRotation_MalformedRequestSpendsNoBudget(t *testing.T) {
	h, signer := rotationHandler(t, SeedRotationLimit{
		PerSIDInterval: time.Hour,
		PerSIDBurst:    1,
		GlobalRate:     1000,
		GlobalBurst:    1000,
	})

	// A CSR whose CommonName is not the stream's SID — rejected before Vault.
	for range 5 {
		h.handleSeedRotationRequest(context.Background(), "host.example.com", "sess-1",
			&keeperv1.SeedRotationRequest{CsrPem: makeCSRPEM(t, "someone-else.example.com")})
	}
	if signer.calls != 0 {
		t.Fatalf("SignCSR calls = %d, want 0 — a CN mismatch reached Vault", signer.calls)
	}

	h.handleSeedRotationRequest(context.Background(), "host.example.com", "sess-1",
		&keeperv1.SeedRotationRequest{CsrPem: makeCSRPEM(t, "host.example.com")})
	if signer.calls != 1 {
		t.Errorf("SignCSR calls = %d, want 1 — the malformed requests spent the budget", signer.calls)
	}
}

// TestSeedRotationLimiter_RefillsAndReportsFirstRefusalOnce — the budget is a
// rate and not a quota, and the log carries one line per episode: a request
// dropped for costing too much must not then cost a disk write per attempt.
func TestSeedRotationLimiter_RefillsAndReportsFirstRefusalOnce(t *testing.T) {
	l := newSeedRotationLimiter(SeedRotationLimit{
		PerSIDInterval: time.Minute,
		PerSIDBurst:    1,
		GlobalRate:     1000,
		GlobalBurst:    1000,
	})
	base := time.Now()

	if allowed, _, _ := l.allow("host.example.com", base); !allowed {
		t.Fatal("first rotation refused")
	}
	if allowed, _, first := l.allow("host.example.com", base); allowed || !first {
		t.Fatalf("second rotation: allowed=%v firstRefusal=%v, want false/true", allowed, first)
	}
	if allowed, _, first := l.allow("host.example.com", base); allowed || first {
		t.Fatalf("third rotation: allowed=%v firstRefusal=%v, want false/false", allowed, first)
	}
	if allowed, _, _ := l.allow("host.example.com", base.Add(time.Minute)); !allowed {
		t.Error("rotation after the interval refused — the budget is a quota, not a rate")
	}
}

// TestSeedRotationLimiter_GlobalRefusalSpendsNoPerSIDToken — a refusal must
// consume nothing, or the fleet's pressure is paid for out of the victim's
// allowance.
//
// Host A rotates once legitimately while the GLOBAL budget is spent by other
// hosts. Taking A's token before discovering the global budget was empty would
// leave A short for the rest of its interval over traffic that was never its
// own — which is the exact failure the per-SID-first ordering claims to
// prevent. Assertion: once the global budget refills, A still has its full
// burst.
func TestSeedRotationLimiter_GlobalRefusalSpendsNoPerSIDToken(t *testing.T) {
	const perSIDBurst = 2
	l := newSeedRotationLimiter(SeedRotationLimit{
		PerSIDInterval: time.Hour,
		PerSIDBurst:    perSIDBurst,
		GlobalRate:     1, // one per second, so the burst refills at a known time
		GlobalBurst:    1,
	})
	base := time.Now()

	// Somebody else spends the whole global burst.
	if allowed, _, _ := l.allow("noisy.example.com", base); !allowed {
		t.Fatal("the noisy host's first rotation was refused")
	}
	// Host A now meets an empty GLOBAL budget, with its own untouched.
	allowed, global, _ := l.allow("host-a.example.com", base)
	if allowed {
		t.Fatal("host A was served while the global budget was spent")
	}
	if !global {
		t.Error("refusal reported as per-SID; it was the global budget that was empty")
	}

	// A second later the global budget has refilled. A must still hold its
	// FULL burst — the refused attempt above cost it nothing.
	for i := range perSIDBurst {
		if allowed, _, _ := l.allow("host-a.example.com", base.Add(time.Duration(i+1)*time.Second)); !allowed {
			t.Fatalf("host A rotation %d refused — the global refusal burned a per-SID token", i+1)
		}
	}
}

// TestSeedRotationLimiter_GlobalRefusalIsLoggedOncePerFleet — a global refusal
// says nothing about the SID that happened to arrive during it, so one line per
// SID would be one line per host. 5000 hosts over the budget must not be 5000
// warnings.
func TestSeedRotationLimiter_GlobalRefusalIsLoggedOncePerFleet(t *testing.T) {
	l := newSeedRotationLimiter(SeedRotationLimit{
		PerSIDInterval: time.Hour,
		PerSIDBurst:    10,
		GlobalRate:     0.0001,
		GlobalBurst:    1,
	})
	base := time.Now()

	if allowed, _, _ := l.allow("first.example.com", base); !allowed {
		t.Fatal("the first rotation was refused")
	}
	firsts := 0
	for i := range 50 {
		sid := "host-" + strconv.Itoa(i) + ".example.com"
		if _, _, first := l.allow(sid, base); first {
			firsts++
		}
	}
	if firsts != 1 {
		t.Errorf("first-refusal reported %d times across 50 SIDs, want 1", firsts)
	}

	// ...and the throttle must RE-ARM. Suppressing one line per fleet is the
	// point; suppressing every line after the first, forever, would leave an
	// operator with a single warning from whenever the episode began and
	// nothing to say it is still going.
	if _, _, first := l.allow("later.example.com", base.Add(seedRotationGlobalLogEvery)); !first {
		t.Error("the global-refusal warning never re-armed — one line per episode became one line ever")
	}
}

// TestSeedRotationLimiter_NilAllows — a keeper with rotation unwired builds no
// limiter, and the nil receiver must not be what decides the message is
// dropped.
func TestSeedRotationLimiter_NilAllows(t *testing.T) {
	var l *seedRotationLimiter
	if allowed, _, _ := l.allow("host.example.com", time.Now()); !allowed {
		t.Error("nil limiter refused a rotation")
	}
}

// errVaultUnavailableForTest stops the rotation path at Vault, so these tests
// never need a transaction, a fingerprint or an outbound stream. The handler
// treats it like any other rotation failure: log and skip.
var errVaultUnavailableForTest = errors.New("vault unavailable (test)")
