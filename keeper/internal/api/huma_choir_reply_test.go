// GOLDEN byte-exact wire-guard для huma-native reply-DTO CHOIR/VOICE-домена (handler-
// native T5d-2c). Для каждого reply-роута (choir create/list-item, voice add/list-item)
// маршалит наполненное native-значение и пинит байты против ЗАФИКСИРОВАННОЙ golden-
// строки (legacy-генерата удалён — пин против фиксированной формы, не против генерёного типа).
// Гарантирует, что wire-форма не уехала (date-time / `*` БЕЗ omitempty → null —
// категории A,D ADR-051). Покрыты обе указательные ветки (nil/non-nil). Мутация формы
// native-struct краснит case.
package api

import (
	"encoding/json"
	"testing"
	"time"
)

// goldenChoir маршалит native-значение и сверяет байты против ожидаемой golden-строки.
func goldenChoir(t *testing.T, name string, native any, want string) {
	t.Helper()
	got, err := json.Marshal(native)
	if err != nil {
		t.Fatalf("%s: marshal: %v", name, err)
	}
	if string(got) != want {
		t.Errorf("%s: GOLDEN wire-дрейф:\n got  = %s\n want = %s", name, got, want)
	}
}

func TestGoldenWire_ChoirReply(t *testing.T) {
	ts := time.Date(2026, 6, 14, 12, 34, 56, 789012345, time.UTC)
	ts2 := time.Date(2026, 6, 13, 1, 2, 3, 456789012, time.UTC)
	aid := "archon-alice"
	desc := "primary partition"
	role := "master"
	minN := 1
	maxN := 5
	pos := 0

	// --- Choir: created_by_aid/description/min_size/max_size — `*` БЕЗ omitempty → null ---
	goldenChoir(t, "Choir/full",
		Choir{ChoirName: "leaders", CreatedAt: ts, CreatedByAID: &aid, Description: &desc, IncarnationName: "redis-prod", MaxSize: &maxN, MinSize: &minN},
		`{"choir_name":"leaders","created_at":"2026-06-14T12:34:56.789012345Z","created_by_aid":"archon-alice","description":"primary partition","incarnation_name":"redis-prod","max_size":5,"min_size":1}`)
	goldenChoir(t, "Choir/nil_optionals",
		Choir{ChoirName: "leaders", CreatedAt: ts, CreatedByAID: nil, Description: nil, IncarnationName: "redis-prod", MaxSize: nil, MinSize: nil},
		`{"choir_name":"leaders","created_at":"2026-06-14T12:34:56.789012345Z","created_by_aid":null,"description":null,"incarnation_name":"redis-prod","max_size":null,"min_size":null}`)

	// --- Voice: added_by_aid/position/role — `*` БЕЗ omitempty → null ---
	goldenChoir(t, "Voice/full",
		Voice{AddedAt: ts2, AddedByAID: &aid, ChoirName: "leaders", IncarnationName: "redis-prod", Position: &pos, Role: &role, SID: "web1.eu"},
		`{"added_at":"2026-06-13T01:02:03.456789012Z","added_by_aid":"archon-alice","choir_name":"leaders","incarnation_name":"redis-prod","position":0,"role":"master","sid":"web1.eu"}`)
	goldenChoir(t, "Voice/nil_optionals",
		Voice{AddedAt: ts2, AddedByAID: nil, ChoirName: "leaders", IncarnationName: "redis-prod", Position: nil, Role: nil, SID: "web1.eu"},
		`{"added_at":"2026-06-13T01:02:03.456789012Z","added_by_aid":null,"choir_name":"leaders","incarnation_name":"redis-prod","position":null,"role":null,"sid":"web1.eu"}`)

	// --- ChoirListReply / VoiceListReply: items non-nil [] / nil (категория B) ---
	goldenChoir(t, "ChoirListReply/full",
		ChoirListReply{Items: []Choir{{ChoirName: "leaders", CreatedAt: ts, IncarnationName: "redis-prod"}}, Limit: 1, Offset: 0, Total: 1},
		`{"items":[{"choir_name":"leaders","created_at":"2026-06-14T12:34:56.789012345Z","created_by_aid":null,"description":null,"incarnation_name":"redis-prod","max_size":null,"min_size":null}],"limit":1,"offset":0,"total":1}`)
	goldenChoir(t, "ChoirListReply/empty",
		ChoirListReply{Items: []Choir{}, Limit: 0, Offset: 0, Total: 0},
		`{"items":[],"limit":0,"offset":0,"total":0}`)
	goldenChoir(t, "VoiceListReply/full",
		VoiceListReply{Items: []Voice{{AddedAt: ts2, ChoirName: "leaders", IncarnationName: "redis-prod", SID: "web1.eu"}}, Limit: 1, Offset: 0, Total: 1},
		`{"items":[{"added_at":"2026-06-13T01:02:03.456789012Z","added_by_aid":null,"choir_name":"leaders","incarnation_name":"redis-prod","position":null,"role":null,"sid":"web1.eu"}],"limit":1,"offset":0,"total":1}`)
	goldenChoir(t, "VoiceListReply/empty",
		VoiceListReply{Items: []Voice{}, Limit: 0, Offset: 0, Total: 0},
		`{"items":[],"limit":0,"offset":0,"total":0}`)
}
