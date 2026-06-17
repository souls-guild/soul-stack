// GOLDEN byte-exact wire-guard для huma-native reply-DTO HERALD-домена (handler-native
// T5d-2c). Для КАЖДОГО reply-роута маршалит наполненное native-значение и пинит байты
// против ЗАФИКСИРОВАННОЙ golden-строки (legacy-генерата удалён — пин против фиксированной формы,
// не против генерёного типа). Гарантирует, что wire-форма native reply-DTO не уехала
// (date-time / []-vs-null / omitempty / nullable — категории A-D ADR-051). Покрыты обе
// указательные ветки (nil и non-nil). ENVELOPE сверяется ЯВНО: herald/tiding list —
// прямой named-struct, на wire участвует native-envelope. Мутация формы native-struct
// (убрать omitempty / сменить тег / тип) краснит case.
package api

import (
	"encoding/json"
	"testing"
	"time"
)

// goldenHerald маршалит native-значение и сверяет байты против ожидаемой golden-строки.
func goldenHerald(t *testing.T, name string, native any, want string) {
	t.Helper()
	got, err := json.Marshal(native)
	if err != nil {
		t.Fatalf("%s: marshal: %v", name, err)
	}
	if string(got) != want {
		t.Errorf("%s: GOLDEN wire-дрейф:\n got  = %s\n want = %s", name, got, want)
	}
}

func TestGoldenWire_HeraldReply(t *testing.T) {
	ts := time.Date(2026, 6, 14, 12, 34, 56, 789012345, time.UTC)
	aid := "archon-alice"
	ref := "vault:secret/hook"
	cfg := map[string]interface{}{"url": "https://hook.test/notify"}

	// --- Herald: full (created_by_aid/secret_ref set) и nil-ветки omitempty ---
	goldenHerald(t, "Herald/full",
		Herald{Config: cfg, CreatedAt: ts, CreatedByAID: &aid, Enabled: true, Name: "ops", SecretRef: &ref, Type: HeraldTypeWebhook, UpdatedAt: ts},
		`{"config":{"url":"https://hook.test/notify"},"created_at":"2026-06-14T12:34:56.789012345Z","created_by_aid":"archon-alice","enabled":true,"name":"ops","secret_ref":"vault:secret/hook","type":"webhook","updated_at":"2026-06-14T12:34:56.789012345Z"}`)
	goldenHerald(t, "Herald/nil_optionals",
		Herald{Config: cfg, CreatedAt: ts, CreatedByAID: nil, Enabled: false, Name: "ops", SecretRef: nil, Type: HeraldTypeWebhook, UpdatedAt: ts},
		`{"config":{"url":"https://hook.test/notify"},"created_at":"2026-06-14T12:34:56.789012345Z","enabled":false,"name":"ops","type":"webhook","updated_at":"2026-06-14T12:34:56.789012345Z"}`)

	// --- HeraldListReply: non-nil items, пустой items ([]), nil items (null) ---
	goldenHerald(t, "HeraldListReply/full",
		HeraldListReply{Items: []Herald{{Config: cfg, CreatedAt: ts, Enabled: true, Name: "ops", Type: HeraldTypeWebhook, UpdatedAt: ts}}, Limit: 50, Offset: 0, Total: 1},
		`{"items":[{"config":{"url":"https://hook.test/notify"},"created_at":"2026-06-14T12:34:56.789012345Z","enabled":true,"name":"ops","type":"webhook","updated_at":"2026-06-14T12:34:56.789012345Z"}],"limit":50,"offset":0,"total":1}`)
	goldenHerald(t, "HeraldListReply/empty",
		HeraldListReply{Items: []Herald{}, Limit: 50, Offset: 0, Total: 0},
		`{"items":[],"limit":50,"offset":0,"total":0}`)
	goldenHerald(t, "HeraldListReply/nil_items",
		HeraldListReply{Items: nil, Limit: 50, Offset: 0, Total: 0},
		`{"items":null,"limit":50,"offset":0,"total":0}`)

	// --- Tiding: full (все опц. указатели набиты) и nil-ветки ---
	ann := map[string]interface{}{"env": "prod"}
	proj := []string{"summary.succeeded"}
	yes := true
	cad := "nightly"
	inc := "redis-prod"
	task := "restart"
	vid := "01J0VOYAGEULID"
	goldenHerald(t, "Tiding/full",
		Tiding{Annotations: &ann, Cadence: &cad, CreatedAt: ts, CreatedByAID: &aid, Enabled: true, Ephemeral: &yes, EventTypes: []string{"scenario_run.*"}, Herald: "ops", Incarnation: &inc, Name: "on-fail", OnlyChanges: true, OnlyFailures: true, Projection: &proj, Task: &task, UpdatedAt: ts, VoyageID: &vid},
		`{"annotations":{"env":"prod"},"cadence":"nightly","created_at":"2026-06-14T12:34:56.789012345Z","created_by_aid":"archon-alice","enabled":true,"ephemeral":true,"event_types":["scenario_run.*"],"herald":"ops","incarnation":"redis-prod","name":"on-fail","only_changes":true,"only_failures":true,"projection":["summary.succeeded"],"task":"restart","updated_at":"2026-06-14T12:34:56.789012345Z","voyage_id":"01J0VOYAGEULID"}`)
	goldenHerald(t, "Tiding/nil_optionals",
		Tiding{CreatedAt: ts, Enabled: false, EventTypes: []string{"voyage.*"}, Herald: "ops", Name: "on-fail", OnlyChanges: false, OnlyFailures: false, UpdatedAt: ts},
		`{"created_at":"2026-06-14T12:34:56.789012345Z","enabled":false,"event_types":["voyage.*"],"herald":"ops","name":"on-fail","only_changes":false,"only_failures":false,"updated_at":"2026-06-14T12:34:56.789012345Z"}`)

	// --- TidingListReply: non-nil / nil items ---
	goldenHerald(t, "TidingListReply/full",
		TidingListReply{Items: []Tiding{{CreatedAt: ts, Enabled: true, EventTypes: []string{"scenario_run.*"}, Herald: "ops", Name: "on-fail", UpdatedAt: ts}}, Limit: 50, Offset: 0, Total: 1},
		`{"items":[{"created_at":"2026-06-14T12:34:56.789012345Z","enabled":true,"event_types":["scenario_run.*"],"herald":"ops","name":"on-fail","only_changes":false,"only_failures":false,"updated_at":"2026-06-14T12:34:56.789012345Z"}],"limit":50,"offset":0,"total":1}`)
	goldenHerald(t, "TidingListReply/nil_items",
		TidingListReply{Items: nil, Limit: 50, Offset: 0, Total: 0},
		`{"items":null,"limit":50,"offset":0,"total":0}`)
}
