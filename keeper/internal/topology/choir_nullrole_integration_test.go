//go:build integration

// Integration tests for NULL-role Voice (Choir, ADR-044) on real PG. They cover
// declared-role absorption `voice.role > spec.hosts[].role` in the degenerate
// voice.role IS NULL case: the key ADR-044 p.2(a) case where an omitted role
// is SQL NULL -> fallback to spec.hosts[].role, or to an empty role when the
// spec role is also absent (host outside declared spec ->
// soulprint.hosts[].role = null, ADR-008).
//
// Before Wave5 Pass1 this test was impossible because of the import cycle
// (tide_target.go); after decoupling, the topology resolver is tested directly
// on real PG. The pattern (testcontainers TestMain, resetAll/seed* helpers)
// matches this package's integration_test.go.

package topology

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/souls-guild/soul-stack/keeper/internal/soul"
)

// seedChoir inserts a Choir (declared group) into incarnation. It is needed
// because of the FK from incarnation_choir_voices to
// incarnation_choirs(incarnation_name, choir_name).
func seedChoir(t *testing.T, incarnationName, choirName string) {
	t.Helper()
	_, err := integrationPool.Exec(context.Background(),
		`INSERT INTO incarnation_choirs (incarnation_name, choir_name) VALUES ($1, $2)`,
		incarnationName, choirName)
	if err != nil {
		t.Fatalf("seedChoir(%s/%s): %v", incarnationName, choirName, err)
	}
}

// seedVoiceNullRole inserts a Voice with role IS NULL (SQL NULL, not an empty
// string) and emulates AddVoice with an omitted role (migration 060: role TEXT
// without NOT NULL). This is exactly the NULL that the resolver scans into
// *string and treats as "no role" -> fallback to spec.
func seedVoiceNullRole(t *testing.T, incarnationName, choirName, sid string) {
	t.Helper()
	_, err := integrationPool.Exec(context.Background(),
		`INSERT INTO incarnation_choir_voices (incarnation_name, choir_name, sid, role)
		 VALUES ($1, $2, $3, NULL)`,
		incarnationName, choirName, sid)
	if err != nil {
		t.Fatalf("seedVoiceNullRole(%s/%s/%s): %v", incarnationName, choirName, sid, err)
	}
}

// seedVoiceRole inserts a Voice with an explicit (non-NULL) role and exercises
// the override branch: voice.role wins over spec.hosts[].role (ADR-044 p.2).
// It mirrors seedVoiceNullRole, but role is bound as a non-empty string.
func seedVoiceRole(t *testing.T, incarnationName, choirName, sid, role string) {
	t.Helper()
	_, err := integrationPool.Exec(context.Background(),
		`INSERT INTO incarnation_choir_voices (incarnation_name, choir_name, sid, role)
		 VALUES ($1, $2, $3, $4)`,
		incarnationName, choirName, sid, role)
	if err != nil {
		t.Fatalf("seedVoiceRole(%s/%s/%s=%q): %v", incarnationName, choirName, sid, role, err)
	}
}

// assertNullRoleScannable is a guard: NULL role in incarnation_choir_voices
// must be read through the same *string scan as in the resolver
// (loadChoirMemberships). If the scan used plain string, pgx would fail with
// "cannot scan NULL into *string"; this SELECT catches the regression at the
// real-PG level before the main test assertion.
func assertNullRoleScannable(t *testing.T, incarnationName, choirName, sid string) {
	t.Helper()
	var role *string
	err := integrationPool.QueryRow(context.Background(),
		`SELECT role FROM incarnation_choir_voices
		 WHERE incarnation_name = $1 AND choir_name = $2 AND sid = $3`,
		incarnationName, choirName, sid).Scan(&role)
	if err != nil {
		if err == pgx.ErrNoRows {
			t.Fatalf("Voice %s/%s/%s was not inserted", incarnationName, choirName, sid)
		}
		t.Fatalf("NULL role scan failed (cannot-scan-NULL regression): %v", err)
	}
	if role != nil {
		t.Fatalf("role = %q, want SQL NULL (nil)", *role)
	}
}

// TestIntegration_NullVoiceRole_FallbacksToSpecRole covers case (a) from
// ADR-044 p.2(a): voice.role IS NULL while spec.hosts[].role is set, so the
// resulting role is the spec role (NULL Voice does NOT erase the declared role;
// fallback applies). On real PG it also verifies that NULL role does not break
// the roster scan (*string scan).
func TestIntegration_NullVoiceRole_FallbacksToSpecRole(t *testing.T) {
	resetAll(t)
	ctx := context.Background()

	seedIncarnation(t, "redis-prod", map[string]any{
		"hosts": []map[string]any{
			{"sid": "a.example.com", "role": "replica"},
		},
	})
	seedSoul(t, "a.example.com", nil, soul.StatusConnected)
	seedMembership(t, "redis-prod", "a.example.com")
	seedChoir(t, "redis-prod", "voters")
	seedVoiceNullRole(t, "redis-prod", "voters", "a.example.com")

	assertNullRoleScannable(t, "redis-prod", "voters", "a.example.com")

	r := NewResolver(integrationPool, nil, nil)
	hosts, err := r.LoadIncarnationHosts(ctx, "redis-prod")
	if err != nil {
		t.Fatalf("LoadIncarnationHosts: %v", err)
	}
	if len(hosts) != 1 || hosts[0].SID != "a.example.com" {
		t.Fatalf("got %v, want [a.example.com]", sids(hosts))
	}
	if hosts[0].Role != "replica" {
		t.Errorf("role = %q, want spec role \"replica\" (NULL voice.role -> fallback to spec)", hosts[0].Role)
	}
	// Voice without role still yields a stable choir fact for where:.
	if got := hosts[0].Choirs; len(got) != 1 || got[0] != "voters" {
		t.Errorf("Choirs = %v, want [voters] (membership exists even with NULL role)", got)
	}
}

// TestIntegration_NullVoiceRole_NoSpec_RoleEmpty covers case (b):
// voice.role IS NULL and spec.hosts[].role is absent (host outside declared
// spec), so the resulting role is empty (soulprint.hosts[].role = null,
// ADR-008), without panic or error.
func TestIntegration_NullVoiceRole_NoSpec_RoleEmpty(t *testing.T) {
	resetAll(t)
	ctx := context.Background()

	// spec has no hosts at all, so the host is definitely outside declared spec.
	seedIncarnation(t, "redis-prod", map[string]any{})
	seedSoul(t, "b.example.com", nil, soul.StatusConnected)
	seedMembership(t, "redis-prod", "b.example.com")
	seedChoir(t, "redis-prod", "voters")
	seedVoiceNullRole(t, "redis-prod", "voters", "b.example.com")

	assertNullRoleScannable(t, "redis-prod", "voters", "b.example.com")

	r := NewResolver(integrationPool, nil, nil)
	hosts, err := r.LoadIncarnationHosts(ctx, "redis-prod")
	if err != nil {
		t.Fatalf("LoadIncarnationHosts: %v", err)
	}
	if len(hosts) != 1 || hosts[0].SID != "b.example.com" {
		t.Fatalf("got %v, want [b.example.com]", sids(hosts))
	}
	if hosts[0].Role != "" {
		t.Errorf("role = %q, want \"\" (NULL voice.role + no spec -> null, host outside declared spec)", hosts[0].Role)
	}
	if got := hosts[0].Choirs; len(got) != 1 || got[0] != "voters" {
		t.Errorf("Choirs = %v, want [voters]", got)
	}
}

// TestIntegration_ExplicitVoiceRole_OverridesSpecRole covers case (c), the
// control case: voice.role is explicit (not NULL) and differs from
// spec.hosts[].role, so the resulting role is the Voice role (ADR-044 p.2:
// Choir absorbs the declared role). It verifies that the resolver's NULL branch
// (nil *string -> fallback) did not break override: the non-empty role from the
// column must win over spec instead of being lost.
func TestIntegration_ExplicitVoiceRole_OverridesSpecRole(t *testing.T) {
	resetAll(t)
	ctx := context.Background()

	// spec gives the host role "replica", intentionally DIFFERENT from voice.role below.
	seedIncarnation(t, "redis-prod", map[string]any{
		"hosts": []map[string]any{
			{"sid": "c.example.com", "role": "replica"},
		},
	})
	seedSoul(t, "c.example.com", nil, soul.StatusConnected)
	seedMembership(t, "redis-prod", "c.example.com")
	seedChoir(t, "redis-prod", "voters")
	seedVoiceRole(t, "redis-prod", "voters", "c.example.com", "primary")

	r := NewResolver(integrationPool, nil, nil)
	hosts, err := r.LoadIncarnationHosts(ctx, "redis-prod")
	if err != nil {
		t.Fatalf("LoadIncarnationHosts: %v", err)
	}
	if len(hosts) != 1 || hosts[0].SID != "c.example.com" {
		t.Fatalf("got %v, want [c.example.com]", sids(hosts))
	}
	if hosts[0].Role != "primary" {
		t.Errorf("role = %q, want Voice role \"primary\" (voice.role wins over spec \"replica\")", hosts[0].Role)
	}
	if got := hosts[0].Choirs; len(got) != 1 || got[0] != "voters" {
		t.Errorf("Choirs = %v, want [voters]", got)
	}
}
