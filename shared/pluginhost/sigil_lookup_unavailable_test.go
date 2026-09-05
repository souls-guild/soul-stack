package pluginhost

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// A grant lookup that FAILED is not a grant that is ABSENT (NIM-814).
//
// Both refuse the spawn and that part is not in question — fail-closed is the decision
// and it stays. What changes is the reason. The Keeper's lookup reads plugin_sigils
// from Postgres, and a read error used to be flattened into a nil record, which verify
// reads as no_sigil, whose hint is:
//
//	plugin "redis" is not allowed; run `keeper.plugin.allow alias=redis …`
//
// So a database outage reached the operator as a missing approval, with the remedy
// "issue one" — a supply-chain gate widened to work around an unrelated failure. The
// registry has no opinion about that alias at all; nobody asked it anything.

// countingLookup answers correctly and records how many times it was asked, so "one
// registry read per Spawn" is a measured number and not a claim.
type countingLookup struct {
	recs  map[string]*SigilRecord
	calls int
	ctxs  []context.Context
}

func (l *countingLookup) Get(ctx context.Context, alias string) (*SigilRecord, error) {
	l.calls++
	l.ctxs = append(l.ctxs, ctx)
	return l.recs[alias], nil
}

func TestSigilVerifyLookupFailureIsNotNoSigil(t *testing.T) {
	e := setupSigilEnv(t)
	h := e.host(t, false)
	dbDown := errors.New("dial tcp 10.0.0.5:5432: connect: connection refused")
	h.Sigils = lookupErrStub{err: dbDown}

	_, err := h.Spawn(context.Background(), e.discovered)
	ve := asVerifyError(t, err)
	if ve.Reason != VerifyReasonLookupUnavailable {
		t.Fatalf("reason = %q, want %q — a registry that could not be read is not an approval that was never issued",
			ve.Reason, VerifyReasonLookupUnavailable)
	}
	// The hint must not send the operator to issue a grant.
	if strings.Contains(ve.Hint, "keeper.plugin.allow") {
		t.Errorf("hint tells the operator to issue an allow for a lookup failure: %q", ve.Hint)
	}
	if !strings.Contains(ve.Hint, "UNKNOWN") {
		t.Errorf("hint does not say the answer is unknown: %q", ve.Hint)
	}
	// It stays a Sigil-verify failure for every caller that classifies one.
	if !errors.Is(err, ErrSigilVerify) {
		t.Error("a lookup failure must still be an ErrSigilVerify — fail-closed is unchanged")
	}
	// And the cause stays reachable, so a cancelled run is still recognisable as one.
	if !errors.Is(err, dbDown) {
		t.Errorf("the underlying read error is not reachable through the returned error: %v", err)
	}
	// …without being printed at everyone: the message reaches an operator through task
	// output, and a database error carries addresses and query text.
	if strings.Contains(err.Error(), "10.0.0.5") {
		t.Errorf("the database error text leaked into the plugin-refusal message: %q", err.Error())
	}
}

// A cancelled run must be recognisable as one rather than as a supply-chain refusal
// wearing a generic reason. This is the half of NIM-814 that the lost context caused:
// with the lookup on context.Background(), the query neither saw the run's deadline nor
// noticed its cancellation.
func TestSigilVerifyLookupCancellationIsVisible(t *testing.T) {
	e := setupSigilEnv(t)
	h := e.host(t, false)
	h.Sigils = lookupErrStub{err: context.DeadlineExceeded}

	_, err := h.Spawn(context.Background(), e.discovered)
	ve := asVerifyError(t, err)
	if ve.Reason != VerifyReasonLookupUnavailable {
		t.Fatalf("reason = %q, want %q", ve.Reason, VerifyReasonLookupUnavailable)
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Error("a lookup that timed out must stay distinguishable from one that answered `no`")
	}
}

// The spawn's own context reaches the lookup. Nothing downstream can restore a deadline
// the lookup never received, so this is checked at the boundary where it is passed.
func TestSigilVerifyLookupRunsOnTheSpawnContext(t *testing.T) {
	type ctxKey struct{}
	e := setupSigilEnv(t)
	h := e.host(t, false)
	look := &countingLookup{recs: map[string]*SigilRecord{e.rec.Alias: e.rec}}
	h.Sigils = look

	ctx, cancel := context.WithCancel(context.WithValue(context.Background(), ctxKey{}, "spawn"))
	defer cancel()
	_, err := h.Spawn(ctx, e.discovered)
	if errors.Is(err, ErrSigilVerify) {
		t.Fatalf("verify must pass for a valid grant, got %v", err)
	}
	if look.calls != 1 {
		t.Fatalf("registry reads = %d, want exactly 1 per Spawn", look.calls)
	}
	if look.ctxs[0].Value(ctxKey{}) != "spawn" {
		t.Fatal("the lookup ran on some other context — context.Background() there outlives the run and ignores its deadline")
	}
	cancel()
	if look.ctxs[0].Err() == nil {
		t.Error("cancelling the spawn did not cancel the context the lookup ran on")
	}
}

// An absent grant keeps its own reason and its own actionable hint. The point of the
// split is that BOTH answers stay precise, not that one of them absorbs the other.
func TestSigilVerifyAbsentGrantStillSaysNoSigil(t *testing.T) {
	e := setupSigilEnv(t)
	h := e.host(t, false)

	_, err := h.Spawn(context.Background(), e.discovered)
	ve := asVerifyError(t, err)
	if ve.Reason != VerifyReasonNoSigil {
		t.Fatalf("reason = %q, want %q", ve.Reason, VerifyReasonNoSigil)
	}
	if !strings.Contains(ve.Hint, "keeper.plugin.allow") {
		t.Errorf("a genuinely missing grant must still tell the operator how to issue one: %q", ve.Hint)
	}
	if ve.Cause != nil {
		t.Errorf("no_sigil is a verdict, not a wrapped failure: Cause = %v", ve.Cause)
	}
}
