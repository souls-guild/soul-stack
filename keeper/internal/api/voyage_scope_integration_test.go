//go:build integration

package api

// E2E over a real PG/router: ADR-047 S4 — the command path of Voyage (`errand.run`)
// intersects the resolved target with the Archon's Purview (target ∩ Purview). Hybrid
// semantics: an explicit foreign SID → 403; a wide target → narrowing; an empty
// intersection → 422; Unrestricted (cluster-admin) → full resolve (backcompat).
//
// Closes a security leak: the command target used to resolve cluster-wide without
// scope, so a scoped Archon could run a command on a foreign coven.

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/souls-guild/soul-stack/keeper/internal/rbac/rbactest"
	"github.com/souls-guild/soul-stack/keeper/internal/soul"
)

// errandRunScopeRBAC — a role with scoped errand.run (`on coven=<coven>`): the Archon
// can only launch a command-Voyage on hosts in its own coven. The scope for the S4
// intersection comes from the selector of the errand.run permission itself.
func errandRunScopeRBAC(aid, coven string) *rbactest.Config {
	return &rbactest.Config{
		Roles: []rbactest.Role{
			{Name: "cmd-ops", Operators: []string{aid}, Permissions: []string{
				"errand.run on coven=" + coven,
			}},
		},
	}
}

// errandRunRegexScopeRBAC — scoped errand.run on the regex dimension (`on regex=<pat>`):
// command-target visibility = regexMatch against the SID ([soulpurview.CompiledScope.Visible]).
func errandRunRegexScopeRBAC(aid, pattern string) *rbactest.Config {
	return &rbactest.Config{
		Roles: []rbactest.Role{
			{Name: "cmd-regex-ops", Operators: []string{aid}, Permissions: []string{
				"errand.run on regex='" + pattern + "'",
			}},
		},
	}
}

// errandRunSoulprintScopeRBAC — scoped errand.run with the coven + soulprint dimension
// (S3b-2b is deferred → Purview.Partial: under-showing, coven works, soulprint does not).
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

// errandRunUnrestrictedRBAC — bare errand.run (no default_scope) →
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

// postCommandVoyage — POST /v1/voyages kind=command; returns the status + body.
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
// (the intersection is empty, the foreign coven did not launch).
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
		t.Fatalf("status = %d, want 422 (чужой coven урезан в butль); body=%s", code, body)
	}
	if !strings.Contains(body, "voyage_empty_target") {
		t.Errorf("detail не withдержит voyage_empty_target: %s", body)
	}
}

// Guard #2: scoped coven=A, an explicit sids=[host-in-B] → 403 (anti-escalation: an
// explicit foreign host = an escalation attempt, not silent narrowing).
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

// Guard #3: scoped coven=A, target coven=A, a host with covens=[A,B] → the run is OK
// (visible via A; multi-membership doesn't get in the way).
func TestIntegration_Voyage_CommandScope_MultiCovenHostVisible_202(t *testing.T) {
	truncateOperators(t)
	seedOperator(t, "archon-a", "")
	// A host in both covens A and B. The Archon is scoped to A → visible.
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

// Guard #4: a wide target coven=[shared] (3 hosts, mixed visibility), scope
// coven=A → only the subset visible via A resolves (narrowing, NOT the whole
// shared). shared+A hosts are visible; the shared-only host is narrowed out.
func TestIntegration_Voyage_CommandScope_WideTargetTrimmed_202(t *testing.T) {
	truncateOperators(t)
	seedOperator(t, "archon-a", "")
	// All three are in shared; the first two are also in coven-a (visible under scope=A), the third is not.
	seedSoulFull(t, "a-01.example.com", "agent", soul.StatusConnected, []string{"shared", "coven-a"}, "archon-a")
	seedSoulFull(t, "a-02.example.com", "agent", soul.StatusConnected, []string{"shared", "coven-a"}, "archon-a")
	seedSoulFull(t, "x-01.example.com", "agent", soul.StatusConnected, []string{"shared"}, "archon-a")

	base, stop := startServer(t, errandRunScopeRBAC("archon-a", "coven-a"))
	defer stop()
	tok := newValidTokenFor(t, "archon-a", []string{"cmd-ops"})

	// Target coven=shared resolves all 3 (via the AND filter @> [shared]); scope=A
	// narrows it to the 2 visible (a-01, a-02), x-01 (shared-only) drops out.
	code, body := postCommandVoyage(t, base, tok,
		`{"kind":"command","module":"core.cmd.shell","target":{"coven":["shared"]}}`)
	if code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202 (урезание без отказа); body=%s", code, body)
	}
	// scope_size = 2 (visible via coven-a), NOT 3 (the whole shared).
	if scope := scopeSizeFromReply(t, body); scope != 2 {
		t.Errorf("scope_size = %d, want 2 (урезаbut to видимых coven-a, не весь shared)", scope)
	}
}

// Guard #5: Unrestricted (cluster-admin) → full resolve (backcompat). An Archon
// without default_scope sees every soul, the command runs on all of them.
func TestIntegration_Voyage_CommandScope_Unrestricted_FullResolve_202(t *testing.T) {
	truncateOperators(t)
	seedOperator(t, "archon-admin", "")
	seedSoulFull(t, "a-01.example.com", "agent", soul.StatusConnected, []string{"coven-a"}, "archon-admin")
	seedSoulFull(t, "b-01.example.com", "agent", soul.StatusConnected, []string{"coven-b"}, "archon-admin")
	seedSoulFull(t, "b-02.example.com", "agent", soul.StatusConnected, []string{"coven-b"}, "archon-admin")

	base, stop := startServer(t, errandRunUnrestrictedRBAC("archon-admin"))
	defer stop()
	tok := newValidTokenFor(t, "archon-admin", []string{"cmd-admin"})

	// Target coven=B — an Unrestricted Archon launches it (its scope doesn't narrow anything).
	code, body := postCommandVoyage(t, base, tok,
		`{"kind":"command","module":"core.cmd.shell","target":{"coven":["coven-b"]}}`)
	if code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202 (Unrestricted backcompat); body=%s", code, body)
	}
	if scope := scopeSizeFromReply(t, body); scope != 2 {
		t.Errorf("scope_size = %d, want 2 (весь coven-b, без урезания)", scope)
	}

	// An explicit SID from any coven is also OK (not 403): Unrestricted has no DeniedExplicit.
	code2, body2 := postCommandVoyage(t, base, tok,
		`{"kind":"command","module":"core.cmd.shell","target":{"sids":["a-01.example.com"]}}`)
	if code2 != http.StatusAccepted {
		t.Fatalf("явный SID при Unrestricted: status = %d, want 202; body=%s", code2, body2)
	}
}

// Guard #6: regex-scoped errand.run on host=^web- → only web-* (the regex dimension
// of Purview via Visible). target coven=prod (wide) narrows down to web-*.
func TestIntegration_Voyage_CommandScope_RegexScope_202(t *testing.T) {
	truncateOperators(t)
	seedOperator(t, "archon-web", "")
	seedSoulFull(t, "web-01.example.com", "agent", soul.StatusConnected, []string{"prod"}, "archon-web")
	seedSoulFull(t, "web-02.example.com", "agent", soul.StatusConnected, []string{"prod"}, "archon-web")
	seedSoulFull(t, "db-01.example.com", "agent", soul.StatusConnected, []string{"prod"}, "archon-web")

	base, stop := startServer(t, errandRunRegexScopeRBAC("archon-web", "^web-"))
	defer stop()
	tok := newValidTokenFor(t, "archon-web", []string{"cmd-regex-ops"})

	// A wide target coven=prod (3 hosts); regex-scope ^web- narrows it to 2 web-*.
	code, body := postCommandVoyage(t, base, tok,
		`{"kind":"command","module":"core.cmd.shell","target":{"coven":["prod"]}}`)
	if code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202; body=%s", code, body)
	}
	if scope := scopeSizeFromReply(t, body); scope != 2 {
		t.Errorf("scope_size = %d, want 2 (только web-*, db-01 урезан regex-ом)", scope)
	}

	// An explicit db-01 (doesn't match ^web-) → 403 (anti-escalation).
	code2, body2 := postCommandVoyage(t, base, tok,
		`{"kind":"command","module":"core.cmd.shell","target":{"sids":["db-01.example.com"]}}`)
	if code2 != http.StatusForbidden {
		t.Fatalf("явный db-01 (не ^web-): status = %d, want 403; body=%s", code2, body2)
	}
}

// Guard #7: soulprint-scoped (Partial — S3b-2b deferred) → under-showing: the coven
// dimension works, soulprint is NOT evaluated (a host reachable ONLY via
// soulprint is not caught — fail-closed, never over-show). An Archon with
// coven=A + soulprint=... sees exactly coven-A; nothing foreign gets caught.
func TestIntegration_Voyage_CommandScope_SoulprintPartial_UnderShow_202(t *testing.T) {
	truncateOperators(t)
	seedOperator(t, "archon-sp", "")
	// Both are in shared. a-01 is also in coven-a (visible via the coven dimension). b-01 is
	// in coven-b — it would only be reachable via soulprint (not evaluated in the MVP →
	// Partial under-showing), it is NOT visible via coven-a → not caught.
	seedSoulFull(t, "a-01.example.com", "agent", soul.StatusConnected, []string{"shared", "coven-a"}, "archon-sp")
	seedSoulFull(t, "b-01.example.com", "agent", soul.StatusConnected, []string{"shared", "coven-b"}, "archon-sp")

	base, stop := startServer(t, errandRunSoulprintScopeRBAC("archon-sp", "coven-a", "soulprint.self.os.family == 'debian'"))
	defer stop()
	tok := newValidTokenFor(t, "archon-sp", []string{"cmd-sp-ops"})

	// A wide target coven=shared (2 hosts): coven-A works (a-01), soulprint
	// under-shows (b-01 is not caught). scope_size = 1.
	code, body := postCommandVoyage(t, base, tok,
		`{"kind":"command","module":"core.cmd.shell","target":{"coven":["shared"]}}`)
	if code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202; body=%s", code, body)
	}
	if scope := scopeSizeFromReply(t, body); scope != 1 {
		t.Errorf("scope_size = %d, want 1 (coven-a работает, soulprint под-показ)", scope)
	}

	// An explicit b-01 (outside coven-A, would only be reachable via soulprint) → 403
	// (fail-closed: under-showing hides it, an explicit reference = escalation).
	code2, body2 := postCommandVoyage(t, base, tok,
		`{"kind":"command","module":"core.cmd.shell","target":{"sids":["b-01.example.com"]}}`)
	if code2 != http.StatusForbidden {
		t.Fatalf("явный b-01 (soulprint-only под-показ): status = %d, want 403; body=%s", code2, body2)
	}
}

// scopeSizeFromReply extracts scope_size from the voyageCreateReply 202 body.
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
