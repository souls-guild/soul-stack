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
// позиционно согласованы. correlation_id — case-insensitive substring (ILIKE).
func TestBuildAuditWhere_VoyageDeliveryCombo(t *testing.T) {
	where, args := buildAuditWhere(ListFilter{
		Types:         []string{"herald.delivered", "herald.failed"},
		CorrelationID: "voyage-1",
	})
	if !strings.Contains(where, "event_type IN ($1,$2)") {
		t.Errorf("where = %q, want event_type IN ($1,$2)", where)
	}
	if !strings.Contains(where, "correlation_id ILIKE $3") {
		t.Errorf("where = %q, want correlation_id ILIKE $3", where)
	}
	if !strings.Contains(where, " AND ") {
		t.Errorf("where = %q, want AND-join", where)
	}
	if len(args) != 3 || args[2] != "%voyage-1%" {
		t.Fatalf("args = %v, want correlation_id wrapped in %%…%%", args)
	}
}

// TestBuildAuditWhere_ArchonAID_ILIKE — фильтр archon_aid материализуется как
// case-insensitive substring: предикат ILIKE с bind-параметром `%val%` (НЕ
// строковая конкатенация в SQL — %-обёртка в значении args). Ловит регресс к
// exact `=`-семантике поиска.
func TestBuildAuditWhere_ArchonAID_ILIKE(t *testing.T) {
	where, args := buildAuditWhere(ListFilter{ArchonAID: "Alice"})
	if !strings.Contains(where, "archon_aid ILIKE $1") {
		t.Fatalf("where = %q, want archon_aid ILIKE predicate", where)
	}
	if len(args) != 1 || args[0] != "%Alice%" {
		t.Fatalf("args = %v, want [%%Alice%%]", args)
	}
}

// TestBuildAuditWhere_CorrelationID_ILIKE — фильтр correlation_id — ILIKE-
// substring с %-обёрткой в bind-значении (параметризованный плейсхолдер).
func TestBuildAuditWhere_CorrelationID_ILIKE(t *testing.T) {
	where, args := buildAuditWhere(ListFilter{CorrelationID: "abc"})
	if !strings.Contains(where, "correlation_id ILIKE $1") {
		t.Fatalf("where = %q, want correlation_id ILIKE predicate", where)
	}
	if len(args) != 1 || args[0] != "%abc%" {
		t.Fatalf("args = %v, want [%%abc%%]", args)
	}
}

// TestLikeContains_EscapesWildcards — LIKE-метасимволы (`\`/`%`/`_`) во вводе
// экранируются, чтобы ILIKE-поиск оставался литеральным (оператор ищет именно
// символ `%`/`_`, а не wildcard). Защита от «`%` находит всё».
func TestLikeContains_EscapesWildcards(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"plain", "%plain%"},
		{"50%", `%50\%%`},
		{"a_b", `%a\_b%`},
		{`c\d`, `%c\\d%`},
	} {
		if got := likeContains(tc.in); got != tc.want {
			t.Errorf("likeContains(%q) = %q, want %q", tc.in, got, tc.want)
		}
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
		"archon_aid ILIKE $3",
		"correlation_id ILIKE $4",
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
	if args[2] != "%archon-alice%" || args[3] != "%voyage-1%" {
		t.Errorf("args[2..3] = %v / %v, want %%-wrapped ILIKE values", args[2], args[3])
	}
}
