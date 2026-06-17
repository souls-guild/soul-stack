//go:build integration

package api

// E2E на реальной PG/роутере: ADR-047 S4 — command-путь Voyage (`errand.run`)
// пересекает резолвнутый target с Purview Архонта (target ∩ Purview). Гибрид-
// семантика: явный чужой SID → 403; широкий target → урезание; пустое
// пересечение → 422; Unrestricted (cluster-admin) → полный резолв (backcompat).
//
// Закрывает security-leak: ранее command-таргет резолвился cluster-wide без
// scope, scoped-Архонт мог запустить command на чужом coven.

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/souls-guild/soul-stack/keeper/internal/rbac/rbactest"
	"github.com/souls-guild/soul-stack/keeper/internal/soul"
)

// errandRunScopeRBAC — роль со scoped errand.run (`on coven=<coven>`): Архонт
// может запускать command-Voyage только на хостах своего coven. Scope для S4-
// пересечения берётся из селектора самого права errand.run.
func errandRunScopeRBAC(aid, coven string) *rbactest.Config {
	return &rbactest.Config{
		Roles: []rbactest.Role{
			{Name: "cmd-ops", Operators: []string{aid}, Permissions: []string{
				"errand.run on coven=" + coven,
			}},
		},
	}
}

// errandRunRegexScopeRBAC — scoped errand.run на regex-измерении (`on regex=<pat>`):
// видимость command-таргета = regexMatch по SID ([soulpurview.CompiledScope.Visible]).
func errandRunRegexScopeRBAC(aid, pattern string) *rbactest.Config {
	return &rbactest.Config{
		Roles: []rbactest.Role{
			{Name: "cmd-regex-ops", Operators: []string{aid}, Permissions: []string{
				"errand.run on regex='" + pattern + "'",
			}},
		},
	}
}

// errandRunSoulprintScopeRBAC — scoped errand.run с coven + soulprint-измерением
// (S3b-2b отложено → Purview.Partial: под-показ, coven работает, soulprint — нет).
func errandRunSoulprintScopeRBAC(aid, coven, soulprintExpr string) *rbactest.Config {
	return &rbactest.Config{
		Roles: []rbactest.Role{
			{Name: "cmd-sp-ops", Operators: []string{aid}, Permissions: []string{
				"errand.run on coven=" + coven,
				"errand.run on soulprint='" + soulprintExpr + "'",
			}},
		},
	}
}

// errandRunUnrestrictedRBAC — bare errand.run (без default_scope) →
// Purview.Unrestricted (cluster-admin backcompat).
func errandRunUnrestrictedRBAC(aid string) *rbactest.Config {
	return &rbactest.Config{
		Roles: []rbactest.Role{
			{Name: "cmd-admin", Operators: []string{aid}, Permissions: []string{
				"errand.run",
			}},
		},
	}
}

// postCommandVoyage — POST /v1/voyages kind=command; возвращает статус + тело.
func postCommandVoyage(t *testing.T, base, tok, body string) (int, string) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost, base+"/v1/voyages", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST /v1/voyages: %v", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(raw)
}

// Guard #1: scoped errand.run on coven=A, target coven=B → 422 voyage_empty_target
// (пересечение пусто, чужой coven не запустился).
func TestIntegration_Voyage_CommandScope_TargetForeignCoven_422(t *testing.T) {
	truncateOperators(t)
	seedOperator(t, "archon-a", "")
	seedSoulFull(t, "a-01.example.com", "agent", soul.StatusConnected, []string{"coven-a"}, "archon-a")
	seedSoulFull(t, "b-01.example.com", "agent", soul.StatusConnected, []string{"coven-b"}, "archon-a")

	base, stop := startServer(t, errandRunScopeRBAC("archon-a", "coven-a"))
	defer stop()
	tok := newValidTokenFor(t, "archon-a", []string{"cmd-ops"})

	code, body := postCommandVoyage(t, base, tok,
		`{"kind":"command","module":"core.cmd.shell","target":{"coven":["coven-b"]}}`)
	if code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422 (чужой coven урезан в ноль); body=%s", code, body)
	}
	if !strings.Contains(body, "voyage_empty_target") {
		t.Errorf("detail не содержит voyage_empty_target: %s", body)
	}
}

// Guard #2: scoped coven=A, явный sids=[host-в-B] → 403 (anti-escalation: явное
// указание чужого хоста = попытка эскалации, не молчаливое урезание).
func TestIntegration_Voyage_CommandScope_ExplicitForeignSID_403(t *testing.T) {
	truncateOperators(t)
	seedOperator(t, "archon-a", "")
	seedSoulFull(t, "a-01.example.com", "agent", soul.StatusConnected, []string{"coven-a"}, "archon-a")
	seedSoulFull(t, "b-01.example.com", "agent", soul.StatusConnected, []string{"coven-b"}, "archon-a")

	base, stop := startServer(t, errandRunScopeRBAC("archon-a", "coven-a"))
	defer stop()
	tok := newValidTokenFor(t, "archon-a", []string{"cmd-ops"})

	code, body := postCommandVoyage(t, base, tok,
		`{"kind":"command","module":"core.cmd.shell","target":{"sids":["b-01.example.com"]}}`)
	if code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 (явный чужой SID); body=%s", code, body)
	}
}

// Guard #3: scoped coven=A, target coven=A, хост с covens=[A,B] → запуск OK
// (виден по A; multi-membership не мешает).
func TestIntegration_Voyage_CommandScope_MultiCovenHostVisible_202(t *testing.T) {
	truncateOperators(t)
	seedOperator(t, "archon-a", "")
	// Хост в обоих covens A и B. Архонт scoped на A → виден.
	seedSoulFull(t, "ab-01.example.com", "agent", soul.StatusConnected, []string{"coven-a", "coven-b"}, "archon-a")

	base, stop := startServer(t, errandRunScopeRBAC("archon-a", "coven-a"))
	defer stop()
	tok := newValidTokenFor(t, "archon-a", []string{"cmd-ops"})

	code, body := postCommandVoyage(t, base, tok,
		`{"kind":"command","module":"core.cmd.shell","target":{"coven":["coven-a"]}}`)
	if code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202 (хост виден по coven-a); body=%s", code, body)
	}
	if scope := scopeSizeFromReply(t, body); scope != 1 {
		t.Errorf("scope_size = %d, want 1", scope)
	}
}

// Guard #4: широкий target coven=[shared] (3 хоста, смешанная видимость), scope
// coven=A → резолвится только подмножество, видимое по A (урезание, НЕ весь
// shared). Хосты shared+A видимы; хост только-shared урезан.
func TestIntegration_Voyage_CommandScope_WideTargetTrimmed_202(t *testing.T) {
	truncateOperators(t)
	seedOperator(t, "archon-a", "")
	// Все три в shared; первые два ещё и в coven-a (видимы scope=A), третий — нет.
	seedSoulFull(t, "a-01.example.com", "agent", soul.StatusConnected, []string{"shared", "coven-a"}, "archon-a")
	seedSoulFull(t, "a-02.example.com", "agent", soul.StatusConnected, []string{"shared", "coven-a"}, "archon-a")
	seedSoulFull(t, "x-01.example.com", "agent", soul.StatusConnected, []string{"shared"}, "archon-a")

	base, stop := startServer(t, errandRunScopeRBAC("archon-a", "coven-a"))
	defer stop()
	tok := newValidTokenFor(t, "archon-a", []string{"cmd-ops"})

	// Target coven=shared резолвит все 3 (по AND-фильтру @> [shared]); scope=A
	// урезает до 2 видимых (a-01, a-02), x-01 (только shared) выпадает.
	code, body := postCommandVoyage(t, base, tok,
		`{"kind":"command","module":"core.cmd.shell","target":{"coven":["shared"]}}`)
	if code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202 (урезание без отказа); body=%s", code, body)
	}
	// scope_size = 2 (видимые по coven-a), НЕ 3 (весь shared).
	if scope := scopeSizeFromReply(t, body); scope != 2 {
		t.Errorf("scope_size = %d, want 2 (урезано до видимых coven-a, не весь shared)", scope)
	}
}

// Guard #5: Unrestricted (cluster-admin) → полный резолв (backcompat). Архонт без
// default_scope видит весь флот, command запускается на всех.
func TestIntegration_Voyage_CommandScope_Unrestricted_FullResolve_202(t *testing.T) {
	truncateOperators(t)
	seedOperator(t, "archon-admin", "")
	seedSoulFull(t, "a-01.example.com", "agent", soul.StatusConnected, []string{"coven-a"}, "archon-admin")
	seedSoulFull(t, "b-01.example.com", "agent", soul.StatusConnected, []string{"coven-b"}, "archon-admin")
	seedSoulFull(t, "b-02.example.com", "agent", soul.StatusConnected, []string{"coven-b"}, "archon-admin")

	base, stop := startServer(t, errandRunUnrestrictedRBAC("archon-admin"))
	defer stop()
	tok := newValidTokenFor(t, "archon-admin", []string{"cmd-admin"})

	// Target coven=B — Unrestricted-Архонт запускает (его scope не урезает).
	code, body := postCommandVoyage(t, base, tok,
		`{"kind":"command","module":"core.cmd.shell","target":{"coven":["coven-b"]}}`)
	if code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202 (Unrestricted backcompat); body=%s", code, body)
	}
	if scope := scopeSizeFromReply(t, body); scope != 2 {
		t.Errorf("scope_size = %d, want 2 (весь coven-b, без урезания)", scope)
	}

	// Явный SID любого coven — тоже OK (не 403): Unrestricted без DeniedExplicit.
	code2, body2 := postCommandVoyage(t, base, tok,
		`{"kind":"command","module":"core.cmd.shell","target":{"sids":["a-01.example.com"]}}`)
	if code2 != http.StatusAccepted {
		t.Fatalf("явный SID при Unrestricted: status = %d, want 202; body=%s", code2, body2)
	}
}

// Guard #6: regex-scoped errand.run on host=^web- → только web-* (regex-измерение
// Purview через Visible). target coven=prod (широкий) урезается до web-*.
func TestIntegration_Voyage_CommandScope_RegexScope_202(t *testing.T) {
	truncateOperators(t)
	seedOperator(t, "archon-web", "")
	seedSoulFull(t, "web-01.example.com", "agent", soul.StatusConnected, []string{"prod"}, "archon-web")
	seedSoulFull(t, "web-02.example.com", "agent", soul.StatusConnected, []string{"prod"}, "archon-web")
	seedSoulFull(t, "db-01.example.com", "agent", soul.StatusConnected, []string{"prod"}, "archon-web")

	base, stop := startServer(t, errandRunRegexScopeRBAC("archon-web", "^web-"))
	defer stop()
	tok := newValidTokenFor(t, "archon-web", []string{"cmd-regex-ops"})

	// Широкий target coven=prod (3 хоста); regex-scope ^web- урезает до 2 web-*.
	code, body := postCommandVoyage(t, base, tok,
		`{"kind":"command","module":"core.cmd.shell","target":{"coven":["prod"]}}`)
	if code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202; body=%s", code, body)
	}
	if scope := scopeSizeFromReply(t, body); scope != 2 {
		t.Errorf("scope_size = %d, want 2 (только web-*, db-01 урезан regex-ом)", scope)
	}

	// Явный db-01 (не матчит ^web-) → 403 (anti-escalation).
	code2, body2 := postCommandVoyage(t, base, tok,
		`{"kind":"command","module":"core.cmd.shell","target":{"sids":["db-01.example.com"]}}`)
	if code2 != http.StatusForbidden {
		t.Fatalf("явный db-01 (не ^web-): status = %d, want 403; body=%s", code2, body2)
	}
}

// Guard #7: soulprint-scoped (Partial — S3b-2b отложено) → под-показ: coven-
// измерение работает, soulprint НЕ вычисляется (хост, доступный ТОЛЬКО по
// soulprint, не зацеплен — fail-closed, никогда over-show). Архонт с
// coven=A + soulprint=... видит ровно coven-A; чужого не цепляет.
func TestIntegration_Voyage_CommandScope_SoulprintPartial_UnderShow_202(t *testing.T) {
	truncateOperators(t)
	seedOperator(t, "archon-sp", "")
	// Оба в shared. a-01 ещё и в coven-a (виден по coven-измерению). b-01 в
	// coven-b — доступен был бы ТОЛЬКО по soulprint (не вычисляется в MVP →
	// Partial под-показ), по coven-a НЕ виден → не зацеплен.
	seedSoulFull(t, "a-01.example.com", "agent", soul.StatusConnected, []string{"shared", "coven-a"}, "archon-sp")
	seedSoulFull(t, "b-01.example.com", "agent", soul.StatusConnected, []string{"shared", "coven-b"}, "archon-sp")

	base, stop := startServer(t, errandRunSoulprintScopeRBAC("archon-sp", "coven-a", "soulprint.self.os.family == 'debian'"))
	defer stop()
	tok := newValidTokenFor(t, "archon-sp", []string{"cmd-sp-ops"})

	// Широкий target coven=shared (2 хоста): coven-A работает (a-01), soulprint
	// под-показ (b-01 не зацеплен). scope_size = 1.
	code, body := postCommandVoyage(t, base, tok,
		`{"kind":"command","module":"core.cmd.shell","target":{"coven":["shared"]}}`)
	if code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202; body=%s", code, body)
	}
	if scope := scopeSizeFromReply(t, body); scope != 1 {
		t.Errorf("scope_size = %d, want 1 (coven-a работает, soulprint под-показ)", scope)
	}

	// Явный b-01 (вне coven-A, доступен был бы только по soulprint) → 403
	// (fail-closed: под-показ скрывает, явное указание = эскалация).
	code2, body2 := postCommandVoyage(t, base, tok,
		`{"kind":"command","module":"core.cmd.shell","target":{"sids":["b-01.example.com"]}}`)
	if code2 != http.StatusForbidden {
		t.Fatalf("явный b-01 (soulprint-only под-показ): status = %d, want 403; body=%s", code2, body2)
	}
}

// scopeSizeFromReply вытаскивает scope_size из 202-тела voyageCreateReply.
func scopeSizeFromReply(t *testing.T, body string) int {
	t.Helper()
	var reply struct {
		ScopeSize int `json:"scope_size"`
	}
	if err := json.Unmarshal([]byte(body), &reply); err != nil {
		t.Fatalf("decode reply: %v; body=%s", err, body)
	}
	return reply.ScopeSize
}
