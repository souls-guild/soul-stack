//go:build integration

// Integration-guard cascade deletion of persistent notify-rules from Cadence (ADR-052 §m /
// ADR-046 §9): DELETE cadence removes Tiding records with created_from_cadence_id == id
// (FK ON DELETE CASCADE, migration 074), but does NOT touch rules with the same
// cadence-SELECTOR and created_from_cadence_id == NULL (manually created).
// Requires real FK CASCADE → only under docker (testcontainers).

package herald

import (
	"context"
	"errors"
	"testing"
)

// insertCadence inserts a minimal cadences row directly (herald package does not
// import cadence — for FK purposes raw INSERT of migration 066 mandatory columns suffices:
// id/name/schedule_kind/overlap_policy/kind/target/created_by_aid).
func insertCadence(t *testing.T, id, aid string) {
	t.Helper()
	_, err := integrationPool.Exec(context.Background(),
		`INSERT INTO cadences (id, name, schedule_kind, interval_seconds, overlap_policy, kind, scenario_name, target, created_by_aid)
		 VALUES ($1, $2, 'interval', 300, 'skip', 'scenario', 'converge', '{"service":"web"}'::jsonb, $3)`,
		id, "nightly-"+id, aid)
	if err != nil {
		t.Fatalf("insertCadence(%s): %v", id, err)
	}
}

// cleanCadences cleans up cadences (resetAll does not touch them directly; dependent
// tidings will be removed by cascade on this DELETE — which is the subject of the neighbor test, but
// here — final cleanup of the fixture).
func cleanCadences(t *testing.T) {
	t.Helper()
	if _, err := integrationPool.Exec(context.Background(), `DELETE FROM cadences`); err != nil {
		t.Fatalf("cleanCadences: %v", err)
	}
}

func TestIntegration_Cadence_DeleteCascadesFormRules_NotManual(t *testing.T) {
	resetAll(t)
	cleanCadences(t)
	seedOperator(t, "archon-alice")
	ctx := context.Background()

	// Delivery channel (FK tidings.herald).
	if err := InsertHerald(ctx, integrationPool, newWebhookHerald("ops-webhook", "archon-alice")); err != nil {
		t.Fatalf("InsertHerald: %v", err)
	}

	// Cascade target schedule.
	const cadID = "01J0CADENCE000000000000001"
	insertCadence(t, cadID, "archon-alice")
	defer cleanCadences(t)

	// (1) Form-rule: created from notify[] of Cadence form — cadence selector +
	// origin-marker created_from_cadence_id == cadID. Must be removed by cascade.
	formRule := &Tiding{
		Name:                 "nightly-notify",
		Herald:               "ops-webhook",
		EventTypes:           []string{"scenario_run.failed"},
		Cadence:              strptr(cadID),
		CreatedFromCadenceID: strptr(cadID),
		Enabled:              true,
		CreatedByAID:         strptr("archon-alice"),
	}
	if err := InsertTiding(ctx, integrationPool, formRule); err != nil {
		t.Fatalf("InsertTiding form-rule: %v", err)
	}

	// (2) Manually created rule with the SAME cadence-SELECTOR, but WITHOUT
	// origin-marker (created_from_cadence_id == NULL). Must NOT be deleted on
	// DELETE cadence — operator created it manually, it survives schedule deletion.
	manualRule := &Tiding{
		Name:         "manual-watch",
		Herald:       "ops-webhook",
		EventTypes:   []string{"scenario_run.completed"},
		Cadence:      strptr(cadID), // Same selector.
		Enabled:      true,
		CreatedByAID: strptr("archon-alice"),
	}
	if err := InsertTiding(ctx, integrationPool, manualRule); err != nil {
		t.Fatalf("InsertTiding manual-rule: %v", err)
	}

	// DELETE cadence → FK ON DELETE CASCADE removes form-rule.
	if _, err := integrationPool.Exec(ctx, `DELETE FROM cadences WHERE id = $1`, cadID); err != nil {
		t.Fatalf("DELETE cadence: %v", err)
	}

	// Form-rule removed by cascade.
	if _, err := SelectTidingByName(ctx, integrationPool, "nightly-notify"); !errors.Is(err, ErrTidingNotFound) {
		t.Errorf("form-rule after DELETE cadence: err = %v, want ErrTidingNotFound (cascade)", err)
	}
	// Manually created rule (created_from_cadence_id == NULL) SURVIVED, despite
	// having the same cadence-selector.
	survived, err := SelectTidingByName(ctx, integrationPool, "manual-watch")
	if err != nil {
		t.Fatalf("manual-rule wrongly deleted after DELETE cadence: %v", err)
	}
	if survived.CreatedFromCadenceID != nil {
		t.Errorf("manual-rule.CreatedFromCadenceID = %v, want nil (manually created)", survived.CreatedFromCadenceID)
	}
}

// Round-trip created_from_cadence_id via Insert/Select (ADR-052 §m): marker
// is correctly written to and read from column 074.
func TestIntegration_Tiding_CreatedFromCadenceRoundTrip(t *testing.T) {
	resetAll(t)
	cleanCadences(t)
	seedOperator(t, "archon-alice")
	ctx := context.Background()

	if err := InsertHerald(ctx, integrationPool, newWebhookHerald("ops-webhook", "archon-alice")); err != nil {
		t.Fatalf("InsertHerald: %v", err)
	}
	const cadID = "01J0CADENCE000000000000002"
	insertCadence(t, cadID, "archon-alice")
	defer cleanCadences(t)

	rule := &Tiding{
		Name:                 "nightly-notify",
		Herald:               "ops-webhook",
		EventTypes:           []string{"scenario_run.failed"},
		Cadence:              strptr(cadID),
		CreatedFromCadenceID: strptr(cadID),
		Enabled:              true,
		CreatedByAID:         strptr("archon-alice"),
	}
	if err := InsertTiding(ctx, integrationPool, rule); err != nil {
		t.Fatalf("InsertTiding: %v", err)
	}
	got, err := SelectTidingByName(ctx, integrationPool, "nightly-notify")
	if err != nil {
		t.Fatalf("SelectTidingByName: %v", err)
	}
	if got.CreatedFromCadenceID == nil || *got.CreatedFromCadenceID != cadID {
		t.Errorf("CreatedFromCadenceID round-trip = %v, want %q", got.CreatedFromCadenceID, cadID)
	}
}
