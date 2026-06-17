// GOLDEN byte-exact wire-guard для NATIVE wire-DTO AUDIT-ENDPOINT-домена (handler-native T5d).
// audit-endpoint больше НЕ зависит от legacy-генерата — golden сверяет json native-значения с
// ЗАФИКСИРОВАННОЙ строкой-эталоном (pinned). Покрыты обе ветки archon_aid/correlation_id
// (nil/non-nil), items non-nil [] и source enum-тип. TestGoldenWire_AuditProjection сверяет
// byte-exact проекции доменной handlers.AuditListPage → native.
package api

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/souls-guild/soul-stack/keeper/internal/api/handlers"
)

func goldenAuditWire(t *testing.T, name string, native any, want string) {
	t.Helper()
	got, err := json.Marshal(native)
	if err != nil {
		t.Fatalf("%s: marshal native: %v", name, err)
	}
	if string(got) != want {
		t.Errorf("%s: WIRE DRIFT\n got  = %s\n want = %s", name, got, want)
	}
}

func TestGoldenWire_AuditEventReply(t *testing.T) {
	ts := time.Date(2026, 6, 14, 12, 34, 56, 0, time.UTC) // секундная точность (parity read-path)
	aid := "archon-alice"
	corr := "01J0CORRELID"
	payload := map[string]interface{}{"role": "operator", "permission": "incarnation.run"}

	// --- AuditEvent: archon_aid/correlation_id наполнены ---
	goldenAuditWire(t, "AuditEvent/full",
		AuditEvent{ArchonAID: &aid, CorrelationID: &corr, CreatedAt: ts, ID: "01J0AUDITULID", Payload: payload, Source: AuditEventSourceAPI, Type: "role.create"},
		`{"archon_aid":"archon-alice","correlation_id":"01J0CORRELID","created_at":"2026-06-14T12:34:56Z","id":"01J0AUDITULID","payload":{"permission":"incarnation.run","role":"operator"},"source":"api","type":"role.create"}`)
	// archon_aid/correlation_id nil → ключи опущены (omitempty); payload пустой объект.
	goldenAuditWire(t, "AuditEvent/nil_optionals",
		AuditEvent{ArchonAID: nil, CorrelationID: nil, CreatedAt: ts, ID: "01J0AUDITULID", Payload: map[string]interface{}{}, Source: AuditEventSourceSoulGRPC, Type: "soul.applied"},
		`{"created_at":"2026-06-14T12:34:56Z","id":"01J0AUDITULID","payload":{},"source":"soul_grpc","type":"soul.applied"}`)

	// --- AuditEventListReply (envelope как top-level reply-DTO): items non-nil + offset/limit/total ---
	evN := AuditEvent{ArchonAID: &aid, CorrelationID: &corr, CreatedAt: ts, ID: "01J0AUDITULID", Payload: payload, Source: AuditEventSourceAPI, Type: "role.create"}
	goldenAuditWire(t, "AuditEventListReply/full",
		AuditEventListReply{Items: []AuditEvent{evN}, Limit: 50, Offset: 0, Total: 1},
		`{"items":[{"archon_aid":"archon-alice","correlation_id":"01J0CORRELID","created_at":"2026-06-14T12:34:56Z","id":"01J0AUDITULID","payload":{"permission":"incarnation.run","role":"operator"},"source":"api","type":"role.create"}],"limit":50,"offset":0,"total":1}`)
	// items пустой [] (ListTyped даёт non-nil []) → byte-exact `[]`, не null.
	goldenAuditWire(t, "AuditEventListReply/empty_items",
		AuditEventListReply{Items: []AuditEvent{}, Limit: 50, Offset: 100, Total: 0},
		`{"items":[],"limit":50,"offset":100,"total":0}`)
}

// TestGoldenWire_AuditProjection сверяет, что проекция доменной handlers.AuditListPage →
// native (newAuditEventListReply) даёт байт-в-байт зафиксированный wire.
func TestGoldenWire_AuditProjection(t *testing.T) {
	ts := time.Date(2026, 6, 14, 12, 0, 0, 0, time.UTC)
	aid := "archon-bob"
	m := map[string]interface{}{"k": "v"}

	page := handlers.AuditListPage{
		Items:  []handlers.AuditEventView{{ArchonAID: &aid, CorrelationID: nil, CreatedAt: ts, ID: "id", Payload: m, Source: "mcp", Type: "incarnation.run"}},
		Limit:  50,
		Offset: 0,
		Total:  1,
	}
	goldenAuditWire(t, "proj/AuditEventListReply", newAuditEventListReply(page),
		`{"items":[{"archon_aid":"archon-bob","created_at":"2026-06-14T12:00:00Z","id":"id","payload":{"k":"v"},"source":"mcp","type":"incarnation.run"}],"limit":50,"offset":0,"total":1}`)
}
