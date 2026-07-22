// GOLDEN byte-exact wire-guard for the NATIVE wire-DTO of the PUSH domain (handler-native T5d). push
// no longer depends on the legacy generator — golden compares the native JSON values against a PINNED
// reference string. Both pointer states (nil/non-nil) are covered — omitempty
// and nullable branches. A shape mutation of the native struct reddens the case.
package api

import (
	"encoding/json"
	"testing"
	"time"
)

func goldenPushWire(t *testing.T, name string, native any, want string) {
	t.Helper()
	got, err := json.Marshal(native)
	if err != nil {
		t.Fatalf("%s: marshal native: %v", name, err)
	}
	if string(got) != want {
		t.Errorf("%s: WIRE DRIFT\n got  = %s\n want = %s", name, got, want)
	}
}

func TestGoldenWire_PushReply(t *testing.T) {
	ts := time.Date(2026, 6, 14, 12, 34, 56, 789012345, time.UTC)
	ts2 := time.Date(2026, 6, 13, 1, 2, 3, 456789012, time.UTC)
	apply := "01J0PUSHULID"
	aid := "archon-alice"
	ssh := "openssh"
	inputMap := map[string]interface{}{"port": 6379}
	summaryMap := map[string]interface{}{"total": 3, "success_count": 2, "fail_count": 1}
	cnt := 2

	// --- PushApplyReply ---
	goldenPushWire(t, "PushApplyReply",
		PushApplyReply{ApplyID: apply},
		`{"apply_id":"01J0PUSHULID"}`)

	// --- PushApplyView: all omitempty branches filled ---
	goldenPushWire(t, "PushApplyView/full",
		PushApplyView{
			ApplyID: apply, CleanupStale: true, DestinyRef: "redis@v2.0.0",
			FinishedAt: &ts, Input: &inputMap, InventorySids: []string{"web1.example.com"},
			SSHProvider: &ssh, StartedAt: ts2, StartedByAID: &aid,
			Status: PushApplyViewStatus("success"), Summary: &summaryMap,
		},
		`{"apply_id":"01J0PUSHULID","cleanup_stale":true,"destiny_ref":"redis@v2.0.0","finished_at":"2026-06-14T12:34:56.789012345Z","input":{"port":6379},"inventory_sids":["web1.example.com"],"ssh_provider":"openssh","started_at":"2026-06-13T01:02:03.456789012Z","started_by_aid":"archon-alice","status":"success","summary":{"fail_count":1,"success_count":2,"total":3}}`)
	// nil branch: finished_at/input/ssh_provider/started_by_aid/summary → key omitted.
	goldenPushWire(t, "PushApplyView/nil_optionals",
		PushApplyView{
			ApplyID: apply, CleanupStale: false, DestinyRef: "redis@main",
			FinishedAt: nil, Input: nil, InventorySids: []string{"web1.example.com", "web2.example.com"},
			SSHProvider: nil, StartedAt: ts2, StartedByAID: nil,
			Status: PushApplyViewStatus("running"), Summary: nil,
		},
		`{"apply_id":"01J0PUSHULID","cleanup_stale":false,"destiny_ref":"redis@main","inventory_sids":["web1.example.com","web2.example.com"],"started_at":"2026-06-13T01:02:03.456789012Z","status":"running"}`)

	// --- PushSummaryCounts (nested): omitempty both branches ---
	goldenPushWire(t, "PushSummaryCounts/full",
		PushSummaryCounts{FailCount: &cnt, SuccessCount: &cnt, Total: &cnt},
		`{"fail_count":2,"success_count":2,"total":2}`)
	goldenPushWire(t, "PushSummaryCounts/nil",
		PushSummaryCounts{},
		`{}`)

	// --- PushRunListEntry (nested element) ---
	entN := PushRunListEntry{
		ApplyID: apply, CleanupStale: true, DestinyRef: "redis@v2.0.0",
		FinishedAt: &ts, InventorySids: []string{"web1.example.com"}, SSHProvider: &ssh,
		StartedAt: ts2, StartedByAID: &aid, Status: PushRunListEntryStatus("success"),
		SummaryCounts: &PushSummaryCounts{FailCount: &cnt, SuccessCount: &cnt, Total: &cnt},
	}
	goldenPushWire(t, "PushRunListEntry/full", entN,
		`{"apply_id":"01J0PUSHULID","cleanup_stale":true,"destiny_ref":"redis@v2.0.0","finished_at":"2026-06-14T12:34:56.789012345Z","inventory_sids":["web1.example.com"],"ssh_provider":"openssh","started_at":"2026-06-13T01:02:03.456789012Z","started_by_aid":"archon-alice","status":"success","summary_counts":{"fail_count":2,"success_count":2,"total":2}}`)
	goldenPushWire(t, "PushRunListEntry/nil_optionals",
		PushRunListEntry{
			ApplyID: apply, CleanupStale: false, DestinyRef: "redis@main",
			FinishedAt: nil, InventorySids: []string{"web1.example.com"}, SSHProvider: nil,
			StartedAt: ts2, StartedByAID: nil, Status: PushRunListEntryStatus("pending"), SummaryCounts: nil,
		},
		`{"apply_id":"01J0PUSHULID","cleanup_stale":false,"destiny_ref":"redis@main","inventory_sids":["web1.example.com"],"started_at":"2026-06-13T01:02:03.456789012Z","status":"pending"}`)

	// --- PushRunListReply (envelope as a top-level reply-DTO): items non-nil + offset/limit/total ---
	goldenPushWire(t, "PushRunListReply/full",
		PushRunListReply{Items: []PushRunListEntry{entN}, Limit: 50, Offset: 0, Total: 1},
		`{"items":[{"apply_id":"01J0PUSHULID","cleanup_stale":true,"destiny_ref":"redis@v2.0.0","finished_at":"2026-06-14T12:34:56.789012345Z","inventory_sids":["web1.example.com"],"ssh_provider":"openssh","started_at":"2026-06-13T01:02:03.456789012Z","started_by_aid":"archon-alice","status":"success","summary_counts":{"fail_count":2,"success_count":2,"total":2}}],"limit":50,"offset":0,"total":1}`)
	// items empty []: byte-exact `[]` (not null).
	goldenPushWire(t, "PushRunListReply/empty_items",
		PushRunListReply{Items: []PushRunListEntry{}, Limit: 50, Offset: 10, Total: 0},
		`{"items":[],"limit":50,"offset":10,"total":0}`)
}
