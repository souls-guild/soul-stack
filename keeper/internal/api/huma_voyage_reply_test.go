// GOLDEN byte-exact wire-guard for the NATIVE wire-DTO of the VOYAGE domain (handler-native T5d). voyage
// no longer depends on the legacy generator — golden compares json native values against a PINNED
// reference string. Guarantees the wire shape:
//
//   - category A (date-time): created_at/started_at/finished_at/schedule_at → the same RFC3339Nano;
//   - category C (omitempty): batch_*/concurrency/fail_threshold/module/on_failure/require_alive/
//     scenario_name/summary/target/no_match/apply_id/errand_id/finished_at/effective_batch_size —
//     key omitted when nil; required fields are present;
//   - enum kind/status/batch_mode/on_failure/target_kind: the same string byte value;
//   - nested Voyage→Summary (native VoyageSummary) and Voyage→Target (class-A reuse api.VoyageTarget).
//
// ★ TARGET (class-A normalization). native api.VoyageTarget — value-slice/value-string WITH omitempty.
// For a nil pointer AND for a populated slice (what the handler actually produces via unmarshal of
// target_origin), the wire is byte-exact. A non-nil-empty pointer is normalized by native into an
// omitted key — accepted class-A compatibility, NOT tested here (the handler never produces that shape).
//
// Mutating the native struct shape (dropping omitempty / changing a json tag / changing a field type) reds out the case.
package api

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/souls-guild/soul-stack/keeper/internal/api/handlers"
)

func goldenVoyageWire(t *testing.T, name string, native any, want string) {
	t.Helper()
	got, err := json.Marshal(native)
	if err != nil {
		t.Fatalf("%s: marshal native: %v", name, err)
	}
	if string(got) != want {
		t.Errorf("%s: WIRE DRIFT\n got  = %s\n want = %s", name, got, want)
	}
}

func TestGoldenWire_VoyageReply(t *testing.T) {
	ts := time.Date(2026, 6, 14, 12, 34, 56, 789012345, time.UTC)
	ts2 := time.Date(2026, 6, 13, 1, 2, 3, 456789012, time.UTC)

	// --- VoyageCreateReply (all required) ---
	goldenVoyageWire(t, "CreateReply",
		VoyageCreateReply{Kind: VoyageCreateReplyKind("scenario"), Location: "/v1/voyages/01H", ScopeSize: 5, Status: VoyageCreateReplyStatus("pending"), VoyageID: "01H"},
		`{"kind":"scenario","location":"/v1/voyages/01H","scope_size":5,"status":"pending","voyage_id":"01H"}`)

	// --- VoyagePreviewReply: effective_batch_size set (barrier) ---
	ebs := 3
	goldenVoyageWire(t, "PreviewReply/barrier_full",
		VoyagePreviewReply{BatchMode: VoyagePreviewReplyBatchMode("barrier"), EffectiveBatchSize: &ebs, Kind: VoyagePreviewReplyKind("command"), ScopeSize: 12, TotalBatches: 4},
		`{"batch_mode":"barrier","effective_batch_size":3,"kind":"command","scope_size":12,"total_batches":4}`)
	// --- VoyagePreviewReply: effective_batch_size omitted (window, omitempty) ---
	goldenVoyageWire(t, "PreviewReply/window_nil_ebs",
		VoyagePreviewReply{BatchMode: VoyagePreviewReplyBatchMode("window"), EffectiveBatchSize: nil, Kind: VoyagePreviewReplyKind("command"), ScopeSize: 12, TotalBatches: 1},
		`{"batch_mode":"window","kind":"command","scope_size":12,"total_batches":1}`)

	// --- VoyageCancelReply (required) ---
	goldenVoyageWire(t, "CancelReply",
		VoyageCancelReply{Status: VoyageCancelReplyStatus("cancelled"), VoyageID: "01H"},
		`{"status":"cancelled","voyage_id":"01H"}`)

	// --- VoyageSummary: no_match set / omitted ---
	nm := 2
	goldenVoyageWire(t, "Summary/no_match_set",
		VoyageSummary{Cancelled: 1, Failed: 0, NoMatch: &nm, Succeeded: 7, Total: 10},
		`{"cancelled":1,"failed":0,"no_match":2,"succeeded":7,"total":10}`)
	goldenVoyageWire(t, "Summary/no_match_nil",
		VoyageSummary{Cancelled: 0, Failed: 1, NoMatch: nil, Succeeded: 9, Total: 10},
		`{"cancelled":0,"failed":1,"succeeded":9,"total":10}`)

	// --- VoyageTargetEntry: scenario (apply_id+finished_at) / command-running (errand_id, nil-optionals) ---
	aid := "01APPLY"
	goldenVoyageWire(t, "TargetEntry/scenario",
		VoyageTargetEntry{ApplyID: &aid, BatchIndex: 0, FinishedAt: &ts2, Status: VoyageTargetEntryStatus("succeeded"), TargetID: "web-prod", TargetKind: VoyageTargetEntryTargetKind("incarnation")},
		`{"apply_id":"01APPLY","batch_index":0,"finished_at":"2026-06-13T01:02:03.456789012Z","status":"succeeded","target_id":"web-prod","target_kind":"incarnation"}`)
	eid := "01ERRAND"
	goldenVoyageWire(t, "TargetEntry/command_running",
		VoyageTargetEntry{ErrandID: &eid, BatchIndex: 1, Status: VoyageTargetEntryStatus("running"), TargetID: "h1.example.com", TargetKind: VoyageTargetEntryTargetKind("sid")},
		`{"batch_index":1,"errand_id":"01ERRAND","status":"running","target_id":"h1.example.com","target_kind":"sid"}`)

	// --- VoyageTargetsReply (required voyage_id+targets[]) ---
	goldenVoyageWire(t, "TargetsReply",
		VoyageTargetsReply{
			Targets: []VoyageTargetEntry{
				{ApplyID: &aid, BatchIndex: 0, FinishedAt: &ts2, Status: "succeeded", TargetID: "web-prod", TargetKind: "incarnation"},
				{ErrandID: &eid, BatchIndex: 1, Status: "running", TargetID: "h1", TargetKind: "sid"},
			},
			VoyageID: "01H",
		},
		`{"targets":[{"apply_id":"01APPLY","batch_index":0,"finished_at":"2026-06-13T01:02:03.456789012Z","status":"succeeded","target_id":"web-prod","target_kind":"incarnation"},{"batch_index":1,"errand_id":"01ERRAND","status":"running","target_id":"h1","target_kind":"sid"}],"voyage_id":"01H"}`)

	// --- Voyage: FULL (all optional fields set; nested Summary + Target populated) ---
	nbm := VoyageBatchMode("barrier")
	nof := VoyageOnFailure("continue")
	bsz, bpc, conc, fth := 4, 25, 2, 1
	ra := true
	scen, mod := "deploy", "core.cmd.shell"
	// Target populated: non-empty slice/string → byte-exact native (value-slice).
	incs := []string{"web-prod", "db-prod"}
	svc := "web"
	sids := []string{"h1", "h2"}
	where := "soulprint.self.os.family == 'debian'"
	coven := []string{"prod", "eu"}
	nativeTarget := &VoyageTarget{Incarnations: incs, Service: svc, SIDs: sids, Where: where, Coven: coven}

	nativeFull := Voyage{
		Attempt: 1, BatchMode: &nbm, BatchPercent: &bpc, BatchSize: &bsz, Concurrency: &conc,
		CreatedAt: ts, CurrentBatchIndex: 2, DryRun: false, FailThreshold: &fth, FinishedAt: &ts2,
		Kind: VoyageKind("scenario"), Module: &mod, OnFailure: &nof, RequireAlive: &ra,
		ScenarioName: &scen, ScheduleAt: &ts2, ScopeSize: 7, StartedAt: &ts, StartedByAID: "archon-alice",
		Status:  VoyageStatus("running"),
		Summary: &VoyageSummary{Cancelled: 0, Failed: 1, NoMatch: &nm, Succeeded: 5, Total: 7},
		Target:  nativeTarget, TotalBatches: 3, VoyageID: "01H",
	}
	goldenVoyageWire(t, "Voyage/full", nativeFull,
		`{"attempt":1,"batch_mode":"barrier","batch_percent":25,"batch_size":4,"concurrency":2,"created_at":"2026-06-14T12:34:56.789012345Z","current_batch_index":2,"dry_run":false,"fail_threshold":1,"finished_at":"2026-06-13T01:02:03.456789012Z","kind":"scenario","module":"core.cmd.shell","on_failure":"continue","require_alive":true,"scenario_name":"deploy","schedule_at":"2026-06-13T01:02:03.456789012Z","scope_size":7,"started_at":"2026-06-14T12:34:56.789012345Z","started_by_aid":"archon-alice","status":"running","summary":{"cancelled":0,"failed":1,"no_match":2,"succeeded":5,"total":7},"target":{"coven":["prod","eu"],"incarnations":["web-prod","db-prod"],"service":"web","sids":["h1","h2"],"where":"soulprint.self.os.family == 'debian'"},"total_batches":3,"voyage_id":"01H"}`)

	// --- Voyage: MINIMAL (only required fields; all optional nil → keys omitted, target/summary omitted) ---
	nativeMin := Voyage{
		Attempt: 0, CreatedAt: ts, CurrentBatchIndex: 0, DryRun: true,
		Kind: VoyageKind("command"), ScopeSize: 0, StartedByAID: "archon-bob",
		Status: VoyageStatus("pending"), TotalBatches: 1, VoyageID: "01J",
	}
	goldenVoyageWire(t, "Voyage/minimal", nativeMin,
		`{"attempt":0,"created_at":"2026-06-14T12:34:56.789012345Z","current_batch_index":0,"dry_run":true,"kind":"command","scope_size":0,"started_by_aid":"archon-bob","status":"pending","total_batches":1,"voyage_id":"01J"}`)

	// --- VoyageListReply: full (items populated) ---
	goldenVoyageWire(t, "ListReply/full",
		VoyageListReply{Items: []Voyage{nativeFull, nativeMin}, Limit: 50, Offset: 0, Total: 2},
		`{"items":[{"attempt":1,"batch_mode":"barrier","batch_percent":25,"batch_size":4,"concurrency":2,"created_at":"2026-06-14T12:34:56.789012345Z","current_batch_index":2,"dry_run":false,"fail_threshold":1,"finished_at":"2026-06-13T01:02:03.456789012Z","kind":"scenario","module":"core.cmd.shell","on_failure":"continue","require_alive":true,"scenario_name":"deploy","schedule_at":"2026-06-13T01:02:03.456789012Z","scope_size":7,"started_at":"2026-06-14T12:34:56.789012345Z","started_by_aid":"archon-alice","status":"running","summary":{"cancelled":0,"failed":1,"no_match":2,"succeeded":5,"total":7},"target":{"coven":["prod","eu"],"incarnations":["web-prod","db-prod"],"service":"web","sids":["h1","h2"],"where":"soulprint.self.os.family == 'debian'"},"total_batches":3,"voyage_id":"01H"},{"attempt":0,"created_at":"2026-06-14T12:34:56.789012345Z","current_batch_index":0,"dry_run":true,"kind":"command","scope_size":0,"started_by_aid":"archon-bob","status":"pending","total_batches":1,"voyage_id":"01J"}],"limit":50,"offset":0,"total":2}`)
}

// TestGoldenWire_VoyageConverters verifies that the handlers.X → api-native projectors preserve
// byte-exact wire output against the pinned reference (not just the type shape): it marshals the
// projector's result and compares bytes. handler-native T5d: the projector input is a flat handlers-DTO
// (plain-string enum, pointer-slice target). Catches regressions in the field mapping itself (swapped/
// missing/nil-mishandled fields), including the deep Voyage→Summary/Target chain and the nil-Items branch.
func TestGoldenWire_VoyageConverters(t *testing.T) {
	ts := time.Date(2026, 6, 14, 12, 0, 0, 123456789, time.UTC)
	nm := 1
	incs := []string{"web"}
	where := "x == 1"

	createH := handlers.VoyageCreateReply{Kind: "scenario", Location: "/v1/voyages/1", ScopeSize: 3, Status: "scheduled", VoyageID: "1"}
	goldenVoyageWire(t, "conv/CreateReply", toVoyageCreateReply(createH),
		`{"kind":"scenario","location":"/v1/voyages/1","scope_size":3,"status":"scheduled","voyage_id":"1"}`)

	ebs := 2
	prevH := handlers.VoyagePreviewReply{BatchMode: "barrier", EffectiveBatchSize: &ebs, Kind: "command", ScopeSize: 6, TotalBatches: 3}
	goldenVoyageWire(t, "conv/PreviewReply", toVoyagePreviewReply(prevH),
		`{"batch_mode":"barrier","effective_batch_size":2,"kind":"command","scope_size":6,"total_batches":3}`)
	prevNilH := handlers.VoyagePreviewReply{BatchMode: "window", EffectiveBatchSize: nil, Kind: "command", ScopeSize: 6, TotalBatches: 1}
	goldenVoyageWire(t, "conv/PreviewReply_nil_ebs", toVoyagePreviewReply(prevNilH),
		`{"batch_mode":"window","kind":"command","scope_size":6,"total_batches":1}`)

	cancelH := handlers.VoyageCancelReply{Status: "cancelled", VoyageID: "1"}
	goldenVoyageWire(t, "conv/CancelReply", toVoyageCancelReply(cancelH),
		`{"status":"cancelled","voyage_id":"1"}`)

	bmH := "window"
	tgtH := &handlers.VoyageTargetDTO{Incarnations: &incs, Where: &where}
	voyH := handlers.VoyageDTO{
		Attempt: 2, BatchMode: &bmH, CreatedAt: ts, CurrentBatchIndex: 0, DryRun: false,
		Kind: "command", ScopeSize: 4, StartedByAID: "archon-x", Status: "running",
		Summary: &handlers.VoyageSummaryDTO{Cancelled: 0, Failed: 0, NoMatch: &nm, Succeeded: 3, Total: 4},
		Target:  tgtH, TotalBatches: 1, VoyageID: "1",
	}
	goldenVoyageWire(t, "conv/Voyage", toVoyage(voyH),
		`{"attempt":2,"batch_mode":"window","created_at":"2026-06-14T12:00:00.123456789Z","current_batch_index":0,"dry_run":false,"kind":"command","scope_size":4,"started_by_aid":"archon-x","status":"running","summary":{"cancelled":0,"failed":0,"no_match":1,"succeeded":3,"total":4},"target":{"incarnations":["web"],"where":"x == 1"},"total_batches":1,"voyage_id":"1"}`)
	// Voyage without summary/target (nil — both omitted).
	voyNilH := handlers.VoyageDTO{Attempt: 0, CreatedAt: ts, CurrentBatchIndex: 0, DryRun: true, Kind: "scenario", ScopeSize: 0, StartedByAID: "archon-y", Status: "pending", TotalBatches: 1, VoyageID: "2"}
	goldenVoyageWire(t, "conv/Voyage_nil_nested", toVoyage(voyNilH),
		`{"attempt":0,"created_at":"2026-06-14T12:00:00.123456789Z","current_batch_index":0,"dry_run":true,"kind":"scenario","scope_size":0,"started_by_aid":"archon-y","status":"pending","total_batches":1,"voyage_id":"2"}`)

	listH := handlers.VoyageListReply{Items: []handlers.VoyageDTO{voyH, voyNilH}, Limit: 10, Offset: 0, Total: 2}
	goldenVoyageWire(t, "conv/ListReply", toVoyageListReply(listH),
		`{"items":[{"attempt":2,"batch_mode":"window","created_at":"2026-06-14T12:00:00.123456789Z","current_batch_index":0,"dry_run":false,"kind":"command","scope_size":4,"started_by_aid":"archon-x","status":"running","summary":{"cancelled":0,"failed":0,"no_match":1,"succeeded":3,"total":4},"target":{"incarnations":["web"],"where":"x == 1"},"total_batches":1,"voyage_id":"1"},{"attempt":0,"created_at":"2026-06-14T12:00:00.123456789Z","current_batch_index":0,"dry_run":true,"kind":"scenario","scope_size":0,"started_by_aid":"archon-y","status":"pending","total_batches":1,"voyage_id":"2"}],"limit":10,"offset":0,"total":2}`)
	// nil-Items branch: handler-safe (schema requires items), but the projector must preserve nil → `null`.
	listNilH := handlers.VoyageListReply{Items: nil, Limit: 0, Offset: 0, Total: 0}
	goldenVoyageWire(t, "conv/ListReply_nil_items", toVoyageListReply(listNilH),
		`{"items":null,"limit":0,"offset":0,"total":0}`)

	aid := "01A"
	targetsH := handlers.VoyageTargetsReply{Targets: []handlers.VoyageTargetEntryDTO{{ApplyID: &aid, BatchIndex: 0, Status: "succeeded", TargetID: "web", TargetKind: "incarnation"}}, VoyageID: "1"}
	goldenVoyageWire(t, "conv/TargetsReply", toVoyageTargetsReply(targetsH),
		`{"targets":[{"apply_id":"01A","batch_index":0,"status":"succeeded","target_id":"web","target_kind":"incarnation"}],"voyage_id":"1"}`)
	targetsNilH := handlers.VoyageTargetsReply{Targets: nil, VoyageID: "1"}
	goldenVoyageWire(t, "conv/TargetsReply_nil_targets", toVoyageTargetsReply(targetsNilH),
		`{"targets":null,"voyage_id":"1"}`)
}
