package auditpg

import (
	"strings"
	"testing"
	"time"
)

// TestBuildAuditWhere_PayloadHerald — docker-free guard предиката истории
// доставок (S2): фильтр payload_herald должен материализоваться как
// `payload->>'herald' = $N` с значением в args. Ловит регресс, если имя
// JSONB-ключа или оператор извлечения изменится.
func TestBuildAuditWhere_PayloadHerald(t *testing.T) {
	where, args := buildAuditWhere(ListFilter{PayloadHerald: "ops-slack"})
	if !strings.Contains(where, "payload->>'herald' = $1") {
		t.Fatalf("where = %q, want payload->>'herald' predicate", where)
	}
	if len(args) != 1 || args[0] != "ops-slack" {
		t.Fatalf("args = %v, want [ops-slack]", args)
	}
}

// TestBuildAuditWhere_PayloadVoyage — docker-free guard предиката visibility
// Voyage detail (ADR-052 amend §k): фильтр payload_voyage материализуется как
// `payload->>'voyage_id' = $N` с значением в args (параметризованный плейсхолдер,
// НЕ конкатенация). Ловит регресс имени JSONB-ключа / оператора извлечения.
func TestBuildAuditWhere_PayloadVoyage(t *testing.T) {
	where, args := buildAuditWhere(ListFilter{PayloadVoyage: "voy-77"})
	if !strings.Contains(where, "payload->>'voyage_id' = $1") {
		t.Fatalf("where = %q, want payload->>'voyage_id' predicate", where)
	}
	if len(args) != 1 || args[0] != "voy-77" {
		t.Fatalf("args = %v, want [voy-77]", args)
	}
}

// TestBuildAuditWhere_VoyageDeliveryCombo — voyage-секция: correlation_id +
// multi-type herald.delivered/failed комбинируются через AND, плейсхолдеры
// позиционно согласованы.
func TestBuildAuditWhere_VoyageDeliveryCombo(t *testing.T) {
	where, args := buildAuditWhere(ListFilter{
		Types:         []string{"herald.delivered", "herald.failed"},
		CorrelationID: "voyage-1",
	})
	if !strings.Contains(where, "event_type IN ($1,$2)") {
		t.Errorf("where = %q, want event_type IN ($1,$2)", where)
	}
	if !strings.Contains(where, "correlation_id = $3") {
		t.Errorf("where = %q, want correlation_id = $3", where)
	}
	if !strings.Contains(where, " AND ") {
		t.Errorf("where = %q, want AND-join", where)
	}
	if len(args) != 3 {
		t.Fatalf("args = %v, want 3", args)
	}
}

// TestBuildAuditWhere_NoFilter — пустой фильтр → без WHERE (полная лента).
func TestBuildAuditWhere_NoFilter(t *testing.T) {
	where, args := buildAuditWhere(ListFilter{})
	if where != "" {
		t.Errorf("where = %q, want empty", where)
	}
	if len(args) != 0 {
		t.Errorf("args = %v, want empty", args)
	}
}

// TestBuildAuditWhere_AllFilters — все поля сразу: позиционные плейсхолдеры
// инкрементятся монотонно, payload-фильтр встаёт между correlation_id и
// временными границами.
func TestBuildAuditWhere_AllFilters(t *testing.T) {
	where, args := buildAuditWhere(ListFilter{
		Types:         []string{"herald.delivered"},
		Sources:       []string{"keeper_internal"},
		ArchonAID:     "archon-alice",
		CorrelationID: "voyage-1",
		PayloadHerald: "ops-slack",
		PayloadVoyage: "voy-77",
		StartedAfter:  time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC),
		StartedBefore: time.Date(2026, 6, 2, 0, 0, 0, 0, time.UTC),
	})
	for _, want := range []string{
		"event_type IN ($1)",
		"source IN ($2)",
		"archon_aid = $3",
		"correlation_id = $4",
		"payload->>'herald' = $5",
		"payload->>'voyage_id' = $6",
		"created_at >= $7",
		"created_at <= $8",
	} {
		if !strings.Contains(where, want) {
			t.Errorf("where = %q, missing %q", where, want)
		}
	}
	if len(args) != 8 {
		t.Fatalf("args len = %d, want 8", len(args))
	}
}
