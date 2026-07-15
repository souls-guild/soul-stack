// GOLDEN byte-exact wire-guard for the NATIVE wire-DTO of the INCARNATION domain (handler-native T5d).
// incarnation no longer depends on the legacy generator — golden checks json native values against a
// pinned reference string. Covers all ADR-051 categories:
//
//   - category A (date-time): same RFC3339Nano bytes;
//   - category B ([]-vs-null): covens without omitempty;
//   - category C (omitempty): apply_id/last_drift_*/changed_by_aid — key omitted when nil;
//   - category D (nullable): spec/state/status_details/created_by_aid — `null` when nil.
//
// Both pointer states covered (nil and non-nil). Mutating the native-struct shape reddens the case.
package api

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/souls-guild/soul-stack/keeper/internal/api/handlers"
)

func goldenIncarnationWire(t *testing.T, name string, native any, want string) {
	t.Helper()
	got, err := json.Marshal(native)
	if err != nil {
		t.Fatalf("%s: marshal native: %v", name, err)
	}
	if string(got) != want {
		t.Errorf("%s: WIRE DRIFT\n got  = %s\n want = %s", name, got, want)
	}
}

func TestGoldenWire_IncarnationReply(t *testing.T) {
	ts := time.Date(2026, 6, 14, 12, 34, 56, 789012345, time.UTC)
	ts2 := time.Date(2026, 6, 13, 1, 2, 3, 456789012, time.UTC)
	apply := "01J0APPLYULID"
	aid := "archon-alice"
	specMap := map[string]interface{}{"hosts": []any{"web1"}}
	stateMap := map[string]interface{}{"users": map[string]any{"app": true}}

	// --- IncarnationCreateReply: apply_id omitempty (both branches) ---
	goldenIncarnationWire(t, "CreateReply/apply_set",
		IncarnationCreateReply{ApplyID: &apply, Incarnation: "redis-prod"},
		`{"apply_id":"01J0APPLYULID","incarnation":"redis-prod"}`)
	goldenIncarnationWire(t, "CreateReply/apply_nil",
		IncarnationCreateReply{ApplyID: nil, Incarnation: "redis-prod"},
		`{"incarnation":"redis-prod"}`)

	// --- IncarnationRunReply ---
	goldenIncarnationWire(t, "RunReply",
		IncarnationRunReply{ApplyID: apply, Incarnation: "redis-prod", Scenario: "converge"},
		`{"apply_id":"01J0APPLYULID","incarnation":"redis-prod","scenario":"converge"}`)

	// --- IncarnationUnlockReply: date-time + enum fields ---
	goldenIncarnationWire(t, "UnlockReply",
		IncarnationUnlockReply{Name: "redis-prod", PreviousStatus: IncarnationStatusErrorLocked, Status: IncarnationStatusReady, UnlockedAt: ts, UnlockedByAID: aid},
		`{"name":"redis-prod","previous_status":"error_locked","status":"ready","unlocked_at":"2026-06-14T12:34:56.789012345Z","unlocked_by_aid":"archon-alice"}`)

	// --- IncarnationUpgradeReply ---
	goldenIncarnationWire(t, "UpgradeReply",
		IncarnationUpgradeReply{ApplyID: apply},
		`{"apply_id":"01J0APPLYULID"}`)

	// --- IncarnationRerunLastReply ---
	goldenIncarnationWire(t, "RerunLastReply",
		IncarnationRerunLastReply{ApplyID: apply, Incarnation: "redis-prod", Scenario: "add_user"},
		`{"apply_id":"01J0APPLYULID","incarnation":"redis-prod","scenario":"add_user"}`)

	// --- IncarnationDestroyReply ---
	goldenIncarnationWire(t, "DestroyReply",
		IncarnationDestroyReply{ApplyID: apply},
		`{"apply_id":"01J0APPLYULID"}`)

	// --- DriftScanSummary (nested, date-time) ---
	goldenIncarnationWire(t, "DriftScanSummary",
		DriftScanSummary{HostsClean: 3, HostsDrifted: 1, HostsFailed: 0, HostsUnsupported: 2, ScannedAt: ts, TotalHosts: 6},
		`{"hosts_clean":3,"hosts_drifted":1,"hosts_failed":0,"hosts_unsupported":2,"scanned_at":"2026-06-14T12:34:56.789012345Z","total_hosts":6}`)

	// --- StateHistoryEntry (nested): changed_by_aid omitempty + state_* nullable ---
	goldenIncarnationWire(t, "StateHistoryEntry/full",
		StateHistoryEntry{ApplyID: apply, ChangedByAID: &aid, CreatedAt: ts, HistoryID: "h1", Scenario: "create", StateAfter: &stateMap, StateBefore: &specMap},
		`{"apply_id":"01J0APPLYULID","changed_by_aid":"archon-alice","created_at":"2026-06-14T12:34:56.789012345Z","history_id":"h1","scenario":"create","state_after":{"users":{"app":true}},"state_before":{"hosts":["web1"]}}`)
	goldenIncarnationWire(t, "StateHistoryEntry/nil_optionals",
		StateHistoryEntry{ApplyID: apply, ChangedByAID: nil, CreatedAt: ts, HistoryID: "h1", Scenario: "migration", StateAfter: nil, StateBefore: nil},
		`{"apply_id":"01J0APPLYULID","created_at":"2026-06-14T12:34:56.789012345Z","history_id":"h1","scenario":"migration","state_after":null,"state_before":null}`)

	// --- IncarnationGetReply: все категории (date-time + []-vs-null + omitempty + nullable) ---
	driftN := DriftScanSummary{HostsClean: 5, ScannedAt: ts2, TotalHosts: 5}
	goldenIncarnationWire(t, "GetReply/full",
		IncarnationGetReply{
			Covens: []string{"prod", "eu"}, CreatedAt: ts, CreatedByAID: &aid,
			LastDriftCheckAt: &ts2, LastDriftSummary: &driftN, Name: "redis-prod", Service: "redis",
			ServiceVersion: "v2.0.0", Spec: &specMap, State: &stateMap, StateSchemaVersion: 3,
			Status: IncarnationStatusDrift, StatusDetails: &stateMap, UpdatedAt: ts2,
		},
		`{"covens":["prod","eu"],"created_at":"2026-06-14T12:34:56.789012345Z","created_by_aid":"archon-alice","last_drift_check_at":"2026-06-13T01:02:03.456789012Z","last_drift_summary":{"hosts_clean":5,"hosts_drifted":0,"hosts_failed":0,"hosts_unsupported":0,"scanned_at":"2026-06-13T01:02:03.456789012Z","total_hosts":5},"name":"redis-prod","service":"redis","service_version":"v2.0.0","spec":{"hosts":["web1"]},"state":{"users":{"app":true}},"state_schema_version":3,"status":"drift","status_details":{"users":{"app":true}},"updated_at":"2026-06-13T01:02:03.456789012Z"}`)
	// nil branch: covens empty array; spec/state/status_details/created_by_aid → null;
	// last_drift_check_at/last_drift_summary → key omitted (omitempty).
	goldenIncarnationWire(t, "GetReply/nil_optionals",
		IncarnationGetReply{
			Covens: []string{}, CreatedAt: ts, CreatedByAID: nil,
			LastDriftCheckAt: nil, LastDriftSummary: nil, Name: "redis-prod", Service: "redis",
			ServiceVersion: "v2.0.0", Spec: nil, State: nil, StateSchemaVersion: 1,
			Status: IncarnationStatusReady, StatusDetails: nil, UpdatedAt: ts2,
		},
		`{"covens":[],"created_at":"2026-06-14T12:34:56.789012345Z","created_by_aid":null,"name":"redis-prod","service":"redis","service_version":"v2.0.0","spec":null,"state":null,"state_schema_version":1,"status":"ready","status_details":null,"updated_at":"2026-06-13T01:02:03.456789012Z"}`)
}

// TestGoldenWire_IncarnationProjection verifies that the projection of domain handlers.*View →
// native reply-DTO keeps byte-exact wire against the pinned reference. Catches a regression in
// the field mapping itself (swapped/missing/nil-dropped).
func TestGoldenWire_IncarnationProjection(t *testing.T) {
	ts := time.Date(2026, 6, 14, 12, 0, 0, 123456789, time.UTC)
	aid := "archon-bob"
	m := map[string]any{"k": "v"}

	getV := handlers.IncarnationGetView{
		Covens: []string{"a"}, CreatedAt: ts, CreatedByAID: &aid, LastDriftCheckAt: &ts,
		LastDriftSummary: &handlers.DriftScanSummaryView{HostsDrifted: 2, ScannedAt: ts, TotalHosts: 2},
		Name:             "x", Service: "s", ServiceVersion: "v1", Spec: m, State: m,
		StateSchemaVersion: 7, Status: "applying", StatusDetails: m, UpdatedAt: ts,
	}
	goldenIncarnationWire(t, "proj/GetReply", newIncarnationGetReply(getV),
		`{"covens":["a"],"created_at":"2026-06-14T12:00:00.123456789Z","created_by_aid":"archon-bob","last_drift_check_at":"2026-06-14T12:00:00.123456789Z","last_drift_summary":{"hosts_clean":0,"hosts_drifted":2,"hosts_failed":0,"hosts_unsupported":0,"scanned_at":"2026-06-14T12:00:00.123456789Z","total_hosts":2},"name":"x","service":"s","service_version":"v1","spec":{"k":"v"},"state":{"k":"v"},"state_schema_version":7,"status":"applying","status_details":{"k":"v"},"updated_at":"2026-06-14T12:00:00.123456789Z"}`)

	histV := handlers.StateHistoryView{ApplyID: "ap", ChangedByAID: &aid, CreatedAt: ts, HistoryID: "h", Scenario: "create", StateAfter: m, StateBefore: m}
	goldenIncarnationWire(t, "proj/StateHistoryEntry", newStateHistoryEntry(histV),
		`{"apply_id":"ap","changed_by_aid":"archon-bob","created_at":"2026-06-14T12:00:00.123456789Z","history_id":"h","scenario":"create","state_after":{"k":"v"},"state_before":{"k":"v"}}`)

	createV := handlers.IncarnationCreateView{ApplyID: nil, Incarnation: "x"}
	goldenIncarnationWire(t, "proj/CreateReply", newIncarnationCreateReply(createV),
		`{"incarnation":"x"}`)

	unlockV := handlers.IncarnationUnlockView{Name: "x", PreviousStatus: "error_locked", Status: "ready", UnlockedAt: ts, UnlockedByAID: aid}
	goldenIncarnationWire(t, "proj/UnlockReply", newIncarnationUnlockReply(unlockV),
		`{"name":"x","previous_status":"error_locked","status":"ready","unlocked_at":"2026-06-14T12:00:00.123456789Z","unlocked_by_aid":"archon-bob"}`)
}

// TestGoldenWire_IncarnationGetReply_TraitsCreatedScenario pins the projection of two
// additive GET-response fields (ADR-060 traits + created_scenario, the multi-create
// mechanism): non-empty values reach the wire in alphabetical positions (created_scenario
// after created_by_aid, traits after status_details), empty ones dropped by omitempty.
// Bug source: the UI traits-modal opened empty (no prefill) — the fields were not returned.
func TestGoldenWire_IncarnationGetReply_TraitsCreatedScenario(t *testing.T) {
	ts := time.Date(2026, 6, 14, 12, 0, 0, 123456789, time.UTC)
	m := map[string]any{"k": "v"}

	// Non-empty traits (scalar + list — Trait is polymorphic) + created_scenario reach the wire.
	full := handlers.IncarnationGetView{
		Covens: []string{}, CreatedAt: ts, CreatedScenario: "create_cluster",
		Name: "x", Service: "s", ServiceVersion: "v1", Spec: m, State: m,
		StateSchemaVersion: 7, Status: "ready", StatusDetails: m,
		Traits: map[string]any{"env": "prod", "az": []any{"a", "b"}}, UpdatedAt: ts,
	}
	goldenIncarnationWire(t, "GetReply/traits+created_scenario", newIncarnationGetReply(full),
		`{"covens":[],"created_at":"2026-06-14T12:00:00.123456789Z","created_by_aid":null,"created_scenario":"create_cluster","name":"x","service":"s","service_version":"v1","spec":{"k":"v"},"state":{"k":"v"},"state_schema_version":7,"status":"ready","status_details":{"k":"v"},"traits":{"az":["a","b"],"env":"prod"},"updated_at":"2026-06-14T12:00:00.123456789Z"}`)

	// Empty: created_scenario "" + traits {} → omitempty drops both keys (byte-exact
	// with the shape before the additive change — backward compatibility for old clients).
	empty := handlers.IncarnationGetView{
		Covens: []string{}, CreatedAt: ts, CreatedScenario: "",
		Name: "x", Service: "s", ServiceVersion: "v1", Spec: nil, State: nil,
		StateSchemaVersion: 1, Status: "ready", StatusDetails: nil,
		Traits: map[string]any{}, UpdatedAt: ts,
	}
	goldenIncarnationWire(t, "GetReply/traits+created_scenario empty", newIncarnationGetReply(empty),
		`{"covens":[],"created_at":"2026-06-14T12:00:00.123456789Z","created_by_aid":null,"name":"x","service":"s","service_version":"v1","spec":null,"state":null,"state_schema_version":1,"status":"ready","status_details":null,"updated_at":"2026-06-14T12:00:00.123456789Z"}`)
}
