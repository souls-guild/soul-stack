//go:build e2e

package harness

// Voyage helpers for multi-keeper crash harness: create scenario-Voyage through
// Operator API (POST /v1/voyages), observe claim owner (voyages.claimed_by_kid),
// and recovery after owner crash (reclaim_voyages -> re-claim by another KID ->
// terminal).
//
// Successful Voyage terminal = status='succeeded' (migration 059). attempt++ on
// every claim/reclaim (fencing epoch ADR-027(g)).

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"
)

// VoyageSnapshot is observable state of a voyages row for assertions.
type VoyageSnapshot struct {
	Status     string
	ClaimedBy  *string
	Attempt    int
	TotalBatch int
	BatchIndex int
	Finished   bool
}

// CreateScenarioVoyage creates scenario-Voyage through POST /v1/voyages over
// listed incarnations. batchSize>0 -> serial waves of batchSize incarnations
// (multiple batches stretch the run, widening crash window). Returns voyage_id
// from 202 body.
//
// 202 -> voyage_id; any other status -> t.Fatal with body (diagnostics without guessing).
// Transient 422 "not registered" (warm service-registry snapshot) is polled as
// in CreateIncarnation.
func (s *Stack) CreateScenarioVoyage(t *testing.T, scenario string, incarnations []string, batchSize int) string {
	t.Helper()
	c := s.opClient(t)
	body := map[string]any{
		"kind":          "scenario",
		"scenario_name": scenario,
		"target": map[string]any{
			"incarnations": incarnations,
		},
	}
	if batchSize > 0 {
		body["batch_size"] = batchSize
	}

	var resp []byte
	var status int
	var err error
	deadline := time.Now().Add(15 * time.Second)
	for {
		resp, status, err = c.post(context.Background(), "/v1/voyages", body)
		if err != nil {
			t.Fatalf("CreateScenarioVoyage %s: http: %v", scenario, err)
		}
		if status == http.StatusUnprocessableEntity &&
			strings.Contains(string(resp), "not registered") &&
			time.Now().Before(deadline) {
			time.Sleep(200 * time.Millisecond)
			continue
		}
		break
	}
	if status != http.StatusAccepted {
		t.Fatalf("CreateScenarioVoyage %s: status %d, body=%s", scenario, status, string(resp))
	}
	var out struct {
		VoyageID string `json:"voyage_id"`
	}
	if err := json.Unmarshal(resp, &out); err != nil {
		t.Fatalf("CreateScenarioVoyage %s: decode: %v (body=%s)", scenario, err, string(resp))
	}
	if out.VoyageID == "" {
		t.Fatalf("CreateScenarioVoyage %s: empty voyage_id (body=%s)", scenario, string(resp))
	}
	return out.VoyageID
}

// VoyageState reads current snapshot of voyages row by ID. Fatal on missing row
// or query error.
func (s *Stack) VoyageState(t *testing.T, voyageID string) VoyageSnapshot {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var (
		snap     VoyageSnapshot
		finished *time.Time
	)
	err := s.db.QueryRow(ctx, `
		SELECT status, claimed_by_kid, attempt, total_batches, current_batch_index, finished_at
		FROM voyages WHERE voyage_id = $1
	`, voyageID).Scan(&snap.Status, &snap.ClaimedBy, &snap.Attempt, &snap.TotalBatch, &snap.BatchIndex, &finished)
	if err != nil {
		t.Fatalf("VoyageState %s: %v", voyageID, err)
	}
	snap.Finished = finished != nil
	return snap
}

// WaitVoyageRunningOwner polls until Voyage becomes running with non-empty
// claimed_by_kid; returns owner KID. This is the "who to kill" capture point.
// Fatal on timeout (with last snapshot).
func (s *Stack) WaitVoyageRunningOwner(t *testing.T, voyageID string, timeout time.Duration) string {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var last VoyageSnapshot
	for time.Now().Before(deadline) {
		last = s.VoyageState(t, voyageID)
		if last.Status == "running" && last.ClaimedBy != nil && *last.ClaimedBy != "" {
			return *last.ClaimedBy
		}
		// If Voyage reached terminal BEFORE we caught running, window is too narrow
		// for kill; test must widen the run.
		if isVoyageTerminal(last.Status) {
			t.Fatalf("WaitVoyageRunningOwner %s: Voyage reached terminal %q before catching running owner (window too narrow - increase scope/batch)",
				voyageID, last.Status)
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("WaitVoyageRunningOwner %s: running owner did not appear within %s (last snapshot=%+v)",
		voyageID, timeout, last)
	return ""
}

// WaitVoyageReclaimed polls until Voyage is re-claimed by ANOTHER KID
// (claimed_by_kid != killedKID) with attempt > attemptBeforeKill. This directly
// proves reclaim_voyages returned stale claim to pending and a live keeper picked
// it up. Returns new owner KID.
//
// Allows intermediate snapshots (pending without owner between reclaim and
// re-claim, or already succeeded with claimed_by_kid of new owner). Key invariant:
// owner changed AND attempt increased.
func (s *Stack) WaitVoyageReclaimed(t *testing.T, voyageID, killedKID string, attemptBeforeKill int, timeout time.Duration) string {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var last VoyageSnapshot
	for time.Now().Before(deadline) {
		last = s.VoyageState(t, voyageID)
		if last.Attempt > attemptBeforeKill && last.ClaimedBy != nil &&
			*last.ClaimedBy != "" && *last.ClaimedBy != killedKID {
			return *last.ClaimedBy
		}
		// Terminal with new non-killed owner and increased attempt is also a valid
		// re-claim (fast run managed to finish).
		if isVoyageTerminal(last.Status) && last.Attempt > attemptBeforeKill &&
			last.ClaimedBy != nil && *last.ClaimedBy != killedKID {
			return *last.ClaimedBy
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("WaitVoyageReclaimed %s: re-claim by another KID did not happen within %s (killed=%s, attempt_before=%d, last snapshot=%+v)",
		voyageID, timeout, killedKID, attemptBeforeKill, last)
	return ""
}

// WaitVoyageSucceeded polls until Voyage reaches status='succeeded' with
// finished_at. Terminal != succeeded (failed/partial_failed/cancelled) is
// immediate t.Fatal (recovery must complete the run to SUCCESS, not any terminal).
func (s *Stack) WaitVoyageSucceeded(t *testing.T, voyageID string, timeout time.Duration) VoyageSnapshot {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var last VoyageSnapshot
	for time.Now().Before(deadline) {
		last = s.VoyageState(t, voyageID)
		switch last.Status {
		case "succeeded":
			if !last.Finished {
				t.Fatalf("WaitVoyageSucceeded %s: status=succeeded, but finished_at is empty (voyages_terminal_finished_at invariant violated)", voyageID)
			}
			return last
		case "failed", "partial_failed", "cancelled":
			t.Fatalf("WaitVoyageSucceeded %s: terminal %q instead of succeeded (recovery did not finish the run; snapshot=%+v)",
				voyageID, last.Status, last)
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("WaitVoyageSucceeded %s: succeeded not reached within %s (last snapshot=%+v)", voyageID, timeout, last)
	return VoyageSnapshot{}
}

// AssertVoyageTargetsTerminal checks that all voyage_targets rows of the run
// reached terminal (status='succeeded'), meaning the run REALLY completed every
// unit (not "formally succeeded on empty scope"). Returns count of succeeded targets.
func (s *Stack) AssertVoyageTargetsTerminal(t *testing.T, voyageID string) int {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	rows, err := s.db.Query(ctx,
		"SELECT target_id, status FROM voyage_targets WHERE voyage_id = $1", voyageID)
	if err != nil {
		t.Fatalf("AssertVoyageTargetsTerminal %s: query: %v", voyageID, err)
	}
	defer rows.Close()
	statuses := map[string]string{}
	for rows.Next() {
		var ref, st string
		if err := rows.Scan(&ref, &st); err != nil {
			t.Fatalf("AssertVoyageTargetsTerminal %s: scan: %v", voyageID, err)
		}
		statuses[ref] = st
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("AssertVoyageTargetsTerminal %s: rows.Err: %v", voyageID, err)
	}
	if len(statuses) == 0 {
		t.Fatalf("AssertVoyageTargetsTerminal %s: no voyage_targets rows", voyageID)
	}
	succeeded := 0
	for ref, st := range statuses {
		if st != "succeeded" {
			t.Fatalf("AssertVoyageTargetsTerminal %s: target=%s status=%q (not succeeded; matrix=%v)",
				voyageID, ref, st, statuses)
		}
		succeeded++
	}
	return succeeded
}

// IncarnationsInStatus returns incarnation names (from incNames) in given status.
// Used to detect seam defect: incarnation orphaned in `applying` after crash of
// keeper owning its per-incarnation scenario-run.
func (s *Stack) IncarnationsInStatus(t *testing.T, incNames []string, status string) []string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var out []string
	for _, name := range incNames {
		var st string
		if err := s.db.QueryRow(ctx, "SELECT status FROM incarnation WHERE name = $1", name).Scan(&st); err != nil {
			t.Fatalf("IncarnationsInStatus(%s): %v", name, err)
		}
		if st == status {
			out = append(out, name)
		}
	}
	return out
}

// IncarnationStatusDetails returns (status, status_details as string) for
// incarnation. status_details is JSONB with error_locked reason (for diagnosing
// seam variant where reclaim leads to error_locked instead of succeeded).
func (s *Stack) IncarnationStatusDetails(t *testing.T, name string) (string, string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var status string
	var details *string
	if err := s.db.QueryRow(ctx,
		"SELECT status, status_details::text FROM incarnation WHERE name = $1", name).Scan(&status, &details); err != nil {
		t.Fatalf("IncarnationStatusDetails(%s): %v", name, err)
	}
	d := ""
	if details != nil {
		d = *details
	}
	return status, d
}

// CountApplyRunsForIncarnation returns number of apply_runs rows for incarnation
// (any status). Confirms orphaned `applying` lock has no live apply_runs (nothing
// to reclaim through reclaim_apply_runs; lock only hangs on incarnation.status).
func (s *Stack) CountApplyRunsForIncarnation(t *testing.T, incarnation string) int {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var n int
	if err := s.db.QueryRow(ctx,
		"SELECT COUNT(*) FROM apply_runs WHERE incarnation_name = $1", incarnation).Scan(&n); err != nil {
		t.Fatalf("CountApplyRunsForIncarnation(%s): %v", incarnation, err)
	}
	return n
}

// DumpRecoveryState logs diagnostic recovery state slice: incarnation statuses +
// apply_runs + voyage_targets. Used when investigating seam bugs
// (crash->reclaim->hang): shows which incarnations stuck in applying and which
// apply_runs orphaned after owner crash.
func (s *Stack) DumpRecoveryState(t *testing.T, voyageID string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	incRows, err := s.db.Query(ctx, `SELECT name, status FROM incarnation ORDER BY name`)
	if err == nil {
		var dump []string
		for incRows.Next() {
			var name, st string
			_ = incRows.Scan(&name, &st)
			dump = append(dump, name+"="+st)
		}
		incRows.Close()
		t.Logf("DUMP incarnation: %v", dump)
	}

	arRows, err := s.db.Query(ctx, `
		SELECT incarnation_name, status, claim_by_kid, COUNT(*)
		FROM apply_runs GROUP BY incarnation_name, status, claim_by_kid ORDER BY incarnation_name`)
	if err == nil {
		var dump []string
		for arRows.Next() {
			var inc, st string
			var kid *string
			var n int
			_ = arRows.Scan(&inc, &st, &kid, &n)
			k := "<nil>"
			if kid != nil {
				k = *kid
			}
			dump = append(dump, fmt.Sprintf("%s/%s@%s×%d", inc, st, k, n))
		}
		arRows.Close()
		t.Logf("DUMP apply_runs: %v", dump)
	}

	tRows, err := s.db.Query(ctx, `
		SELECT target_id, status, batch_index FROM voyage_targets
		WHERE voyage_id = $1 ORDER BY batch_index, target_id`, voyageID)
	if err == nil {
		var dump []string
		for tRows.Next() {
			var id, st string
			var bi int
			_ = tRows.Scan(&id, &st, &bi)
			dump = append(dump, fmt.Sprintf("%s/%s@b%d", id, st, bi))
		}
		tRows.Close()
		t.Logf("DUMP voyage_targets: %v", dump)
	}
}

// isVoyageTerminal is true for terminal voyages statuses (migration 059).
func isVoyageTerminal(status string) bool {
	switch status {
	case "succeeded", "failed", "partial_failed", "cancelled":
		return true
	default:
		return false
	}
}

// CountAuditEvents returns count of audit_log records with given event_type and
// payload->>'voyage_id' = voyageID. Proves `voyage.reclaimed` was actually emitted
// on crash recovery.
func (s *Stack) CountAuditEvents(t *testing.T, eventType, voyageID string) int {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var n int
	err := s.db.QueryRow(ctx, `
		SELECT COUNT(*) FROM audit_log
		WHERE event_type = $1 AND payload->>'voyage_id' = $2
	`, eventType, voyageID).Scan(&n)
	if err != nil {
		t.Fatalf("CountAuditEvents(%s, %s): %v", eventType, voyageID, err)
	}
	return n
}
