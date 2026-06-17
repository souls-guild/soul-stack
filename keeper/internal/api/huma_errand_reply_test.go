// GOLDEN byte-exact wire-guard для NATIVE wire-DTO ERRAND-домена (handler-native
// T5d-2c-full). errand-read-DTO больше НЕ зависят от legacy-генерата (0 legacy-генерата в errand-файлах),
// поэтому golden сверяет json native-значения с ЗАФИКСИРОВАННОЙ строкой-эталоном (а не с
// legacy-генерата-значением, как делал общий goldenWire для ещё-oapi-доменов). Это пиннит точные
// wire-байты: мутация формы (убрать omitempty / сменить json-тег / тип поля / ПОРЯДОК
// полей под oapi byte-order) краснит соответствующий case. Покрыты element ErrandResult
// (full + nil-ветки omitempty), envelope ErrandListReply (non-nil/empty/nil items) и
// 202-тело ErrandAccepted (errand-get running). errand-get терминал/running на wire
// сериализует register-func из native-проекции — те же native-типы, что здесь пиннятся.
package api

import (
	"encoding/json"
	"testing"
	"time"
)

// goldenErrandWire сверяет json.Marshal(native) байт-в-байт с зафиксированным эталоном.
// PILOT-форма golden для handler-native-доменов (без legacy-генерата-эталона).
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

	// --- ErrandResult: full (все опц. набиты) — ★ FIELD-ORDER под oapi byte-order ---
	goldenErrandWire(t, "ErrandResult/full",
		ErrandResult{DurationMs: &dur, ErrandID: "e1", ErrorMessage: &emsg, ExitCode: &exit, FinishedAt: &ts2, Module: "core.exec", Output: &out, SID: "h1.test", StartedAt: ts, StartedByAID: "archon-alice", Status: ErrandResultStatusFailed, Stderr: &se, StderrTruncated: &yes, Stdout: &so, StdoutTruncated: &yes},
		`{"duration_ms":1234,"errand_id":"e1","error_message":"boom","exit_code":1,"finished_at":"2026-06-14T12:35:00Z","module":"core.exec","output":{"k":"v"},"sid":"h1.test","started_at":"2026-06-14T12:34:56.789012345Z","started_by_aid":"archon-alice","status":"failed","stderr":"err","stderr_truncated":true,"stdout":"out","stdout_truncated":true}`)

	// --- ErrandResult: running, все опц. — nil (omitempty опускает) ---
	goldenErrandWire(t, "ErrandResult/running_nil_optionals",
		ErrandResult{ErrandID: "e2", Module: "core.exec", SID: "h1.test", StartedAt: ts, StartedByAID: "archon-alice", Status: ErrandResultStatusRunning},
		`{"errand_id":"e2","module":"core.exec","sid":"h1.test","started_at":"2026-06-14T12:34:56.789012345Z","started_by_aid":"archon-alice","status":"running"}`)

	// --- ErrandListReply: non-nil / пустой / nil items ---
	goldenErrandWire(t, "ErrandListReply/full",
		ErrandListReply{Items: []ErrandResult{{ErrandID: "e1", Module: "core.exec", SID: "h1.test", StartedAt: ts, StartedByAID: "archon-alice", Status: ErrandResultStatusSuccess}}, Limit: 50, Offset: 0, Total: 1},
		`{"items":[{"errand_id":"e1","module":"core.exec","sid":"h1.test","started_at":"2026-06-14T12:34:56.789012345Z","started_by_aid":"archon-alice","status":"success"}],"limit":50,"offset":0,"total":1}`)
	goldenErrandWire(t, "ErrandListReply/empty",
		ErrandListReply{Items: []ErrandResult{}, Limit: 50, Offset: 0, Total: 0},
		`{"items":[],"limit":50,"offset":0,"total":0}`)
	goldenErrandWire(t, "ErrandListReply/nil_items",
		ErrandListReply{Items: nil, Limit: 50, Offset: 0, Total: 0},
		`{"items":null,"limit":50,"offset":0,"total":0}`)

	// --- ErrandAccepted: 202-тело errand-get running (errand_id + status) ---
	goldenErrandWire(t, "ErrandAccepted",
		ErrandAccepted{ErrandID: "01J0000000000000000000000", Status: "running"},
		`{"errand_id":"01J0000000000000000000000","status":"running"}`)
}
