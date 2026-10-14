package scenario

import (
	"encoding/json"
	"math"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/souls-guild/soul-stack/shared/audit"

	"google.golang.org/protobuf/types/known/structpb"

	"github.com/souls-guild/soul-stack/keeper/internal/render"
)

func planStruct(t *testing.T, m map[string]any) *structpb.Struct {
	t.Helper()
	st, err := structpb.NewStruct(m)
	if err != nil {
		t.Fatalf("structpb: %v", err)
	}
	return st
}

// The run plan carries ONE params map per task beside per-host RESULTS. Since
// NIM-908 a task can render different params on different hosts, and showing the
// first host's as if it were everyone's is the substitution this ticket removed
// from the run itself — reappearing in the view an operator opens to check what ran.
//
// A differing cell is MARKED; a host-invariant one stays visible, because blanking
// the map to cover one key would cost the view its purpose.
func TestMaskRunPlanParams_PerHostCellsAreMarked(t *testing.T) {
	rt := &render.RenderedTask{
		Name:   "echo",
		Module: "core.exec.run",
		Params: planStruct(t, map[string]any{"cmd": "echo a", "port": "6379"}),
		ParamsBySID: map[string]*structpb.Struct{
			"a.example.com": planStruct(t, map[string]any{"cmd": "echo a", "port": "6379"}),
			"b.example.com": planStruct(t, map[string]any{"cmd": "echo b", "port": "6379"}),
		},
	}

	var got map[string]any
	if err := json.Unmarshal(maskRunPlanParams(rt, nil), &got); err != nil {
		t.Fatalf("unmarshal the persisted plan params: %v", err)
	}
	if got["cmd"] != render.PerHostParamValue {
		t.Errorf("params.cmd = %v, want %q — the plan row shows one host's value as everyone's", got["cmd"], render.PerHostParamValue)
	}
	if got["port"] != "6379" {
		t.Errorf("params.port = %v, want %q — a host-invariant cell must stay visible", got["port"], "6379")
	}
}

// The golden path is untouched: a task with no per-host params persists exactly
// what it always did.
func TestMaskRunPlanParams_GoldenPathUnchanged(t *testing.T) {
	rt := &render.RenderedTask{
		Name:   "echo",
		Module: "core.exec.run",
		Params: planStruct(t, map[string]any{"cmd": "echo a"}),
	}
	var got map[string]any
	if err := json.Unmarshal(maskRunPlanParams(rt, nil), &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got["cmd"] != "echo a" {
		t.Errorf("params.cmd = %v, want %q", got["cmd"], "echo a")
	}
}

// A sealed cell is masked whether or not it varies: the seal marks PATHS, so
// per-host values under the same path are all covered. The per-host marker must not
// become a way for a secret to reach the row unmasked.
func TestMaskRunPlanParams_SealedPerHostCellStaysMasked(t *testing.T) {
	rt := &render.RenderedTask{
		Name:   "write",
		Module: "core.file.present",
		Params: planStruct(t, map[string]any{"content": "pw-for-a"}),
		ParamsBySID: map[string]*structpb.Struct{
			"a.example.com": planStruct(t, map[string]any{"content": "pw-for-a"}),
			"b.example.com": planStruct(t, map[string]any{"content": "pw-for-b"}),
		},
	}
	var got map[string]any
	if err := json.Unmarshal(maskRunPlanParams(rt, map[string]bool{"content": true}), &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	for _, leaked := range []string{"pw-for-a", "pw-for-b"} {
		if got["content"] == leaked {
			t.Fatalf("a sealed per-host cell reached the plan row in the clear: %v", got["content"])
		}
	}
	// ★ And it is MASKED, not merely replaced by the per-host marker. Asserting
	// "neither plaintext" would pass whether the seal ran or not, because the marker
	// alone satisfies it — so an ordering regression (mark after mask, or skip
	// masking for marked cells) would go unnoticed. The seal runs last and wins.
	if got["content"] != audit.MaskedValue {
		t.Fatalf("params.content = %v, want %q — the seal must win over the per-host marker", got["content"], audit.MaskedValue)
	}
}

func decodePlanJSON(t *testing.T, b []byte) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("unmarshal %s: %v", b, err)
	}
	return m
}

// NIM-985: what the marker hides is kept per host — every host the task was
// rendered for gets a row holding its value of each marked key and nothing else.
// A key one host lacks is marked too, and that host's row simply lacks it; a key
// the FIRST host lacks is marked as well, or the plan row would not show it at all.
func TestMaskRunPlanParamsByHost_RowsCarryOnlyTheDifferingKeys(t *testing.T) {
	rt := &render.RenderedTask{
		Name:   "echo",
		Module: "core.exec.run",
		Params: planStruct(t, map[string]any{"cmd": "echo a", "port": "6379", "extra": "x"}),
		ParamsBySID: map[string]*structpb.Struct{
			"a.example.com": planStruct(t, map[string]any{"cmd": "echo a", "port": "6379", "extra": "x"}),
			"b.example.com": planStruct(t, map[string]any{"cmd": "echo b", "port": "6379", "extra": "x"}),
			"c.example.com": planStruct(t, map[string]any{"cmd": "echo c", "port": "6379", "late": "only-c"}),
		},
	}
	common, byHost := maskRunPlanParamsByHost(rt, nil)
	got := decodePlanJSON(t, common)
	wantPlan := map[string]any{"cmd": render.PerHostParamValue, "extra": render.PerHostParamValue, "late": render.PerHostParamValue, "port": "6379"}
	if !reflect.DeepEqual(got, wantPlan) {
		t.Errorf("plan row = %v, want %v", got, wantPlan)
	}
	if len(byHost) != 3 {
		t.Fatalf("rows for %d hosts, want one per host the task was rendered for (3)", len(byHost))
	}
	want := map[string]map[string]any{
		"a.example.com": {"cmd": "echo a", "extra": "x"},
		"b.example.com": {"cmd": "echo b", "extra": "x"},
		"c.example.com": {"cmd": "echo c", "late": "only-c"},
	}
	for sid, w := range want {
		if row := decodePlanJSON(t, byHost[sid]); !reflect.DeepEqual(row, w) {
			t.Errorf("row %s = %v, want %v", sid, row, w)
		}
	}
}

// The golden path writes no rows, and neither does a task whose only difference
// is a transport key the plan never stores.
func TestMaskRunPlanParamsByHost_NoRowsWithoutAVisibleDifference(t *testing.T) {
	plain := &render.RenderedTask{Module: "core.exec.run", Params: planStruct(t, map[string]any{"cmd": "echo"})}
	if _, byHost := maskRunPlanParamsByHost(plain, nil); byHost != nil {
		t.Errorf("golden path wrote rows: %v", byHost)
	}
	rendered := &render.RenderedTask{
		Module: "core.file.rendered",
		Params: planStruct(t, map[string]any{"dest": "/etc/x", paramRenderContext: "a"}),
		ParamsBySID: map[string]*structpb.Struct{
			"a.example.com": planStruct(t, map[string]any{"dest": "/etc/x", paramRenderContext: "a"}),
			"b.example.com": planStruct(t, map[string]any{"dest": "/etc/x", paramRenderContext: "b"}),
		},
	}
	common, byHost := maskRunPlanParamsByHost(rendered, nil)
	if byHost != nil {
		t.Errorf("a transport-only difference wrote rows: %v", byHost)
	}
	if got := decodePlanJSON(t, common); !reflect.DeepEqual(got, map[string]any{"dest": "/etc/x"}) {
		t.Errorf("plan row = %v, want dest only, unmarked", got)
	}
}

// ★ GUARD (NIM-985, the NIM-811/823/853 class): a per-host secret never reaches a
// per-host row. Each host is masked with the run's seal BEFORE the comparison, so a
// sealed cell — top-level or nested in a key that differs for another reason — is
// one mask on every host, and a vault reference masks by content the same way.
// The sealed cells carry names the key-name regex does not know, so only the seal
// can mask them.
//
// Mutations: compare the raw per-host structs and mask only the plan row → the
// per-host values land in the rows; mask the hosts without the seal set → the
// sealed cells land in the rows.
func TestMaskRunPlanParamsByHost_SealedPerHostValueNeverReachesARow(t *testing.T) {
	host := func(n string) *structpb.Struct {
		return planStruct(t, map[string]any{
			"cmd":      "echo " + n,
			"greeting": "hello-" + n,
			"conn":     map[string]any{"host": n + ".db", "note": "note-" + n},
			"ref":      "vault:kv/" + n + "#value",
		})
	}
	rt := &render.RenderedTask{
		Name:        "connect",
		Module:      "core.exec.run",
		Params:      host("a"),
		ParamsBySID: map[string]*structpb.Struct{"a.example.com": host("a"), "b.example.com": host("b")},
	}
	common, byHost := maskRunPlanParamsByHost(rt, map[string]bool{"greeting": true, "conn.note": true})
	if len(byHost) != 2 {
		t.Fatalf("rows = %v, want one per host — cmd and conn.host differ", byHost)
	}
	leaks := []string{"hello-a", "hello-b", "note-a", "note-b", "vault:kv/"}
	for sid, row := range byHost {
		for _, leak := range leaks {
			if strings.Contains(string(row), leak) {
				t.Fatalf("row %s carries %q in the clear: %s", sid, leak, row)
			}
		}
		got := decodePlanJSON(t, row)
		if _, ok := got["greeting"]; ok {
			t.Errorf("row %s carries greeting, want it shared as one mask: %v", sid, got)
		}
		conn, _ := got["conn"].(map[string]any)
		if conn == nil || conn["note"] != audit.MaskedValue || conn["host"] == nil {
			t.Errorf("row %s conn = %v, want the host shown and the note masked", sid, got["conn"])
		}
	}
	for _, leak := range leaks {
		if strings.Contains(string(common), leak) {
			t.Fatalf("plan row carries %q in the clear: %s", leak, common)
		}
	}
	got := decodePlanJSON(t, common)
	if got["greeting"] != audit.MaskedValue || got["ref"] != audit.MaskedValue {
		t.Errorf("plan row = %v, want greeting and ref as one shared mask", got)
	}
}

// The regex last resort's alarm reports a PATH the seal missed: once per path per
// task, whichever hosts carried the cell — including one the first host lacks.
//
// Mutations: drop the once-per-path set → api_key fires 1 + N times; raise the
// alarm for the plan row only → db_password never fires.
func TestMaskRunPlanParamsByHost_RegexAlarmFiresOncePerCell(t *testing.T) {
	var fired []string
	prev := audit.DefaultSealHooks
	audit.SetSealHooks(audit.SealHooks{RegexFallback: func(path string) { fired = append(fired, path) }})
	t.Cleanup(func() { audit.SetSealHooks(prev) })

	host := func(n string) *structpb.Struct {
		m := map[string]any{"cmd": "echo " + n, "api_key": "k-" + n}
		if n != "a" {
			m["db_password"] = "k-db-" + n
		}
		return planStruct(t, m)
	}
	rt := &render.RenderedTask{
		Module:      "core.exec.run",
		Params:      host("a"),
		ParamsBySID: map[string]*structpb.Struct{"a.example.com": host("a"), "b.example.com": host("b"), "c.example.com": host("c")},
	}
	_, byHost := maskRunPlanParamsByHost(rt, nil)
	slices.Sort(fired)
	if !reflect.DeepEqual(fired, []string{"api_key", "db_password"}) {
		t.Errorf("alarm fired for %v, want once for api_key and once for db_password", fired)
	}
	for sid, row := range byHost {
		if strings.Contains(string(row), "k-") {
			t.Errorf("row %s carries the key by name in the clear: %s", sid, row)
		}
	}
}

// A host row that does not encode costs only the per-host rows, and the error
// names the host. Unreachable from a rendered struct (AsMap stringifies NaN and
// ±Inf), so this calls the encoder directly.
func TestPerHostPlanRows_UnencodableRowYieldsNoRows(t *testing.T) {
	rows, err := perHostPlanRows(map[string]map[string]any{
		"a.example.com": {"ratio": 1.5},
		"b.example.com": {"ratio": math.Inf(1)},
	}, map[string]bool{"ratio": true})
	if err == nil || rows != nil {
		t.Fatalf("rows = %v, err = %v; want no rows and an error naming the host", rows, err)
	}
	if !strings.Contains(err.Error(), "b.example.com") {
		t.Errorf("err = %v, want it to name b.example.com", err)
	}
}
