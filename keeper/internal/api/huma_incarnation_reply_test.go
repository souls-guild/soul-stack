// GOLDEN byte-exact wire-guard for the NATIVE wire-DTO of the INCARNATION domain (handler-native T5d).
// incarnation no longer depends on the legacy generator — golden checks json native values against a
// pinned reference string. Covers all ADR-051 categories:
//
//   - category A (date-time): same RFC3339Nano bytes;
//   - category B ([]-vs-null): covens without omitempty;
//   - category C (omitempty): apply_id/changed_by_aid — key omitted when nil;
//   - category D (nullable): spec/state/status_details/created_by_aid — `null` when nil.
//
// Both pointer states covered (nil and non-nil). Mutating the native-struct shape reddens the case.
package api

import (
	"bytes"
	"encoding/json"
	"testing"
	"time"

	"github.com/souls-guild/soul-stack/keeper/internal/api/handlers"
	"github.com/souls-guild/soul-stack/keeper/internal/applyrun"
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
		IncarnationUnlockReply{ID: "redis-prod", PreviousStatus: IncarnationStatusErrorLocked, Status: IncarnationStatusReady, UnlockedAt: ts, UnlockedByAID: aid},
		`{"id":"redis-prod","previous_status":"error_locked","status":"ready","unlocked_at":"2026-06-14T12:34:56.789012345Z","unlocked_by_aid":"archon-alice"}`)

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

	// --- StateHistoryEntry (nested): changed_by_aid omitempty + state_* nullable ---
	goldenIncarnationWire(t, "StateHistoryEntry/full",
		StateHistoryEntry{ApplyID: apply, ChangedByAID: &aid, CreatedAt: ts, HistoryID: "h1", Scenario: "create", StateAfter: &stateMap, StateBefore: &specMap},
		`{"apply_id":"01J0APPLYULID","changed_by_aid":"archon-alice","created_at":"2026-06-14T12:34:56.789012345Z","history_id":"h1","scenario":"create","state_after":{"users":{"app":true}},"state_before":{"hosts":["web1"]}}`)
	goldenIncarnationWire(t, "StateHistoryEntry/nil_optionals",
		StateHistoryEntry{ApplyID: apply, ChangedByAID: nil, CreatedAt: ts, HistoryID: "h1", Scenario: "migration", StateAfter: nil, StateBefore: nil},
		`{"apply_id":"01J0APPLYULID","created_at":"2026-06-14T12:34:56.789012345Z","history_id":"h1","scenario":"migration","state_after":null,"state_before":null}`)

	// --- IncarnationGetReply: all categories (date-time + []-vs-null + omitempty + nullable) ---
	// status stays `drift` on the wire: NIM-446 removed the drift CHECK, not the
	// status — a legacy upgrade still leaves an incarnation in it (ADR-031(d)).
	goldenIncarnationWire(t, "GetReply/full",
		IncarnationGetReply{
			Covens: []string{"prod", "eu"}, CreatedAt: ts, CreatedByAID: &aid,
			ID: "redis-prod", Service: "redis",
			ServiceVersion: "v2.0.0", State: &stateMap, StateSchemaVersion: 3,
			Status: IncarnationStatusDrift, StatusDetails: &stateMap, UpdatedAt: ts2,
		},
		`{"covens":["prod","eu"],"created_at":"2026-06-14T12:34:56.789012345Z","created_by_aid":"archon-alice","id":"redis-prod","service":"redis","service_version":"v2.0.0","state":{"users":{"app":true}},"state_schema_version":3,"status":"drift","status_details":{"users":{"app":true}},"updated_at":"2026-06-13T01:02:03.456789012Z"}`)
	// nil branch: covens empty array; spec/state/status_details/created_by_aid → null.
	goldenIncarnationWire(t, "GetReply/nil_optionals",
		IncarnationGetReply{
			Covens: []string{}, CreatedAt: ts, CreatedByAID: nil,
			ID: "redis-prod", Service: "redis",
			ServiceVersion: "v2.0.0", State: nil, StateSchemaVersion: 1,
			Status: IncarnationStatusReady, StatusDetails: nil, UpdatedAt: ts2,
		},
		`{"covens":[],"created_at":"2026-06-14T12:34:56.789012345Z","created_by_aid":null,"id":"redis-prod","service":"redis","service_version":"v2.0.0","state":null,"state_schema_version":1,"status":"ready","status_details":null,"updated_at":"2026-06-13T01:02:03.456789012Z"}`)
}

// TestGoldenWire_IncarnationProjection verifies that the projection of domain handlers.*View →
// native reply-DTO keeps byte-exact wire against the pinned reference. Catches a regression in
// the field mapping itself (swapped/missing/nil-dropped).
func TestGoldenWire_IncarnationProjection(t *testing.T) {
	ts := time.Date(2026, 6, 14, 12, 0, 0, 123456789, time.UTC)
	aid := "archon-bob"
	m := map[string]any{"k": "v"}

	getV := handlers.IncarnationGetView{
		Covens: []string{"a"}, CreatedAt: ts, CreatedByAID: &aid,
		ID: "x", Service: "s", ServiceVersion: "v1", State: m,
		StateSchemaVersion: 7, Status: "applying", StatusDetails: m, UpdatedAt: ts,
	}
	goldenIncarnationWire(t, "proj/GetReply", newIncarnationGetReply(getV),
		`{"covens":["a"],"created_at":"2026-06-14T12:00:00.123456789Z","created_by_aid":"archon-bob","id":"x","service":"s","service_version":"v1","state":{"k":"v"},"state_schema_version":7,"status":"applying","status_details":{"k":"v"},"updated_at":"2026-06-14T12:00:00.123456789Z"}`)

	histV := handlers.StateHistoryView{ApplyID: "ap", ChangedByAID: &aid, CreatedAt: ts, HistoryID: "h", Scenario: "create", StateAfter: m, StateBefore: m}
	goldenIncarnationWire(t, "proj/StateHistoryEntry", newStateHistoryEntry(histV),
		`{"apply_id":"ap","changed_by_aid":"archon-bob","created_at":"2026-06-14T12:00:00.123456789Z","history_id":"h","scenario":"create","state_after":{"k":"v"},"state_before":{"k":"v"}}`)

	createV := handlers.IncarnationCreateView{ApplyID: nil, Incarnation: "x"}
	goldenIncarnationWire(t, "proj/CreateReply", newIncarnationCreateReply(createV),
		`{"incarnation":"x"}`)

	unlockV := handlers.IncarnationUnlockView{ID: "x", PreviousStatus: "error_locked", Status: "ready", UnlockedAt: ts, UnlockedByAID: aid}
	goldenIncarnationWire(t, "proj/UnlockReply", newIncarnationUnlockReply(unlockV),
		`{"id":"x","previous_status":"error_locked","status":"ready","unlocked_at":"2026-06-14T12:00:00.123456789Z","unlocked_by_aid":"archon-bob"}`)
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
		ID: "x", Service: "s", ServiceVersion: "v1", State: m,
		StateSchemaVersion: 7, Status: "ready", StatusDetails: m,
		Traits: map[string]any{"env": "prod", "az": []any{"a", "b"}}, UpdatedAt: ts,
	}
	goldenIncarnationWire(t, "GetReply/traits+created_scenario", newIncarnationGetReply(full),
		`{"covens":[],"created_at":"2026-06-14T12:00:00.123456789Z","created_by_aid":null,"created_scenario":"create_cluster","id":"x","service":"s","service_version":"v1","state":{"k":"v"},"state_schema_version":7,"status":"ready","status_details":{"k":"v"},"traits":{"az":["a","b"],"env":"prod"},"updated_at":"2026-06-14T12:00:00.123456789Z"}`)

	// Empty: created_scenario "" + traits {} → omitempty drops both keys (byte-exact
	// with the shape before the additive change — backward compatibility for old clients).
	empty := handlers.IncarnationGetView{
		Covens: []string{}, CreatedAt: ts, CreatedScenario: "",
		ID: "x", Service: "s", ServiceVersion: "v1", State: nil,
		StateSchemaVersion: 1, Status: "ready", StatusDetails: nil,
		Traits: map[string]any{}, UpdatedAt: ts,
	}
	goldenIncarnationWire(t, "GetReply/traits+created_scenario empty", newIncarnationGetReply(empty),
		`{"covens":[],"created_at":"2026-06-14T12:00:00.123456789Z","created_by_aid":null,"id":"x","service":"s","service_version":"v1","state":null,"state_schema_version":1,"status":"ready","status_details":null,"updated_at":"2026-06-14T12:00:00.123456789Z"}`)
}

// NIM-985: the wire form of params_by_host. A shared cell carries no sids, a hunk
// carries line/line_count, a variant with no lines on a host reads absent, and a
// task without per-host rows omits the field.
func TestGoldenWire_RunTasksReply_ParamsByHost(t *testing.T) {
	reply := newRunTasksReply(handlers.RunTasksView{Tasks: []handlers.RunTaskView{
		{PlanIndex: 0, Name: "write", Module: "core.file.present",
			Params: map[string]any{"content": "<per-host>", "path": "/etc/x"},
			ParamsByHost: &applyrun.ParamsByHost{
				Hosts: 2,
				Cells: []applyrun.ParamCell{
					{Path: "content", Line: 2, LineCount: 1, Values: []applyrun.ParamVariant{
						{Lines: []string{"ip 10.0.0.1"}, SIDs: []string{"a"}},
						{Absent: true, SIDs: []string{"b"}},
					}},
					{Path: "path", Same: true, Values: []applyrun.ParamVariant{{Value: "/etc/x"}}},
				},
				Texts: []applyrun.ParamText{{Path: "content", Lines: []string{"port 1", "ip 10.0.0.1"}}},
			}},
		{PlanIndex: 1, Name: "plain", Module: "core.exec.run", Params: map[string]any{"cmd": "true"}},
	}})
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(reply); err != nil {
		t.Fatalf("marshal: %v", err)
	}
	b := bytes.TrimSpace(buf.Bytes())
	want := `{"tasks":[` +
		`{"plan_index":0,"passage":0,"id":"write","module":"core.file.present","params":{"content":"<per-host>","path":"/etc/x"},` +
		`"params_by_host":{"hosts":2,"cells":[` +
		`{"path":"content","line":2,"line_count":1,"same":false,"values":[{"lines":["ip 10.0.0.1"],"sids":["a"]},{"absent":true,"sids":["b"]}]},` +
		`{"path":"path","same":true,"values":[{"value":"/etc/x"}]}],` +
		`"texts":[{"path":"content","lines":["port 1","ip 10.0.0.1"]}]},"hosts":[]},` +
		`{"plan_index":1,"passage":0,"id":"plain","module":"core.exec.run","params":{"cmd":"true"},"hosts":[]}]}`
	if string(b) != want {
		t.Errorf("wire =\n%s\nwant\n%s", b, want)
	}
}

// NIM-996: a serial: step's waves ride its tasks in dispatch order, and a task sent in
// one wave omits the key; a dropped host's row says so, every other row omits it, so
// the body of a run without either is byte-for-byte what it was.
func TestGoldenWire_RunWavesAndDropped(t *testing.T) {
	encode := func(v any) string {
		var buf bytes.Buffer
		enc := json.NewEncoder(&buf)
		enc.SetEscapeHTML(false)
		if err := enc.Encode(v); err != nil {
			t.Fatalf("marshal: %v", err)
		}
		return string(bytes.TrimSpace(buf.Bytes()))
	}

	tasks := newRunTasksReply(handlers.RunTasksView{Tasks: []handlers.RunTaskView{
		{PlanIndex: 0, Passage: 1, Name: "upgrade", Module: "core.exec.run", Waves: [][]string{{"b"}, {"a", "c"}}},
		{PlanIndex: 1, Passage: 2, Name: "report", Module: "core.exec.run"},
	}})
	wantTasks := `{"tasks":[` +
		`{"plan_index":0,"passage":1,"id":"upgrade","module":"core.exec.run","waves":[["b"],["a","c"]],"hosts":[]},` +
		`{"plan_index":1,"passage":2,"id":"report","module":"core.exec.run","hosts":[]}]}`
	if got := encode(tasks); got != wantTasks {
		t.Errorf("tasks wire =\n%s\nwant\n%s", got, wantTasks)
	}

	detail := newRunDetailReply(handlers.RunDetailView{
		ApplyID: "01M4XGKBF2WQS7SBZP6QDKV9GA", Scenario: "canary", Status: "partial_failed",
		Hosts: []handlers.RunHostStatusView{
			{SID: "a", Status: "failed", Passage: 0, Dropped: true},
			{SID: "b", Status: "success", Passage: 0},
		},
	})
	got := encode(detail.Hosts)
	wantHosts := `[{"sid":"a","status":"failed","passage":0,"attempt":0,"cancel_requested":false,"dropped":true},` +
		`{"sid":"b","status":"success","passage":0,"attempt":0,"cancel_requested":false}]`
	if got != wantHosts {
		t.Errorf("hosts wire =\n%s\nwant\n%s", got, wantHosts)
	}
}

// TestGoldenWire_RunHostHandled pins `handled` on the run's host row (NIM-993): the
// key is present on a handled row, which keeps its failure fields, and absent on
// every other row, so an unaffected run's body is what it was.
func TestGoldenWire_RunHostHandled(t *testing.T) {
	planIdx, taskIdx := 3, 1
	summary := "task 1 core.exec.run: boom"
	detail := newRunDetailReply(handlers.RunDetailView{
		ApplyID: "01M4XGKBF2WQS7SBZP6QDKV9GA", Scenario: "deploy", Status: "success",
		Hosts: []handlers.RunHostStatusView{
			{SID: "a", Status: "failed", FailedTaskIdx: &taskIdx, FailedPlanIndex: &planIdx, ErrorSummary: &summary, Handled: true},
			{SID: "b", Status: "success"},
		},
	})
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(detail.Hosts); err != nil {
		t.Fatalf("marshal: %v", err)
	}
	want := `[{"sid":"a","status":"failed","passage":0,"failed_task_idx":1,"failed_plan_index":3,"error_summary":"task 1 core.exec.run: boom","attempt":0,"cancel_requested":false,"handled":true},` +
		`{"sid":"b","status":"success","passage":0,"attempt":0,"cancel_requested":false}]`
	if got := string(bytes.TrimSpace(buf.Bytes())); got != want {
		t.Errorf("hosts wire =\n%s\nwant\n%s", got, want)
	}
}
