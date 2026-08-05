//go:build integration

// Engine provenance on state (ADR-0076(l), NIM-162): the record of WHICH ENGINES
// produced an incarnation's current state, and — the part that actually matters
// half a year later — that the record is still there when you come looking. A
// stamp that a later write silently drops is worse than no stamp: it reads as an
// authoritative answer while describing the wrong run.

package incarnation

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/souls-guild/soul-stack/keeper/internal/statemigrate"
	"github.com/souls-guild/soul-stack/shared/config"
)

// sampleStamp — a stamp of the shape a real run produces: a build inside a
// declared window, with the capability set its plan required.
func sampleStamp() *EngineCompat {
	return &EngineCompat{
		KeeperVersion:    "v0.2.0",
		KeeperWindow:     &config.VersionWindow{Min: "0.1.0", Max: "0.3.0"},
		WindowEnforced:   true,
		SoulCapabilities: []string{"flow_control", "module:core.exec"},
	}
}

// readEngineCompat reads the raw column and decodes it; ok=false means SQL NULL.
func readEngineCompat(t *testing.T, name string) (stamp EngineCompat, ok bool) {
	t.Helper()
	var raw []byte
	if err := integrationPool.QueryRow(context.Background(),
		`SELECT engine_compat FROM incarnation WHERE name = $1`, name).Scan(&raw); err != nil {
		t.Fatalf("read incarnation.engine_compat: %v", err)
	}
	if raw == nil {
		return EngineCompat{}, false
	}
	if err := json.Unmarshal(raw, &stamp); err != nil {
		t.Fatalf("decode incarnation.engine_compat %q: %v", raw, err)
	}
	return stamp, true
}

func readHistoryEngineCompat(t *testing.T, historyID string) (stamp EngineCompat, ok bool) {
	t.Helper()
	var raw []byte
	if err := integrationPool.QueryRow(context.Background(),
		`SELECT engine_compat FROM state_history WHERE history_id = $1`, historyID).Scan(&raw); err != nil {
		t.Fatalf("read state_history.engine_compat: %v", err)
	}
	if raw == nil {
		return EngineCompat{}, false
	}
	if err := json.Unmarshal(raw, &stamp); err != nil {
		t.Fatalf("decode state_history.engine_compat %q: %v", raw, err)
	}
	return stamp, true
}

// TestIntegration_EngineCompat_StampedOnSuccessfulCommit — the base fact: a
// successful commit records the engine contract on BOTH the incarnation (which
// engines the CURRENT state came from) and its state_history row (which engines
// THAT transition came from). Both, because the incarnation answers "now" and
// only the history answers "then".
func TestIntegration_EngineCompat_StampedOnSuccessfulCommit(t *testing.T) {
	resetAll(t)
	ctx := context.Background()
	const (
		name    = "stamp-ok"
		applyID = "01HSTAMP00000000000000001"
		hist    = "01HSTAMPHIST000000000001"
	)
	seedApplyingWithApplyRun(t, name, applyID, name+".host-01")

	want := sampleStamp()
	if err := UpdateStateFromRun(ctx, integrationPool, name, "deploy", applyID,
		map[string]any{"v": 1.0}, map[string]any{"v": 2.0},
		StatusReady, nil, nil, hist, want, nil); err != nil {
		t.Fatalf("UpdateStateFromRun: %v", err)
	}

	got, ok := readEngineCompat(t, name)
	if !ok {
		t.Fatal("incarnation.engine_compat is NULL after a successful stamped commit")
	}
	if got.KeeperVersion != want.KeeperVersion {
		t.Errorf("keeper_version = %q, want %q", got.KeeperVersion, want.KeeperVersion)
	}
	if got.KeeperWindow == nil || got.KeeperWindow.Min != "0.1.0" || got.KeeperWindow.Max != "0.3.0" {
		t.Errorf("keeper_window = %v, want [0.1.0, 0.3.0)", got.KeeperWindow)
	}
	if !got.WindowEnforced {
		t.Error("window_enforced = false, want true (a declared window and a comparable build)")
	}
	if len(got.SoulCapabilities) != 2 || got.SoulCapabilities[0] != "flow_control" {
		t.Errorf("soul_capabilities = %v, want the required set", got.SoulCapabilities)
	}

	histStamp, ok := readHistoryEngineCompat(t, hist)
	if !ok {
		t.Fatal("state_history.engine_compat is NULL - the transition lost its provenance")
	}
	if histStamp.KeeperVersion != want.KeeperVersion {
		t.Errorf("history keeper_version = %q, want %q", histStamp.KeeperVersion, want.KeeperVersion)
	}
}

// TestIntegration_EngineCompat_FailedRunKeepsPreviousStamp — ★ a failed run does
// NOT relabel the state. It changed nothing, so the engines that produced what is
// there are still the previous run's; overwriting (or clearing) the stamp would
// attribute the state to a run that never committed it. The failure's own
// state_history row honestly carries NULL — nothing was rendered into state.
func TestIntegration_EngineCompat_FailedRunKeepsPreviousStamp(t *testing.T) {
	resetAll(t)
	ctx := context.Background()
	const (
		name     = "stamp-keep"
		applyOK  = "01HSTAMP00000000000000002"
		applyBad = "01HSTAMP00000000000000003"
		histOK   = "01HSTAMPHIST000000000002"
		histBad  = "01HSTAMPHIST000000000003"
	)
	seedApplyingWithApplyRun(t, name, applyOK, name+".host-01")

	if err := UpdateStateFromRun(ctx, integrationPool, name, "deploy", applyOK,
		map[string]any{}, map[string]any{"v": 1.0},
		StatusReady, nil, nil, histOK, sampleStamp(), nil); err != nil {
		t.Fatalf("UpdateStateFromRun (success): %v", err)
	}

	// Back to applying for a second run that fails.
	if _, err := integrationPool.Exec(ctx,
		`UPDATE incarnation SET status = 'applying' WHERE name = $1`, name); err != nil {
		t.Fatalf("re-arm applying: %v", err)
	}
	if err := UpdateStateFromRun(ctx, integrationPool, name, "deploy", applyBad,
		map[string]any{"v": 1.0}, map[string]any{"v": 1.0},
		StatusErrorLocked, map[string]any{"reason": "boom"}, nil, histBad, nil, nil); err != nil {
		t.Fatalf("UpdateStateFromRun (failure): %v", err)
	}

	got, ok := readEngineCompat(t, name)
	if !ok {
		t.Fatal("* a failed run CLEARED the incarnation stamp - the surviving state now claims no provenance")
	}
	if got.KeeperVersion != "v0.2.0" {
		t.Errorf("* keeper_version = %q, want the last SUCCESSFUL run's v0.2.0", got.KeeperVersion)
	}

	if _, ok := readHistoryEngineCompat(t, histBad); ok {
		t.Error("the failed transition's history row carries a stamp - it rendered nothing into state")
	}
}

// TestIntegration_EngineCompat_SurvivesStateSchemaUpgrade — ★ the stamp outlives
// a state migration (ADR-019). A schema upgrade rewrites `state` and bumps
// `state_schema_version`, and that write must leave the provenance alone: the
// engines that produced the state are a fact about the run, not about the shape
// the state has since been migrated into.
func TestIntegration_EngineCompat_SurvivesStateSchemaUpgrade(t *testing.T) {
	resetAll(t)
	ctx := context.Background()
	const (
		name    = "stamp-migrate"
		applyID = "01HSTAMP00000000000000004"
		hist    = "01HSTAMPHIST000000000004"
	)
	seedApplyingWithApplyRun(t, name, applyID, name+".host-01")

	if err := UpdateStateFromRun(ctx, integrationPool, name, "deploy", applyID,
		map[string]any{}, map[string]any{"v": 1.0},
		StatusReady, nil, nil, hist, sampleStamp(), nil); err != nil {
		t.Fatalf("UpdateStateFromRun: %v", err)
	}

	if _, err := UpgradeStateSchema(ctx, integrationPool, UpgradeInput{
		Name:             name,
		TargetServiceVer: "v2.0.0",
		TargetSchemaVer:  2,
		Chain:            statemigrate.Chain{setStep(1, 2)},
		Evaluator:        newEvaluator(t),
		ApplyID:          "01HSTAMPUPGRADE0000000001",
	}); err != nil {
		t.Fatalf("UpgradeStateSchema: %v", err)
	}

	var schemaVer int
	if err := integrationPool.QueryRow(ctx,
		`SELECT state_schema_version FROM incarnation WHERE name = $1`, name).Scan(&schemaVer); err != nil {
		t.Fatalf("read state_schema_version: %v", err)
	}
	if schemaVer != 2 {
		t.Fatalf("state_schema_version = %d, want 2 (the migration must actually have run)", schemaVer)
	}

	got, ok := readEngineCompat(t, name)
	if !ok {
		t.Fatal("* the state migration DROPPED the provenance stamp")
	}
	if got.KeeperVersion != "v0.2.0" || !got.WindowEnforced {
		t.Errorf("* stamp altered by the migration: %+v", got)
	}
}

// TestIntegration_EngineCompat_UnstampedRowsReadFine — the nullable discipline
// for everything written before migration 103: an incarnation and a history row
// with engine_compat NULL are ordinary rows on every normal read path. Nothing
// backfills, so this is the state of the entire estate on the day the migration
// lands.
func TestIntegration_EngineCompat_UnstampedRowsReadFine(t *testing.T) {
	resetAll(t)
	ctx := context.Background()
	const (
		name    = "stamp-legacy"
		applyID = "01HSTAMP00000000000000005"
		hist    = "01HSTAMPHIST000000000005"
	)
	seedApplyingWithApplyRun(t, name, applyID, name+".host-01")

	// nil stamp — exactly what every pre-103 row holds.
	if err := UpdateStateFromRun(ctx, integrationPool, name, "deploy", applyID,
		map[string]any{}, map[string]any{"v": 1.0},
		StatusReady, nil, nil, hist, nil, nil); err != nil {
		t.Fatalf("UpdateStateFromRun: %v", err)
	}
	if _, ok := readEngineCompat(t, name); ok {
		t.Fatal("a nil stamp wrote a non-NULL engine_compat")
	}

	inc, err := SelectByName(ctx, integrationPool, name)
	if err != nil {
		t.Fatalf("SelectByName on an unstamped incarnation: %v", err)
	}
	if inc.Status != StatusReady {
		t.Errorf("status = %q, want ready", inc.Status)
	}

	entries, total, err := HistorySelectByName(ctx, integrationPool, name, HistoryFilter{}, 0, 10)
	if err != nil {
		t.Fatalf("HistorySelectByName on unstamped history: %v", err)
	}
	if total != 1 || len(entries) != 1 {
		t.Errorf("history entries = %d (total %d), want 1", len(entries), total)
	}
}
