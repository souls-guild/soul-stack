//go:build e2e

package harness

// Helpers for live-crash proof of two recovery-backstop findings (ADR-027
// amend (m)/(n), slice S3):
//
//   - reconcile_orphan_applying — releases an orphaned applying-lock from a
//     standalone run whose owning keeper crashed (Reaper rule, presence-gated);
//   - eventstream.lease_force_released — presence-gated takeover of a SID
//     lease from a provably dead holder when the same SID reconnects to a
//     live keeper.
//
// All read-helpers are narrow SQL projections (status / epoch columns /
// audit rows) over the shared s.db. Polling wrappers use the Eventually
// pattern (deadline + short poll tick), not a fixed sleep for the full TTL.

import (
	"context"
	"testing"
	"time"
)

// Eventually polls cond until true or until timeout (poll tick 100ms).
// Fatal with msg on timeout. A generic Eventually pattern for asserts that
// just need a "state reached" predicate, without a dedicated read helper.
func (s *Stack) Eventually(t *testing.T, timeout time.Duration, cond func() bool, msg string) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("Eventually: %s (not satisfied within %s)", msg, timeout)
}

// IncarnationApplyingEpoch — snapshot of the applying-lock epoch columns of
// an incarnation (migration 082). A nil pointer means the column is NULL in
// PG (epoch cleared or never written). For asserting "after releasing the
// orphan lock, the epoch is nulled out".
type IncarnationApplyingEpoch struct {
	ApplyID *string
	Attempt *int
	ByKID   *string
	// SinceSet — true if applying_since is NOT NULL (the exact time does not
	// matter to the assert, only the fact "epoch present / cleared" does).
	SinceSet bool
}

// IncarnationApplyingEpochSnapshot reads the applying-lock epoch columns of
// an incarnation from PG. Fatal if the row does not exist.
func (s *Stack) IncarnationApplyingEpochSnapshot(t *testing.T, name string) IncarnationApplyingEpoch {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var (
		ep    IncarnationApplyingEpoch
		since *time.Time
	)
	if err := s.db.QueryRow(ctx,
		`SELECT applying_apply_id, applying_attempt, applying_by_kid, applying_since
		   FROM incarnation WHERE name = $1`,
		name).Scan(&ep.ApplyID, &ep.Attempt, &ep.ByKID, &since); err != nil {
		t.Fatalf("IncarnationApplyingEpochSnapshot(%s): %v", name, err)
	}
	ep.SinceSet = since != nil
	return ep
}

// EpochCleared — true if ALL applying-lock epoch columns are nulled out
// (applying_apply_id / applying_attempt / applying_by_kid / applying_since).
// A sign that reconcile_orphan_applying OR an honest terminal state fully
// released the lock (ReleaseApplyingOrphan clears the epoch in the same tx
// as status->ready).
func (ep IncarnationApplyingEpoch) EpochCleared() bool {
	return ep.ApplyID == nil && ep.Attempt == nil && ep.ByKID == nil && !ep.SinceSet
}

// WaitIncarnationStatus polls incarnation.status until it reaches one of
// the want statuses, or until timeout. Returns the reached status. Fatal on
// timeout with the last observed status. An Eventually wrapper for
// recovery transitions applying->ready.
func (s *Stack) WaitIncarnationStatus(t *testing.T, name string, want []string, timeout time.Duration) string {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var last string
	for time.Now().Before(deadline) {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		err := s.db.QueryRow(ctx, "SELECT status FROM incarnation WHERE name = $1", name).Scan(&last)
		cancel()
		if err == nil {
			for _, w := range want {
				if last == w {
					return last
				}
			}
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatalf("WaitIncarnationStatus(%s): status %v not reached within %s (last=%q)", name, want, timeout, last)
	return ""
}

// CountAuditEventsByPayload returns the number of audit_log entries with a
// given event_type where payload->>field = value. For
// reconcile_orphan_applying.executed (field="incarnation") and
// eventstream.lease_force_released (field="sid") — both are keyed by
// something other than voyage_id, so CountAuditEvents (voyage_id-only)
// doesn't fit them.
func (s *Stack) CountAuditEventsByPayload(t *testing.T, eventType, field, value string) int {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var n int
	if err := s.db.QueryRow(ctx, `
		SELECT COUNT(*) FROM audit_log
		WHERE event_type = $1 AND payload->>$2 = $3`,
		eventType, field, value).Scan(&n); err != nil {
		t.Fatalf("CountAuditEventsByPayload(%s, %s=%s): %v", eventType, field, value, err)
	}
	return n
}

// WaitAuditEventByPayload polls audit_log until at least 1 entry appears
// (event_type + payload->>field=value), or until timeout. Returns true when
// it appears. Fatal on timeout. An Eventually wrapper around
// CountAuditEventsByPayload.
func (s *Stack) WaitAuditEventByPayload(t *testing.T, eventType, field, value string, timeout time.Duration) bool {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if s.CountAuditEventsByPayload(t, eventType, field, value) >= 1 {
			return true
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatalf("WaitAuditEventByPayload(%s, %s=%s): did not appear within %s", eventType, field, value, timeout)
	return false
}

// AuditPayloadField returns the value of payload->>outField from the most
// recent audit entry (event_type + payload->>selField=selValue), or an
// empty string if no entry exists. For asserting specific payload fields
// (prev_kid in reconcile_orphan_applying.executed, new_kid in
// lease_force_released).
func (s *Stack) AuditPayloadField(t *testing.T, eventType, selField, selValue, outField string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var out string
	err := s.db.QueryRow(ctx, `
		SELECT COALESCE(payload->>$4, '') FROM audit_log
		WHERE event_type = $1 AND payload->>$2 = $3
		ORDER BY created_at DESC LIMIT 1`,
		eventType, selField, selValue, outField).Scan(&out)
	if err != nil {
		return ""
	}
	return out
}

// LeaseForceReleasedNewKID returns new_kid from the payload of the
// eventstream.lease_force_released audit event for a given SID (the KID of
// the live keeper that presence-gated the lease takeover from the dead
// holder), or an empty string if no event exists yet. This is the
// authoritative signal of which keeper the SID lease moved to after a
// force-release — the payload is written in the SAME tx as
// ForceAcquireSoulLease (eventstream.auditLeaseForceReleased), so the event
// means the takeover actually happened.
func (s *Stack) LeaseForceReleasedNewKID(t *testing.T, sid string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var newKID string
	err := s.db.QueryRow(ctx, `
		SELECT payload->>'new_kid' FROM audit_log
		WHERE event_type = $1 AND payload->>'sid' = $2
		ORDER BY created_at DESC LIMIT 1`,
		"eventstream.lease_force_released", sid).Scan(&newKID)
	if err != nil {
		return ""
	}
	return newKID
}

// WaitLeaseForceReleased polls audit_log until an
// eventstream.lease_force_released event appears for SID with new_kid in
// wantNewKIDs, or until timeout. Returns the observed new_kid. Fatal on
// timeout. An Eventually wrapper around LeaseForceReleasedNewKID (lease
// moved killed->live keeper).
func (s *Stack) WaitLeaseForceReleased(t *testing.T, sid string, wantNewKIDs []string, timeout time.Duration) string {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var last string
	for time.Now().Before(deadline) {
		last = s.LeaseForceReleasedNewKID(t, sid)
		for _, w := range wantNewKIDs {
			if last == w {
				return last
			}
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatalf("WaitLeaseForceReleased(%s): new_kid %v not reached within %s (last=%q)", sid, wantNewKIDs, timeout, last)
	return ""
}
