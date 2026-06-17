package oracle

import (
	"testing"

	keeperv1 "github.com/souls-guild/soul-stack/proto/gen/go/keeper/v1"
	"google.golang.org/protobuf/types/known/structpb"
)

func TestWhereEvaluator_Match(t *testing.T) {
	w, err := NewWhereEvaluator()
	if err != nil {
		t.Fatalf("NewWhereEvaluator: %v", err)
	}

	data := map[string]any{
		"severity": "critical",
		"path":     "/etc/passwd",
		"count":    int64(3),
	}

	cases := []struct {
		name string
		expr string
		want bool
	}{
		{"equality true", `event.data.severity == "critical"`, true},
		{"equality false", `event.data.severity == "info"`, false},
		{"numeric comparison", `event.data.count > 2`, true},
		{"string contains", `event.data.path.contains("passwd")`, true},
		{"compound", `event.data.severity == "critical" && event.data.count >= 3`, true},
		{"missing key → no-match (default-deny)", `event.data.absent == "x"`, false},
		{"non-bool result → no-match", `event.data.severity`, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := w.Eval(c.expr, data)
			if err != nil {
				t.Fatalf("Eval(%q): unexpected error %v", c.expr, err)
			}
			if got != c.want {
				t.Errorf("Eval(%q) = %v, want %v", c.expr, got, c.want)
			}
		})
	}
}

func TestWhereEvaluator_CompileError(t *testing.T) {
	w, err := NewWhereEvaluator()
	if err != nil {
		t.Fatalf("NewWhereEvaluator: %v", err)
	}
	// Синтаксически невалидное выражение → compile-ошибка (битый Decree).
	if _, err := w.Eval(`event.data.x ==`, map[string]any{}); err == nil {
		t.Error("ожидали compile-ошибку на невалидном where_cel")
	}
}

func TestWhereEvaluator_NilData(t *testing.T) {
	w, err := NewWhereEvaluator()
	if err != nil {
		t.Fatalf("NewWhereEvaluator: %v", err)
	}
	// nil-payload: обращение к ключу → no-such-key → no-match (default-deny),
	// без ошибки.
	got, err := w.Eval(`event.data.severity == "critical"`, nil)
	if err != nil {
		t.Fatalf("Eval с nil-data: %v", err)
	}
	if got {
		t.Error("при nil-payload предикат не должен срабатывать")
	}
}

// --- Typed PortentPayload CEL-access (V5-1, ADR-030 amendment 2026-05-26) ---

// mustStructPB — local helper.
func mustStructPB(t *testing.T, fields map[string]any) *structpb.Struct {
	t.Helper()
	s, err := structpb.NewStruct(fields)
	if err != nil {
		t.Fatalf("structpb.NewStruct: %v", err)
	}
	return s
}

// TestEvalEvent_TypedAccess — where-CEL читает typed payload через
// `event.<branch>.<field>` (file_changed / service_down / disk_full / ...).
// Это первый стиль where-CEL, который должен работать после V5-1.
func TestEvalEvent_TypedAccess(t *testing.T) {
	w, err := NewWhereEvaluator()
	if err != nil {
		t.Fatalf("NewWhereEvaluator: %v", err)
	}

	cases := []struct {
		name string
		evt  *keeperv1.PortentEvent
		expr string
		want bool
	}{
		{
			name: "file_changed.path startsWith",
			evt: &keeperv1.PortentEvent{
				BeaconName: "v",
				Payload: &keeperv1.PortentEvent_FileChanged{FileChanged: &keeperv1.FileChangedPortent{
					Path: "/etc/nginx.conf", Sha256: "abc",
				}},
			},
			expr: `event.file_changed.path.startsWith("/etc/")`,
			want: true,
		},
		{
			name: "service_down.active false",
			evt: &keeperv1.PortentEvent{
				BeaconName: "v",
				Payload: &keeperv1.PortentEvent_ServiceDown{ServiceDown: &keeperv1.ServiceDownPortent{
					Service: "nginx", Active: false, InitSystem: "systemd",
				}},
			},
			expr: `event.service_down.service == "nginx" && !event.service_down.active`,
			want: true,
		},
		{
			name: "disk_full threshold breach",
			evt: &keeperv1.PortentEvent{
				BeaconName: "v",
				Payload: &keeperv1.PortentEvent_DiskFull{DiskFull: &keeperv1.DiskFullPortent{
					Path: "/var", UsedPercent: 97.0, Threshold: 90.0,
				}},
			},
			expr: `event.disk_full.used_percent > event.disk_full.threshold`,
			want: true,
		},
		{
			name: "port_closed numeric port",
			evt: &keeperv1.PortentEvent{
				BeaconName: "v",
				Payload: &keeperv1.PortentEvent_PortClosed{PortClosed: &keeperv1.PortClosedPortent{
					Host: "127.0.0.1", Port: 5432,
				}},
			},
			expr: `event.port_closed.port == 5432`,
			want: true,
		},
		{
			name: "http_unhealthy 5xx",
			evt: &keeperv1.PortentEvent{
				BeaconName: "v",
				Payload: &keeperv1.PortentEvent_HttpUnhealthy{HttpUnhealthy: &keeperv1.HttpUnhealthyPortent{
					Url: "https://a/b", Status: 503,
				}},
			},
			expr: `event.http_unhealthy.status >= 500`,
			want: true,
		},
		{
			name: "process_absent.pattern match",
			evt: &keeperv1.PortentEvent{
				BeaconName: "v",
				Payload: &keeperv1.PortentEvent_ProcessAbsent{ProcessAbsent: &keeperv1.ProcessAbsentPortent{
					Pattern: "redis-server",
				}},
			},
			expr: `event.process_absent.pattern == "redis-server"`,
			want: true,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := w.EvalEvent(c.expr, c.evt)
			if err != nil {
				t.Fatalf("EvalEvent: %v", err)
			}
			if got != c.want {
				t.Errorf("EvalEvent(%q) = %v, want %v", c.expr, got, c.want)
			}
		})
	}
}

// TestEvalEvent_LegacyDataAccess — backward-compat: where-CEL `event.data.*`
// продолжает работать в hand-off-период, когда Soul шлёт ОБЕ ветки (data +
// typed) одновременно.
func TestEvalEvent_LegacyDataAccess(t *testing.T) {
	w, err := NewWhereEvaluator()
	if err != nil {
		t.Fatalf("NewWhereEvaluator: %v", err)
	}

	// Soul шлёт обе ветки: legacy data + typed file_changed.
	evt := &keeperv1.PortentEvent{
		BeaconName: "v",
		Data: mustStructPB(t, map[string]any{
			"path":   "/etc/passwd",
			"sha256": "abc",
		}),
		Payload: &keeperv1.PortentEvent_FileChanged{FileChanged: &keeperv1.FileChangedPortent{
			Path: "/etc/passwd", Sha256: "abc",
		}},
	}

	got, err := w.EvalEvent(`event.data.path == "/etc/passwd"`, evt)
	if err != nil {
		t.Fatalf("EvalEvent legacy: %v", err)
	}
	if !got {
		t.Error("legacy event.data.* должен матчить в hand-off-период")
	}

	got, err = w.EvalEvent(`event.file_changed.path == "/etc/passwd"`, evt)
	if err != nil {
		t.Fatalf("EvalEvent typed: %v", err)
	}
	if !got {
		t.Error("typed event.file_changed.* должен матчить одновременно с legacy")
	}
}

// TestEvalEvent_TypeMismatchFailSafe — CEL ожидает file_changed, прилетел
// service_down → отсутствующая ветка `event.file_changed.path` даёт no-such-key
// → cel runtime-error → false (default-deny, fail-safe).
func TestEvalEvent_TypeMismatchFailSafe(t *testing.T) {
	w, err := NewWhereEvaluator()
	if err != nil {
		t.Fatalf("NewWhereEvaluator: %v", err)
	}
	evt := &keeperv1.PortentEvent{
		BeaconName: "v",
		Payload: &keeperv1.PortentEvent_ServiceDown{ServiceDown: &keeperv1.ServiceDownPortent{
			Service: "nginx",
		}},
	}
	got, err := w.EvalEvent(`event.file_changed.path == "/x"`, evt)
	if err != nil {
		t.Fatalf("EvalEvent type-mismatch: %v", err)
	}
	if got {
		t.Error("type-mismatch должен давать false (default-deny)")
	}
}

// TestEvalEvent_NilEvent — nil-event → false (default-deny на пустой вход).
func TestEvalEvent_NilEvent(t *testing.T) {
	w, err := NewWhereEvaluator()
	if err != nil {
		t.Fatalf("NewWhereEvaluator: %v", err)
	}
	got, err := w.EvalEvent(`event.file_changed.path == "/x"`, nil)
	if err != nil {
		t.Fatalf("EvalEvent nil: %v", err)
	}
	if got {
		t.Error("nil-event должен давать false (default-deny)")
	}
}

// TestEvalEvent_CustomPayload — V5-2 plugin-beacon: where-CEL читает
// `event.custom.<field>` из произвольного Struct.
func TestEvalEvent_CustomPayload(t *testing.T) {
	w, err := NewWhereEvaluator()
	if err != nil {
		t.Fatalf("NewWhereEvaluator: %v", err)
	}
	custom := mustStructPB(t, map[string]any{
		"queue_depth": 1500,
		"backlog":     "growing",
	})
	evt := &keeperv1.PortentEvent{
		BeaconName: "soul_beacon.kafka_lag",
		Payload:    &keeperv1.PortentEvent_Custom{Custom: custom},
	}
	got, err := w.EvalEvent(`event.custom.queue_depth > 1000`, evt)
	if err != nil {
		t.Fatalf("EvalEvent custom: %v", err)
	}
	if !got {
		t.Error("event.custom.queue_depth > 1000 должен совпасть")
	}
}
