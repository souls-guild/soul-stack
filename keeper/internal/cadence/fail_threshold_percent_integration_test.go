//go:build integration

package cadence_test

// Integration round-trip колонки cadences.fail_threshold_percent (миграция 070,
// ADR-043 amendment 2026-06-09, Cadence-recipe S3). Под тем же testcontainers-PG,
// что min_period_integration_test.go (общий integrationPool/TestMain). Проверяет:
//   - миграция 070 применилась (колонка существует, Insert percent проходит);
//   - round-trip Insert→Get сохраняет fail_threshold_percent;
//   - CHECK cadences_fail_threshold_percent_range отвергает percent вне [1, 100]
//     на DB-уровне (defence-in-depth поверх handler-валидации).

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

// TestFailThresholdPercent_RoundTrip — Insert с percent → Get возвращает то же
// значение (колонка 070 реально хранит и читается через scanCadence).
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
		t.Errorf("fail_threshold = %v, want nil (percent-only рецепт)", got.FailThreshold)
	}
}

// TestFailThresholdPercent_AbsoluteRoundTrip — backcompat: рецепт с абсолютным
// fail_threshold (без percent) → percent-колонка NULL.
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
		t.Errorf("fail_threshold_percent = %v, want nil (абсолютный рецепт)", got.FailThresholdPercent)
	}
}

// TestFailThresholdPercent_CheckRejectsOutOfRange — DB-CHECK
// cadences_fail_threshold_percent_range отвергает percent=101 на INSERT. Вставляем
// в обход cadence.validate (raw SQL), чтобы дойти именно до CHECK-а PG.
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
		t.Fatal("INSERT с fail_threshold_percent=101 прошёл, ожидался CHECK-violation")
	}
	if !strings.Contains(err.Error(), "cadences_fail_threshold_percent_range") {
		t.Errorf("ожидался cadences_fail_threshold_percent_range violation, got: %v", err)
	}
}
