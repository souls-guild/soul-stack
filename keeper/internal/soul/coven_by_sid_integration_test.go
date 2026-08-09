//go:build integration

// Guard tests on a REAL Postgres for [CovenBySID] — the one-column read the
// per-host RBAC gate does before `soul.forget` / `soul.issue-token` /
// `soul.ssh-target-update` reach their handlers (NIM-588).
//
// Live PG rather than a fake, because every fake in the unit suites answers this
// read from the same Go value the test wrote. That proves the plumbing and says
// nothing about the two things only a database can refute: that `souls.coven`
// (`text[]`) scans into a `[]string` at all, and that an absent row surfaces as
// `pgx.ErrNoRows` rather than as an empty list. Get either wrong and the gate is
// still green everywhere it is faked while denying — or admitting — for real.
//
// Run:
//
//	SOUL_STACK_INTEGRATION_REQUIRE_DOCKER=1 go test -tags=integration -count=1 -p 1 ./internal/soul/

package soul

import (
	"context"
	"errors"
	"sort"
	"testing"
)

// TestIntegration_CovenBySID_ReadsTheHostsOwnLabels — GUARD: the gate's read
// returns exactly the labels an operator attached to that host, in full. A host
// carries a LIST (ADR-008), and the gate asks the enforcer once per label, so a
// read that returned only the first would silently confine every multi-coven
// host to one of its covens.
func TestIntegration_CovenBySID_ReadsTheHostsOwnLabels(t *testing.T) {
	resetAll(t)
	ctx := context.Background()

	seedBulkSoul(t, "multi.example.com", []string{"web", "eu-bastioned", "canary"})
	seedBulkSoul(t, "other.example.com", []string{"db"})

	got, err := CovenBySID(ctx, integrationPool, "multi.example.com")
	if err != nil {
		t.Fatalf("CovenBySID(multi.example.com): %v", err)
	}
	sorted := append([]string(nil), got...)
	sort.Strings(sorted)
	want := []string{"canary", "eu-bastioned", "web"}
	if len(sorted) != len(want) {
		t.Fatalf("coven = %v, want %v — the gate asks once per label, so a truncated read narrows the grant it was meant to honour",
			got, want)
	}
	for i := range want {
		if sorted[i] != want[i] {
			t.Fatalf("coven = %v, want %v", got, want)
		}
	}

	// The read is per-host: a neighbour's labels never leak into the context
	// this host is authorized against.
	neighbour, err := CovenBySID(ctx, integrationPool, "other.example.com")
	if err != nil {
		t.Fatalf("CovenBySID(other.example.com): %v", err)
	}
	if len(neighbour) != 1 || neighbour[0] != "db" {
		t.Errorf("coven(other) = %v, want [db]", neighbour)
	}
}

// TestIntegration_CovenBySID_LabelLessHostIsEmptyNotMissing — GUARD: a
// registered host with no covens reads back as an empty list and NOT as an
// error. The two are different answers at the gate: empty means "assert the host
// dimension alone, and let a `coven=` grant fail closed", while an error is the
// unreadable-row path. A driver that turned `'{}'` into `ErrNoRows` would route a
// perfectly present host down the fallback branch.
func TestIntegration_CovenBySID_LabelLessHostIsEmptyNotMissing(t *testing.T) {
	resetAll(t)
	ctx := context.Background()

	seedBulkSoul(t, "bare.example.com", nil)

	got, err := CovenBySID(ctx, integrationPool, "bare.example.com")
	if err != nil {
		t.Fatalf("CovenBySID(bare.example.com) = %v, want no error — the host exists, it simply carries no label", err)
	}
	if len(got) != 0 {
		t.Errorf("coven = %v, want empty", got)
	}
}

// TestIntegration_CovenBySID_UnknownHostIsErrSoulNotFound — GUARD: a SID with no
// row answers ErrSoulNotFound, distinguishable from "exists, no labels" above.
// The selector maps this onto the host-only context, so a forged SID cannot
// produce a context that satisfies a `coven=` grant.
func TestIntegration_CovenBySID_UnknownHostIsErrSoulNotFound(t *testing.T) {
	resetAll(t)
	ctx := context.Background()

	got, err := CovenBySID(ctx, integrationPool, "ghost.example.com")
	if !errors.Is(err, ErrSoulNotFound) {
		t.Fatalf("CovenBySID(ghost) = (%v, %v), want ErrSoulNotFound — an absent row must not read as a host with no covens",
			got, err)
	}
}

// TestIntegration_CovenBySID_AgreesWithTheRowTheHandlerReads — GUARD: the gate
// and the handler must see one host. They run different queries against the same
// row — the gate a one-column read before the middleware chain ends, the handler
// a full SelectBySID — and the moment those disagree an operator is authorized
// against a coven set the operation then does not act on.
func TestIntegration_CovenBySID_AgreesWithTheRowTheHandlerReads(t *testing.T) {
	resetAll(t)
	ctx := context.Background()

	seedBulkSoul(t, "agree.example.com", []string{"web", "prod"})

	gate, err := CovenBySID(ctx, integrationPool, "agree.example.com")
	if err != nil {
		t.Fatalf("CovenBySID: %v", err)
	}
	full, err := SelectBySID(ctx, integrationPool, "agree.example.com")
	if err != nil {
		t.Fatalf("SelectBySID: %v", err)
	}

	gateSorted := append([]string(nil), gate...)
	sort.Strings(gateSorted)
	handlerSorted := append([]string(nil), full.Coven...)
	sort.Strings(handlerSorted)

	if len(gateSorted) != len(handlerSorted) {
		t.Fatalf("gate sees %v, handler sees %v — two reads of one row must be one answer", gate, full.Coven)
	}
	for i := range gateSorted {
		if gateSorted[i] != handlerSorted[i] {
			t.Fatalf("gate sees %v, handler sees %v", gate, full.Coven)
		}
	}
}
