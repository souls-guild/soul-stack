//go:build integration

// Integration tests on real PG for where a host's declared role comes from
// (Choir, ADR-044).
//
// Since the amendment 2026-07-30 (NIM-330) the answer is: from the host's Voice
// (`incarnation_choir_voices.role`) and from nowhere else.
//
// These tests used to seed the retired `spec.hosts[]` tier back by direct SQL
// and assert the resolver ignored it — `incarnation.spec` was freeform jsonb, so
// an old dump or a hand-written row could put the key back at any time.
// Migration 112 (NIM-408) dropped that column, so THAT tier can no longer be
// seeded and the negative test can no longer be written. This is narrower than
// "a fallback is now impossible": the row still carries freeform jsonb in
// `state` and `traits`, and 112's down migration restores `spec` outright.
//
// What closes the general case is the first test below: a member with no Voice
// must resolve to an EMPTY role, so any fallback tier — from any source — that
// hands such a host a role reddens it. That is broader than what the
// spec-seeding version could assert.
//
// What is left, then, is the positive rule in its three states: no Voice, a
// Voice with a NULL role, and a Voice with a role.
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
// *string and treats as "no role".
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

// seedVoiceRole inserts a Voice with an explicit (non-NULL) role. It mirrors
// seedVoiceNullRole, but role is bound as a non-empty string.
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

// TestIntegration_LoadIncarnationHosts_MemberWithoutVoiceHasNoRole is the
// NIM-330 rule stated positively: a host that is a member of the incarnation but
// belongs to no Choir resolves to an EMPTY role, and this is a normal answer
// rather than an error or a default.
//
// ADR-008 called a declared role "the only place of declared topology"; ADR-044
// p.2 moved that to the Voice and kept `spec.hosts[]` as a fallback for
// bootstrap-`create`; the amendment 2026-07-30 removed the fallback, and NIM-410
// removed the column it lived in. A host nobody put into a part has no declared
// role, and an empty role is the honest answer -- there is no longer a tier
// underneath for it to fall through to.
func TestIntegration_LoadIncarnationHosts_MemberWithoutVoiceHasNoRole(t *testing.T) {
	resetAll(t)
	ctx := context.Background()

	seedIncarnation(t, "redis-prod")
	seedSoul(t, "a.example.com", nil, soul.StatusConnected)
	seedMembership(t, "redis-prod", "a.example.com")

	r := NewResolver(integrationPool, nil, nil)
	hosts, err := r.LoadIncarnationHosts(ctx, "redis-prod")
	if err != nil {
		t.Fatalf("LoadIncarnationHosts: %v", err)
	}
	if len(hosts) != 1 || hosts[0].SID != "a.example.com" {
		t.Fatalf("got %v, want [a.example.com]", sids(hosts))
	}
	if hosts[0].Role != "" {
		t.Errorf("role = %q, want \"\" -- the host has no Voice, and the Voice is the only "+
			"source of a declared role (ADR-044 amendment 2026-07-30, NIM-330)",
			hosts[0].Role)
	}
	if hosts[0].Choirs != nil {
		t.Errorf("Choirs = %v, want nil (host belongs to no Choir)", hosts[0].Choirs)
	}
}

// TestIntegration_NullVoiceRole_RoleEmpty covers the degenerate Voice: the row
// exists but its role is SQL NULL (AddVoice writes NULL when role is omitted,
// migration 060). That used to fall through to `spec.hosts[].role`; it now
// resolves to an empty role, and there is nothing behind it to fall through to.
//
// The Voice itself is NOT erased by having no role -- the host still carries a
// stable `choirs[]` fact for `where:` targeting.
func TestIntegration_NullVoiceRole_RoleEmpty(t *testing.T) {
	resetAll(t)
	ctx := context.Background()

	seedIncarnation(t, "redis-prod")
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
		t.Errorf("role = %q, want \"\" (NULL voice.role is \"no role\"; there is no tier behind it)",
			hosts[0].Role)
	}
	if got := hosts[0].Choirs; len(got) != 1 || got[0] != "voters" {
		t.Errorf("Choirs = %v, want [voters] (membership exists even with NULL role)", got)
	}
}

// TestIntegration_ExplicitVoiceRole_IsTheRole is the positive control: an
// explicit non-NULL `voice.role` is what the resolver returns. Without it the
// two tests above would also pass on a resolver that always returned an empty
// role, which is the one way all three could be green while the feature is
// dead.
func TestIntegration_ExplicitVoiceRole_IsTheRole(t *testing.T) {
	resetAll(t)
	ctx := context.Background()

	seedIncarnation(t, "redis-prod")
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
		t.Errorf("role = %q, want Voice role \"primary\" (an empty role here would mean the "+
			"resolver never reads voice.role at all)",
			hosts[0].Role)
	}
	if got := hosts[0].Choirs; len(got) != 1 || got[0] != "voters" {
		t.Errorf("Choirs = %v, want [voters]", got)
	}
}
