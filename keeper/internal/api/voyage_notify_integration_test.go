//go:build integration

package api

// E2E на реальной PG/роутере: блок notify в POST /v1/voyages (ADR-052(g) N2).
// Атомарное создание ephemeral-Tiding в одной tx с Voyage + herald.read-guard +
// маппинг on→event_types. Самый важный guard — атомарность (отказ ⇒ ни Voyage,
// ни правил) и то, что pre-check (RBAC/existence) отрабатывает ДО persist.

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/souls-guild/soul-stack/keeper/internal/rbac/rbactest"
	"github.com/souls-guild/soul-stack/keeper/internal/soul"
)

// --- RBAC-конфиги для notify --------------------------------------------

// notifyFullRBAC — errand.run (запуск command) + herald.read (право ссылаться на
// канал). Архонт может и запустить прогон, и подписать его на уведомления.
func notifyFullRBAC(aid string) *rbactest.Config {
	return &rbactest.Config{
		Roles: []rbactest.Role{
			{Name: "notify-full", Operators: []string{aid}, Permissions: []string{
				"errand.run", "herald.read",
			}},
		},
	}
}

// notifyNoHeraldReadRBAC — errand.run БЕЗ herald.read: прогон запустить можно,
// подписать на канал — нет (403 на notify).
func notifyNoHeraldReadRBAC(aid string) *rbactest.Config {
	return &rbactest.Config{
		Roles: []rbactest.Role{
			{Name: "notify-noread", Operators: []string{aid}, Permissions: []string{
				"errand.run",
			}},
		},
	}
}

// --- helpers ------------------------------------------------------------

// truncateHeralds чистит heralds/tidings (truncateOperators их НЕ сносит —
// created_by_aid это ON DELETE SET NULL, не CASCADE). Вызывается ПОСЛЕ
// truncateOperators в каждом notify-тесте.
func truncateHeralds(t *testing.T) {
	t.Helper()
	if _, err := integrationPool.Exec(context.Background(),
		`TRUNCATE tidings, heralds RESTART IDENTITY CASCADE`); err != nil {
		t.Fatalf("truncate heralds/tidings: %v", err)
	}
}

// seedHerald вставляет webhook-Herald напрямую в БД (минуя API).
func seedHerald(t *testing.T, name, aid string) {
	t.Helper()
	cfg, _ := json.Marshal(map[string]any{"url": "https://hooks.example.com/" + name})
	if _, err := integrationPool.Exec(context.Background(),
		`INSERT INTO heralds (name, type, config, enabled, created_by_aid)
		 VALUES ($1, 'webhook', $2, true, $3)`, name, cfg, aid); err != nil {
		t.Fatalf("seedHerald(%s): %v", name, err)
	}
}

// ephemeralTidingRow — снимок строки tidings для assert-ов.
type ephemeralTidingRow struct {
	name         string
	herald       string
	eventTypes   []string
	onlyFailures bool
	onlyChanges  bool
	ephemeral    bool
	voyageID     *string
	annotations  string
	projection   []string
	createdByAID *string
}

// selectTidingsByVoyage читает все ephemeral-Tiding-и, привязанные к voyage_id.
func selectTidingsByVoyage(t *testing.T, voyageID string) []ephemeralTidingRow {
	t.Helper()
	rows, err := integrationPool.Query(context.Background(),
		`SELECT name, herald, event_types, only_failures, only_changes, ephemeral,
		        voyage_id, annotations::text, projection, created_by_aid
		 FROM tidings WHERE voyage_id = $1 ORDER BY name`, voyageID)
	if err != nil {
		t.Fatalf("selectTidingsByVoyage: %v", err)
	}
	defer rows.Close()
	var out []ephemeralTidingRow
	for rows.Next() {
		var r ephemeralTidingRow
		if err := rows.Scan(&r.name, &r.herald, &r.eventTypes, &r.onlyFailures, &r.onlyChanges,
			&r.ephemeral, &r.voyageID, &r.annotations, &r.projection, &r.createdByAID); err != nil {
			t.Fatalf("scan tiding: %v", err)
		}
		out = append(out, r)
	}
	return out
}

// countVoyages / countTidings — счётчики для atomicity-assert-ов.
func countVoyages(t *testing.T) int {
	t.Helper()
	var n int
	if err := integrationPool.QueryRow(context.Background(),
		`SELECT COUNT(*) FROM voyages`).Scan(&n); err != nil {
		t.Fatalf("countVoyages: %v", err)
	}
	return n
}

func countTidings(t *testing.T) int {
	t.Helper()
	var n int
	if err := integrationPool.QueryRow(context.Background(),
		`SELECT COUNT(*) FROM tidings`).Scan(&n); err != nil {
		t.Fatalf("countTidings: %v", err)
	}
	return n
}

// --- Tests --------------------------------------------------------------

// Атомарность (happy-path): создание command-Voyage с notify → ephemeral-Tiding
// в БД с правильными полями (voyage_id нового прогона, ephemeral=true, маппинг
// on→event_types, annotations/projection, created_by_aid инициатора).
func TestIntegration_VoyageNotify_CreatesEphemeralTiding_202(t *testing.T) {
	truncateOperators(t)
	truncateHeralds(t)
	seedOperator(t, "archon-n", "")
	seedHerald(t, "ops-webhook", "archon-n")
	seedSoulFull(t, "n-01.example.com", "agent", soul.StatusConnected, []string{"coven-n"}, "archon-n")

	base, stop := startServer(t, notifyFullRBAC("archon-n"))
	defer stop()
	tok := newValidTokenFor(t, "archon-n", []string{"notify-full"})

	body := `{
		"kind":"command","module":"core.cmd.shell",
		"target":{"sids":["n-01.example.com"]},
		"notify":[{
			"herald":"ops-webhook","on":["failed","partial"],
			"only_failures":true,
			"annotations":{"team":"ops","severity":"high"},
			"projection":["summary.succeeded"]
		}]
	}`
	code, respBody := postCommandVoyage(t, base, tok, body)
	if code != 202 {
		t.Fatalf("status = %d, want 202; body=%s", code, respBody)
	}
	voyageID := voyageIDFromReply(t, respBody)

	tidings := selectTidingsByVoyage(t, voyageID)
	if len(tidings) != 1 {
		t.Fatalf("ephemeral-Tiding-ов = %d, want 1", len(tidings))
	}
	got := tidings[0]
	if !got.ephemeral {
		t.Error("ephemeral = false, want true")
	}
	if got.voyageID == nil || *got.voyageID != voyageID {
		t.Errorf("voyage_id = %v, want %s", got.voyageID, voyageID)
	}
	if got.herald != "ops-webhook" {
		t.Errorf("herald = %q, want ops-webhook", got.herald)
	}
	// on=[failed,partial] + kind=command → command_run.{failed,partial_failed}.
	wantET := map[string]bool{"command_run.failed": true, "command_run.partial_failed": true}
	if len(got.eventTypes) != 2 {
		t.Fatalf("event_types = %v, want 2 (command_run.failed/partial_failed)", got.eventTypes)
	}
	for _, et := range got.eventTypes {
		if !wantET[et] {
			t.Errorf("неожиданный event_type %q (want command_run.failed/partial_failed)", et)
		}
	}
	if !got.onlyFailures {
		t.Error("only_failures = false, want true")
	}
	if got.annotations == "" || got.annotations == "{}" {
		t.Errorf("annotations пустой: %q", got.annotations)
	}
	if len(got.projection) != 1 || got.projection[0] != "summary.succeeded" {
		t.Errorf("projection = %v, want [summary.succeeded]", got.projection)
	}
	if got.createdByAID == nil || *got.createdByAID != "archon-n" {
		t.Errorf("created_by_aid = %v, want archon-n", got.createdByAID)
	}
}

// Маппинг default-on: пустой on → все три терминала по kind=command.
func TestIntegration_VoyageNotify_DefaultOnAllTerminals_202(t *testing.T) {
	truncateOperators(t)
	truncateHeralds(t)
	seedOperator(t, "archon-n", "")
	seedHerald(t, "ops-webhook", "archon-n")
	seedSoulFull(t, "n-01.example.com", "agent", soul.StatusConnected, []string{"coven-n"}, "archon-n")

	base, stop := startServer(t, notifyFullRBAC("archon-n"))
	defer stop()
	tok := newValidTokenFor(t, "archon-n", []string{"notify-full"})

	body := `{"kind":"command","module":"core.cmd.shell",
		"target":{"sids":["n-01.example.com"]},
		"notify":[{"herald":"ops-webhook"}]}`
	code, respBody := postCommandVoyage(t, base, tok, body)
	if code != 202 {
		t.Fatalf("status = %d, want 202; body=%s", code, respBody)
	}
	tidings := selectTidingsByVoyage(t, voyageIDFromReply(t, respBody))
	if len(tidings) != 1 {
		t.Fatalf("tidings = %d, want 1", len(tidings))
	}
	want := map[string]bool{
		"command_run.completed":      true,
		"command_run.failed":         true,
		"command_run.partial_failed": true,
	}
	if len(tidings[0].eventTypes) != 3 {
		t.Fatalf("event_types = %v, want 3 терминала", tidings[0].eventTypes)
	}
	for _, et := range tidings[0].eventTypes {
		if !want[et] {
			t.Errorf("неожиданный event_type %q", et)
		}
	}
}

// RBAC 403: errand.run есть, herald.read НЕТ → 403, И ни Voyage, ни Tiding в БД
// (guard ДО persist — атомарность отказа).
func TestIntegration_VoyageNotify_NoHeraldRead_403_NoSideEffect(t *testing.T) {
	truncateOperators(t)
	truncateHeralds(t)
	seedOperator(t, "archon-n", "")
	seedHerald(t, "ops-webhook", "archon-n")
	seedSoulFull(t, "n-01.example.com", "agent", soul.StatusConnected, []string{"coven-n"}, "archon-n")

	base, stop := startServer(t, notifyNoHeraldReadRBAC("archon-n"))
	defer stop()
	tok := newValidTokenFor(t, "archon-n", []string{"notify-noread"})

	body := `{"kind":"command","module":"core.cmd.shell",
		"target":{"sids":["n-01.example.com"]},
		"notify":[{"herald":"ops-webhook"}]}`
	code, respBody := postCommandVoyage(t, base, tok, body)
	if code != 403 {
		t.Fatalf("status = %d, want 403 (нет herald.read); body=%s", code, respBody)
	}
	// Атомарность отказа: guard сработал ДО persist — БД чистая.
	if v := countVoyages(t); v != 0 {
		t.Errorf("voyages = %d, want 0 (notify-403 не должен создать Voyage)", v)
	}
	if ti := countTidings(t); ti != 0 {
		t.Errorf("tidings = %d, want 0", ti)
	}
}

// 422: канал не существует → 422, И ни Voyage, ни Tiding (guard ДО persist).
func TestIntegration_VoyageNotify_HeraldNotFound_422_NoSideEffect(t *testing.T) {
	truncateOperators(t)
	truncateHeralds(t)
	seedOperator(t, "archon-n", "")
	seedSoulFull(t, "n-01.example.com", "agent", soul.StatusConnected, []string{"coven-n"}, "archon-n")

	base, stop := startServer(t, notifyFullRBAC("archon-n"))
	defer stop()
	tok := newValidTokenFor(t, "archon-n", []string{"notify-full"})

	body := `{"kind":"command","module":"core.cmd.shell",
		"target":{"sids":["n-01.example.com"]},
		"notify":[{"herald":"does-not-exist"}]}`
	code, respBody := postCommandVoyage(t, base, tok, body)
	if code != 422 {
		t.Fatalf("status = %d, want 422 (канал не существует); body=%s", code, respBody)
	}
	if v := countVoyages(t); v != 0 {
		t.Errorf("voyages = %d, want 0 (notify-422 не должен создать Voyage)", v)
	}
	if ti := countTidings(t); ti != 0 {
		t.Errorf("tidings = %d, want 0", ti)
	}
}

// 422: невалидное значение on (вне completed/failed/partial) → 422, БД чистая.
func TestIntegration_VoyageNotify_InvalidOn_422(t *testing.T) {
	truncateOperators(t)
	truncateHeralds(t)
	seedOperator(t, "archon-n", "")
	seedHerald(t, "ops-webhook", "archon-n")
	seedSoulFull(t, "n-01.example.com", "agent", soul.StatusConnected, []string{"coven-n"}, "archon-n")

	base, stop := startServer(t, notifyFullRBAC("archon-n"))
	defer stop()
	tok := newValidTokenFor(t, "archon-n", []string{"notify-full"})

	body := `{"kind":"command","module":"core.cmd.shell",
		"target":{"sids":["n-01.example.com"]},
		"notify":[{"herald":"ops-webhook","on":["started"]}]}`
	code, respBody := postCommandVoyage(t, base, tok, body)
	if code != 422 {
		t.Fatalf("status = %d, want 422 (невалидный on); body=%s", code, respBody)
	}
	if v := countVoyages(t); v != 0 {
		t.Errorf("voyages = %d, want 0", v)
	}
}

// Атомарность (положительная сторона all-or-nothing): несколько notify-элементов
// на ОДИН прогон ложатся все в одной tx с Voyage. Два notify → ровно два
// ephemeral-Tiding на один voyage_id, имена уникальны. Отрицательную сторону
// (отказ ⇒ ни Voyage, ни правил) закрывают тесты 403/422 выше: guard отрабатывает
// ДО persist, half-write невозможен; единственная tx в persist (voyage+targets+
// tidings) гарантирует, что in-tx-сбой любого INSERT-а откатил бы всё.
func TestIntegration_VoyageNotify_MultipleAllOrNothing_202(t *testing.T) {
	truncateOperators(t)
	truncateHeralds(t)
	seedOperator(t, "archon-n", "")
	seedHerald(t, "ops-a", "archon-n")
	seedHerald(t, "ops-b", "archon-n")
	seedSoulFull(t, "n-01.example.com", "agent", soul.StatusConnected, []string{"coven-n"}, "archon-n")

	base, stop := startServer(t, notifyFullRBAC("archon-n"))
	defer stop()
	tok := newValidTokenFor(t, "archon-n", []string{"notify-full"})

	body := `{"kind":"command","module":"core.cmd.shell",
		"target":{"sids":["n-01.example.com"]},
		"notify":[{"herald":"ops-a","on":["completed"]},{"herald":"ops-b","on":["failed"]}]}`
	code, respBody := postCommandVoyage(t, base, tok, body)
	if code != 202 {
		t.Fatalf("status = %d, want 202; body=%s", code, respBody)
	}
	voyageID := voyageIDFromReply(t, respBody)
	tidings := selectTidingsByVoyage(t, voyageID)
	if len(tidings) != 2 {
		t.Fatalf("ephemeral-Tiding-ов = %d, want 2 (оба notify в одной tx)", len(tidings))
	}
	// Имена уникальны (свежий ULID на правило), оба привязаны к одному voyage_id.
	if tidings[0].name == tidings[1].name {
		t.Errorf("имена ephemeral совпали: %q (нарушена уникальность)", tidings[0].name)
	}
	heralds := map[string]bool{tidings[0].herald: true, tidings[1].herald: true}
	if !heralds["ops-a"] || !heralds["ops-b"] {
		t.Errorf("heralds = %v, want {ops-a, ops-b}", heralds)
	}
}

// voyageIDFromReply вытаскивает voyage_id из 202-ответа Create.
func voyageIDFromReply(t *testing.T, body string) string {
	t.Helper()
	var r struct {
		VoyageID string `json:"voyage_id"`
	}
	if err := json.Unmarshal([]byte(body), &r); err != nil {
		t.Fatalf("unmarshal reply: %v; body=%s", err, body)
	}
	if r.VoyageID == "" {
		t.Fatalf("voyage_id пустой в ответе: %s", body)
	}
	return r.VoyageID
}
