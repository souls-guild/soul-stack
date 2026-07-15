//go:build integration

package incarnation

import (
	"context"
	"testing"
	"time"
)

// TestIntegration_SelectScryCandidates_HappyPath — the iterator predicate of the
// `scry_background` rule: returns only ready/drift incarnations WITHOUT an active
// run, sorted by `last_drift_check_at NULLS FIRST`.
func TestIntegration_SelectScryCandidates_HappyPath(t *testing.T) {
	resetAll(t)
	seedOperator(t, "archon-alice")
	ctx := context.Background()
	creator := "archon-alice"

	// 3 incarnations: a — ready, never scanned; b — drift, with a stale
	// last_drift_check_at; c — applying (should be excluded).
	for _, n := range []string{"alpha-ready", "beta-drift", "gamma-applying"} {
		status := StatusReady
		switch n {
		case "beta-drift":
			status = StatusDrift
		case "gamma-applying":
			status = StatusApplying
		}
		inc := &Incarnation{
			Name: n, Service: "redis", ServiceVersion: "v1",
			StateSchemaVersion: 1, Status: status, CreatedByAID: &creator,
		}
		if err := Create(ctx, integrationPool, inc); err != nil {
			t.Fatalf("Create %s: %v", n, err)
		}
	}

	// beta-drift: set last_drift_check_at in the past.
	old := time.Now().Add(-24 * time.Hour).UTC()
	if _, err := integrationPool.Exec(ctx,
		`UPDATE incarnation SET last_drift_check_at = $1 WHERE name = 'beta-drift'`, old); err != nil {
		t.Fatalf("seed last_drift_check_at: %v", err)
	}

	got, err := SelectScryCandidates(ctx, integrationPool, 0, 10)
	if err != nil {
		t.Fatalf("SelectScryCandidates: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("len(got) = %d, want 2 (ready+drift, applying excluded); got=%+v", len(got), got)
	}
	// ORDER BY last_drift_check_at NULLS FIRST → alpha-ready (NULL) comes first.
	if got[0].Name != "alpha-ready" {
		t.Errorf("got[0].Name = %q, want alpha-ready (NULLS FIRST)", got[0].Name)
	}
	if got[1].Name != "beta-drift" {
		t.Errorf("got[1].Name = %q, want beta-drift", got[1].Name)
	}
}

// TestIntegration_SelectScryCandidates_ExcludesActiveApplyRuns — an incarnation with
// an unfinished apply_run (finished_at IS NULL) is excluded, regardless of
// status.
func TestIntegration_SelectScryCandidates_ExcludesActiveApplyRuns(t *testing.T) {
	resetAll(t)
	seedOperator(t, "archon-alice")
	ctx := context.Background()
	creator := "archon-alice"

	inc := &Incarnation{
		Name: "alpha", Service: "redis", ServiceVersion: "v1",
		StateSchemaVersion: 1, Status: StatusReady, CreatedByAID: &creator,
	}
	if err := Create(ctx, integrationPool, inc); err != nil {
		t.Fatalf("Create: %v", err)
	}

	// An active apply_run (without finished_at).
	if _, err := integrationPool.Exec(ctx, `
INSERT INTO apply_runs (apply_id, sid, incarnation_name, scenario, status, started_by_aid)
VALUES ('01HACTIVE000000000000000A', 'host-1', 'alpha', 'create', 'running', $1)`, creator); err != nil {
		t.Fatalf("seed apply_runs: %v", err)
	}

	got, err := SelectScryCandidates(ctx, integrationPool, 0, 10)
	if err != nil {
		t.Fatalf("SelectScryCandidates: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("len(got) = %d, want 0 (active apply_run blocks scan); got=%+v", len(got), got)
	}

	// Finish the run — the incarnation becomes a candidate.
	if _, err := integrationPool.Exec(ctx,
		`UPDATE apply_runs SET status='success', finished_at=NOW() WHERE apply_id='01HACTIVE000000000000000A'`); err != nil {
		t.Fatalf("finish apply_runs: %v", err)
	}
	got, err = SelectScryCandidates(ctx, integrationPool, 0, 10)
	if err != nil {
		t.Fatalf("SelectScryCandidates: %v", err)
	}
	if len(got) != 1 || got[0].Name != "alpha" {
		t.Fatalf("after finish: got=%+v, want [alpha]", got)
	}
}

// TestIntegration_SelectScryCandidates_MinIntervalThrottle — given a
// min_interval, an incarnation with a fresh last_drift_check_at is excluded.
func TestIntegration_SelectScryCandidates_MinIntervalThrottle(t *testing.T) {
	resetAll(t)
	seedOperator(t, "archon-alice")
	ctx := context.Background()
	creator := "archon-alice"

	for _, n := range []string{"fresh", "stale"} {
		inc := &Incarnation{
			Name: n, Service: "redis", ServiceVersion: "v1",
			StateSchemaVersion: 1, Status: StatusReady, CreatedByAID: &creator,
		}
		if err := Create(ctx, integrationPool, inc); err != nil {
			t.Fatalf("Create %s: %v", n, err)
		}
	}
	now := time.Now().UTC()
	// fresh — 1 minute ago; stale — an hour ago. min_interval = 30m: fresh
	// should be excluded, stale should pass.
	if _, err := integrationPool.Exec(ctx,
		`UPDATE incarnation SET last_drift_check_at = $1 WHERE name = 'fresh'`, now.Add(-time.Minute)); err != nil {
		t.Fatalf("seed fresh: %v", err)
	}
	if _, err := integrationPool.Exec(ctx,
		`UPDATE incarnation SET last_drift_check_at = $1 WHERE name = 'stale'`, now.Add(-time.Hour)); err != nil {
		t.Fatalf("seed stale: %v", err)
	}

	got, err := SelectScryCandidates(ctx, integrationPool, 30*time.Minute, 10)
	if err != nil {
		t.Fatalf("SelectScryCandidates: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("len(got) = %d, want 1 (fresh throttled, stale ok); got=%+v", len(got), got)
	}
	if got[0].Name != "stale" {
		t.Errorf("got[0].Name = %q, want stale", got[0].Name)
	}
}

// TestIntegration_UpdateDriftScanResult_HappyPath — writes the columns and reads
// them back via SelectByName.
func TestIntegration_UpdateDriftScanResult_HappyPath(t *testing.T) {
	resetAll(t)
	seedOperator(t, "archon-alice")
	ctx := context.Background()
	creator := "archon-alice"

	inc := &Incarnation{
		Name: "redis-prod", Service: "redis", ServiceVersion: "v1",
		StateSchemaVersion: 1, Status: StatusReady, CreatedByAID: &creator,
	}
	if err := Create(ctx, integrationPool, inc); err != nil {
		t.Fatalf("Create: %v", err)
	}

	// 123456 µs (a non-zero fraction of a second): PG timestamptz stores microseconds,
	// the round-trip must preserve sub-second precision without truncating to seconds.
	scannedAt := time.Date(2026, 5, 26, 12, 0, 0, 123456000, time.UTC)
	summary := DriftScanSummary{
		HostsDrifted: 1, HostsClean: 2, HostsUnsupported: 0, HostsFailed: 0,
		TotalHosts: 3, ScannedAt: scannedAt,
	}
	if err := UpdateDriftScanResult(ctx, integrationPool, "redis-prod", summary); err != nil {
		t.Fatalf("UpdateDriftScanResult: %v", err)
	}

	got, err := SelectByName(ctx, integrationPool, "redis-prod")
	if err != nil {
		t.Fatalf("SelectByName: %v", err)
	}
	if got.LastDriftCheckAt == nil {
		t.Fatalf("LastDriftCheckAt is nil after UpdateDriftScanResult")
	}
	if !got.LastDriftCheckAt.Equal(scannedAt) {
		t.Errorf("LastDriftCheckAt = %v, want %v", got.LastDriftCheckAt, scannedAt)
	}
	if got.LastDriftSummary == nil {
		t.Fatalf("LastDriftSummary is nil")
	}
	// Typed round-trip: writing scry → reading the column into DriftScanSummary — no
	// loss on counts or sub-second (µs) scanned_at.
	if got.LastDriftSummary.HostsDrifted != 1 {
		t.Errorf("hosts_drifted = %d, want 1", got.LastDriftSummary.HostsDrifted)
	}
	if got.LastDriftSummary.HostsClean != 2 {
		t.Errorf("hosts_clean = %d, want 2", got.LastDriftSummary.HostsClean)
	}
	if got.LastDriftSummary.TotalHosts != 3 {
		t.Errorf("total_hosts = %d, want 3", got.LastDriftSummary.TotalHosts)
	}
	if !got.LastDriftSummary.ScannedAt.Equal(scannedAt) {
		t.Errorf("scanned_at = %v, want %v", got.LastDriftSummary.ScannedAt, scannedAt)
	}
}

// TestIntegration_CountActiveDryRuns — a counter for the throttle cap:
// counts only recipe.dry_run=true + finished_at IS NULL.
func TestIntegration_CountActiveDryRuns(t *testing.T) {
	resetAll(t)
	seedOperator(t, "archon-alice")
	ctx := context.Background()
	creator := "archon-alice"

	inc := &Incarnation{
		Name: "alpha", Service: "redis", ServiceVersion: "v1",
		StateSchemaVersion: 1, Status: StatusReady, CreatedByAID: &creator,
	}
	if err := Create(ctx, integrationPool, inc); err != nil {
		t.Fatalf("Create: %v", err)
	}

	// (a) a regular live apply_run (without recipe-dry_run) — not counted.
	if _, err := integrationPool.Exec(ctx, `
INSERT INTO apply_runs (apply_id, sid, incarnation_name, scenario, status, started_by_aid, recipe)
VALUES ('01HRUN1000000000000000000A', 'host-1', 'alpha', 'create', 'running', $1, NULL)`, creator); err != nil {
		t.Fatalf("seed non-dry: %v", err)
	}
	// (b) dry_run live — counted.
	if _, err := integrationPool.Exec(ctx, `
INSERT INTO apply_runs (apply_id, sid, incarnation_name, scenario, status, started_by_aid, recipe)
VALUES ('01HDRY10000000000000000001', 'host-1', 'alpha', 'converge', 'planned', $1,
        '{"service_ref":{},"scenario_name":"converge","dry_run":true}'::jsonb)`, creator); err != nil {
		t.Fatalf("seed dry-live: %v", err)
	}
	// (c) dry_run finished — NOT counted.
	if _, err := integrationPool.Exec(ctx, `
INSERT INTO apply_runs (apply_id, sid, incarnation_name, scenario, status, started_by_aid, recipe, finished_at)
VALUES ('01HDRY20000000000000000002', 'host-2', 'alpha', 'converge', 'success', $1,
        '{"service_ref":{},"scenario_name":"converge","dry_run":true}'::jsonb, NOW())`, creator); err != nil {
		t.Fatalf("seed dry-fin: %v", err)
	}

	n, err := CountActiveDryRuns(ctx, integrationPool)
	if err != nil {
		t.Fatalf("CountActiveDryRuns: %v", err)
	}
	if n != 1 {
		t.Errorf("CountActiveDryRuns = %d, want 1 (only live-dry)", n)
	}
}
