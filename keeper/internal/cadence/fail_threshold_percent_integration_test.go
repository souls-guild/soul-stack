//go:build integration

package cadence_test

// Integration round-trip for the cadences.fail_threshold_percent column
// (migration 070, ADR-043 amendment 2026-06-09, Cadence-recipe S3). Uses the
// same testcontainers-PG as min_period_integration_test.go (shared
// integrationPool/TestMain). Verifies:
//   - migration 070 applied (column exists, Insert percent succeeds);
//   - round-trip Insert->Get preserves fail_threshold_percent;
//   - CHECK cadences_fail_threshold_percent_range rejects percent outside
//     [1, 100] at DB level (defence-in-depth over handler validation).

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/souls-guild/soul-stack/keeper/internal/cadence"
)

func newPercentCadence(percent *int) *cadence.Cadence {
	return &cadence.Cadence{
		ID:                   nextID(),
		Name:                 "ftp-roundtrip",
		Enabled:              true,
		ScheduleKind:         cadence.ScheduleKindInterval,
		IntervalSeconds:      intptr(300),
		OverlapPolicy:        cadence.OverlapPolicySkip,
		Kind:                 cadence.KindScenario,
		ScenarioName:         strptr("converge"),
		Target:               json.RawMessage(`{"coven":"prod"}`),
		FailThresholdPercent: percent,
		CreatedByAID:         testAID,
	}
}

// TestFailThresholdPercent_RoundTrip checks Insert with percent -> Get returns
// the same value (column 070 is really stored and read through scanCadence).
func TestFailThresholdPercent_RoundTrip(t *testing.T) {
	clearCadences(t)
	ctx := context.Background()

	c := newPercentCadence(intptr(25))
	if err := cadence.Insert(ctx, integrationPool, c); err != nil {
		t.Fatalf("Insert percent cadence: %v", err)
	}

	got, err := cadence.Get(ctx, integrationPool, c.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.FailThresholdPercent == nil || *got.FailThresholdPercent != 25 {
		t.Errorf("fail_threshold_percent round-trip = %v, want 25", got.FailThresholdPercent)
	}
	if got.FailThreshold != nil {
		t.Errorf("fail_threshold = %v, want nil (percent-only recipe)", got.FailThreshold)
	}
}

// TestFailThresholdPercent_AbsoluteRoundTrip checks backcompat: a recipe with
// absolute fail_threshold (without percent) leaves the percent column NULL.
func TestFailThresholdPercent_AbsoluteRoundTrip(t *testing.T) {
	clearCadences(t)
	ctx := context.Background()

	c := newPercentCadence(nil)
	c.FailThreshold = intptr(4)
	if err := cadence.Insert(ctx, integrationPool, c); err != nil {
		t.Fatalf("Insert absolute cadence: %v", err)
	}

	got, err := cadence.Get(ctx, integrationPool, c.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.FailThreshold == nil || *got.FailThreshold != 4 {
		t.Errorf("fail_threshold = %v, want 4", got.FailThreshold)
	}
	if got.FailThresholdPercent != nil {
		t.Errorf("fail_threshold_percent = %v, want nil (absolute recipe)", got.FailThresholdPercent)
	}
}

// TestFailThresholdPercent_CheckRejectsOutOfRange — DB-CHECK
// cadences_fail_threshold_percent_range rejects percent=101 on INSERT. Insert
// around cadence.validate (raw SQL) to reach the PG CHECK specifically.
func TestFailThresholdPercent_CheckRejectsOutOfRange(t *testing.T) {
	clearCadences(t)
	ctx := context.Background()

	_, err := integrationPool.Exec(ctx, `
		INSERT INTO cadences (
		    id, name, enabled, schedule_kind, interval_seconds, overlap_policy,
		    kind, scenario_name, target, fail_threshold_percent, created_by_aid
		) VALUES ($1, 'bad', true, 'interval', 300, 'skip',
		    'scenario', 'converge', '{"coven":"prod"}'::jsonb, 101, $2)`,
		nextID(), testAID)
	if err == nil {
		t.Fatal("INSERT with fail_threshold_percent=101 succeeded, expected CHECK violation")
	}
	if !strings.Contains(err.Error(), "cadences_fail_threshold_percent_range") {
		t.Errorf("expected cadences_fail_threshold_percent_range violation, got: %v", err)
	}
}
