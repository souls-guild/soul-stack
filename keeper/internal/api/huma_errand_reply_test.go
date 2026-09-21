// GOLDEN byte-exact wire-guard for the NATIVE wire-DTO of the ERRAND domain (handler-native
// T5d-2c-full). The errand read-DTO no longer depend on the legacy generator (0 legacy generator in errand files),
// so golden compares the native JSON values against a PINNED reference string (not against a
// legacy-generator value, as the shared goldenWire did for still-oapi domains). This pins the exact
// wire bytes: a shape mutation (drop omitempty / change a json tag / a field type / the field ORDER
// under oapi byte-order) reddens the corresponding case. Covered: element ErrandResult
// (full + nil omitempty branches), envelope ErrandListReply (non-nil/empty/nil items) and
// the 202 body ErrandAccepted (errand-get running). errand-get terminal/running is serialized on the
// wire by the register-func from the native projection — the same native types pinned here.
package api

import (
	"encoding/json"
	"testing"
	"time"
)

// goldenErrandWire compares json.Marshal(native) byte-for-byte against a pinned reference.
// The PILOT form of golden for handler-native domains (without a legacy-generator reference).
func goldenErrandWire(t *testing.T, name string, native any, want string) {
	t.Helper()
	got, err := json.Marshal(native)
	if err != nil {
		t.Fatalf("%s: marshal native: %v", name, err)
	}
	if string(got) != want {
		t.Errorf("%s: WIRE DRIFT\n got  = %s\n want = %s", name, got, want)
	}
}

func TestGoldenWire_ErrandReply(t *testing.T) {
	ts := time.Date(2026, 6, 14, 12, 34, 56, 789012345, time.UTC)
	ts2 := time.Date(2026, 6, 14, 12, 35, 0, 0, time.UTC)
	dur := int64(1234)
	emsg := "boom"
	exit := int32(1)
	out := map[string]interface{}{"k": "v"}
	so := "out"
	se := "err"
	yes := true

	// --- ErrandResult: full (all optionals set) — ★ FIELD-ORDER under oapi byte-order ---
	goldenErrandWire(t, "ErrandResult/full",
		ErrandResult{DurationMs: &dur, ErrandID: "e1", ErrorMessage: &emsg, ExitCode: &exit, FinishedAt: &ts2, Module: "core.exec", Output: &out, SID: "h1.test", StartedAt: ts, StartedByAID: "archon-alice", Status: ErrandResultStatusFailed, Stderr: &se, StderrTruncated: &yes, Stdout: &so, StdoutTruncated: &yes},
		`{"duration_ms":1234,"errand_id":"e1","error_message":"boom","exit_code":1,"finished_at":"2026-06-14T12:35:00Z","module":"core.exec","output":{"k":"v"},"sid":"h1.test","started_at":"2026-06-14T12:34:56.789012345Z","started_by_aid":"archon-alice","status":"failed","stderr":"err","stderr_truncated":true,"stdout":"out","stdout_truncated":true}`)

	// --- ErrandResult: running, all optionals nil (omitempty omits them) ---
	goldenErrandWire(t, "ErrandResult/running_nil_optionals",
		ErrandResult{ErrandID: "e2", Module: "core.exec", SID: "h1.test", StartedAt: ts, StartedByAID: "archon-alice", Status: ErrandResultStatusRunning},
		`{"errand_id":"e2","module":"core.exec","sid":"h1.test","started_at":"2026-06-14T12:34:56.789012345Z","started_by_aid":"archon-alice","status":"running"}`)

	// --- ErrandListReply: non-nil / empty / nil items ---
	goldenErrandWire(t, "ErrandListReply/full",
		ErrandListReply{Items: []ErrandResult{{ErrandID: "e1", Module: "core.exec", SID: "h1.test", StartedAt: ts, StartedByAID: "archon-alice", Status: ErrandResultStatusSuccess}}, Limit: 50, Offset: 0, Total: 1},
		`{"items":[{"errand_id":"e1","module":"core.exec","sid":"h1.test","started_at":"2026-06-14T12:34:56.789012345Z","started_by_aid":"archon-alice","status":"success"}],"limit":50,"offset":0,"total":1}`)
	goldenErrandWire(t, "ErrandListReply/empty",
		ErrandListReply{Items: []ErrandResult{}, Limit: 50, Offset: 0, Total: 0},
		`{"items":[],"limit":50,"offset":0,"total":0}`)
	goldenErrandWire(t, "ErrandListReply/nil_items",
		ErrandListReply{Items: nil, Limit: 50, Offset: 0, Total: 0},
		`{"items":null,"limit":50,"offset":0,"total":0}`)

	// --- ErrandAccepted: 202 body errand-get running (errand_id + status) ---
	goldenErrandWire(t, "ErrandAccepted",
		ErrandAccepted{ErrandID: "01J0000000000000000000000", Status: "running"},
		`{"errand_id":"01J0000000000000000000000","status":"running"}`)
}
