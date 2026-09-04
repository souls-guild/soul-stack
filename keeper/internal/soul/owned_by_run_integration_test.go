//go:build integration

// Integration tests for OwnedByRun against real Postgres. The predicate is
// SQL-driven (a NOT EXISTS / EXISTS pair over incarnation_membership), so a fake
// pool cannot verify it; and it is a fail-closed security boundary — the one
// thing standing between "converge over my own host" and "take over somebody
// else's identity" (NIM-780) — so it is pinned here, at its own level, rather
// than only through the bootstrap issuance that calls it.
//
// Uses the shared integration_test.go harness (integrationPool / resetAll) and
// the seedIncarnationRow / seedMembership helpers from
// bulk_coven_integration_test.go.

package soul

import (
	"context"
	"testing"
)

func TestIntegration_OwnedByRun_OwnershipBoundary(t *testing.T) {
	const (
		mine   = "redis-sa"
		theirs = "redis-other"
		sid    = "host.example.com"
	)
	cases := []struct {
		name string
		// insertRow is false for the one case where the souls row is absent.
		insertRow bool
		bindTo    []string
		asking    string
		want      bool
	}{
		{
			name:      "unbound row is this run's own — souls are registered before anything binds them",
			insertRow: true,
			asking:    mine,
			want:      true,
		},
		{
			name:      "bound to the asking incarnation",
			insertRow: true,
			bindTo:    []string{mine},
			asking:    mine,
			want:      true,
		},
		{
			name:      "bound only to another incarnation is somebody else's",
			insertRow: true,
			bindTo:    []string{theirs},
			asking:    mine,
			want:      false,
		},
		{
			name:      "bound to both — a host may serve several incarnations (M:N)",
			insertRow: true,
			bindTo:    []string{mine, theirs},
			asking:    mine,
			want:      true,
		},
		{
			// "" is unknown, never "no owner": a caller with no run context must
			// not be able to claim a host somebody bound.
			name:      "unknown incarnation cannot claim a bound row",
			insertRow: true,
			bindTo:    []string{theirs},
			asking:    "",
			want:      false,
		},
		{
			name:      "unknown incarnation still gets the unbound row",
			insertRow: true,
			asking:    "",
			want:      true,
		},
		{
			// The ErrNoRows branch. Unreachable from the bootstrap caller, which
			// holds the row under FOR UPDATE — which is exactly why it needs a test
			// of its own or it is documented behaviour nothing executes.
			name:      "a row that does not exist is not owned",
			insertRow: false,
			asking:    mine,
			want:      false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resetAll(t)
			ctx := context.Background()
			seedIncarnationRow(t, mine)
			seedIncarnationRow(t, theirs)
			if tc.insertRow {
				if err := Insert(ctx, integrationPool, &Soul{
					SID: sid, Transport: TransportAgent, Status: StatusConnected,
				}); err != nil {
					t.Fatalf("insert Soul: %v", err)
				}
				for _, inc := range tc.bindTo {
					seedMembership(t, inc, sid)
				}
			}

			got, err := OwnedByRun(ctx, integrationPool, sid, tc.asking)
			if err != nil {
				t.Fatalf("OwnedByRun: %v", err)
			}
			if got != tc.want {
				t.Errorf("OwnedByRun(%q, asking %q) = %t, want %t", sid, tc.asking, got, tc.want)
			}
		})
	}
}

// The predicate must answer for the SID it was asked about and no other — a
// membership row belonging to a different host is not this one's ownership.
func TestIntegration_OwnedByRun_DoesNotLeakAcrossSIDs(t *testing.T) {
	resetAll(t)
	ctx := context.Background()
	const mine, theirs = "redis-sa", "redis-other"
	seedIncarnationRow(t, mine)
	seedIncarnationRow(t, theirs)

	for _, sid := range []string{"a.example.com", "b.example.com"} {
		if err := Insert(ctx, integrationPool, &Soul{
			SID: sid, Transport: TransportAgent, Status: StatusConnected,
		}); err != nil {
			t.Fatalf("insert %s: %v", sid, err)
		}
	}
	// a belongs to us, b to somebody else. Neither answer may come from the
	// other's row.
	seedMembership(t, mine, "a.example.com")
	seedMembership(t, theirs, "b.example.com")

	ownedA, err := OwnedByRun(ctx, integrationPool, "a.example.com", mine)
	if err != nil {
		t.Fatalf("OwnedByRun(a): %v", err)
	}
	ownedB, err := OwnedByRun(ctx, integrationPool, "b.example.com", mine)
	if err != nil {
		t.Fatalf("OwnedByRun(b): %v", err)
	}
	if !ownedA || ownedB {
		t.Errorf("owned(a)/owned(b) = %t/%t, want true/false", ownedA, ownedB)
	}
}
