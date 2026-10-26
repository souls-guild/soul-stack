//go:build integration

// Integration tests for the owner of a machine operator (migration 131,
// ADR-014 amendment NIM-1057) against real Postgres: who may be an owner, the
// schema's own refusals, ownership through a Synod, the coverage check over
// the real RBAC rows, and the refusal to delete a Synod that owns operators.

package operator

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/souls-guild/soul-stack/keeper/internal/rbac"
)

func ownerService(t *testing.T) *Service {
	t.Helper()
	s, err := NewService(ServiceDeps{
		Pool: integrationPool, Issuer: &syncIssuer{}, RBAC: &fakeRBAC{},
		TTLDefault: 24 * time.Hour, TTLMax: 90 * 24 * time.Hour,
	})
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	return s
}

// seedPerson — an Archon that signs in itself, the only kind that may own.
func seedPerson(t *testing.T, aid string, m AuthMethod) {
	t.Helper()
	via := CreatedViaUser
	if m == AuthMethodLDAP || m == AuthMethodOIDC {
		via = string(m)
	}
	if err := Insert(context.Background(), integrationPool, &Operator{AID: aid, DisplayName: aid, AuthMethod: m, CreatedVia: via}); err != nil {
		t.Fatalf("seed %q: %v", aid, err)
	}
}

func execOrFatal(t *testing.T, sql string, args ...any) {
	t.Helper()
	if _, err := integrationPool.Exec(context.Background(), sql, args...); err != nil {
		t.Fatalf("%s: %v", sql, err)
	}
}

// grantRole creates role (if new) with perms and binds it to aid.
func grantRole(t *testing.T, role, aid string, perms ...string) {
	t.Helper()
	execOrFatal(t, `INSERT INTO rbac_roles (name, description) VALUES ($1, '') ON CONFLICT (name) DO NOTHING`, role)
	for _, p := range perms {
		execOrFatal(t, `INSERT INTO rbac_role_permissions (role_name, permission) VALUES ($1, $2) ON CONFLICT DO NOTHING`, role, p)
	}
	execOrFatal(t, `INSERT INTO rbac_role_operators (role_name, aid) VALUES ($1, $2)`, role, aid)
}

func seedSynod(t *testing.T, name string, members ...string) {
	t.Helper()
	execOrFatal(t, `INSERT INTO synods (name) VALUES ($1)`, name)
	for _, m := range members {
		execOrFatal(t, `INSERT INTO synod_operators (synod_name, aid) VALUES ($1, $2)`, name, m)
	}
}

func ownerColumns(t *testing.T, aid string) Owner {
	t.Helper()
	op, err := SelectByAID(context.Background(), integrationPool, aid)
	if err != nil {
		t.Fatalf("SelectByAID(%s): %v", aid, err)
	}
	return OwnerOf(op)
}

func TestIntegration_Owner_SetValidatesTheOwner(t *testing.T) {
	resetOperators(t)
	ctx := context.Background()
	seedActiveOperator(t, "dba-sd", nil)
	seedActiveOperator(t, "other-sa", nil)
	seedPerson(t, "alice", AuthMethodLocal)
	seedPerson(t, "mike", AuthMethodOIDC)
	seedPerson(t, "gone", AuthMethodLocal)
	seedPerson(t, "lucy", AuthMethodLocal)
	if err := Revoke(ctx, integrationPool, "gone", ""); err != nil {
		t.Fatalf("revoke gone: %v", err)
	}
	seedSynod(t, "team")
	s := ownerService(t)
	set := func(aid string, o Owner) (SetOwnerResult, error) {
		return s.SetOwner(ctx, SetOwnerInput{AID: aid, Owner: o})
	}

	res, err := set("dba-sd", Owner{AID: "alice"})
	if err != nil || !res.Previous.IsZero() || ownerColumns(t, "dba-sd") != (Owner{AID: "alice"}) {
		t.Fatalf("set alice: res %+v err %v, row %+v", res, err, ownerColumns(t, "dba-sd"))
	}
	res, err = set("dba-sd", Owner{Synod: "team"})
	if err != nil || res.Previous != (Owner{AID: "alice"}) || ownerColumns(t, "dba-sd") != (Owner{Synod: "team"}) {
		t.Fatalf("set team: res %+v err %v, row %+v", res, err, ownerColumns(t, "dba-sd"))
	}
	if _, err := set("dba-sd", Owner{AID: "mike"}); err != nil {
		t.Fatalf("an OIDC person may own: %v", err)
	}
	res, err = set("dba-sd", Owner{})
	if err != nil || res.Previous != (Owner{AID: "mike"}) || !ownerColumns(t, "dba-sd").IsZero() {
		t.Fatalf("clear: res %+v err %v, row %+v", res, err, ownerColumns(t, "dba-sd"))
	}

	for name, o := range map[string]Owner{
		"a machine operator":  {AID: "other-sa"},
		"the operator itself": {AID: "dba-sd"},
		"a revoked person":    {AID: "gone"},
		"an unknown Archon":   {AID: "nobody"},
		"an unknown Synod":    {Synod: "ghost"},
		"both fields":         {AID: "alice", Synod: "team"},
	} {
		if _, err := set("dba-sd", o); !errors.Is(err, ErrInvalidOwner) {
			t.Errorf("%s: err = %v, want ErrInvalidOwner", name, err)
		}
	}
	if !ownerColumns(t, "dba-sd").IsZero() {
		t.Fatalf("a refused owner was written: %+v", ownerColumns(t, "dba-sd"))
	}
	if _, err := set("lucy", Owner{AID: "alice"}); !errors.Is(err, ErrOwnerNotAllowed) {
		t.Errorf("owner of a person: err = %v, want ErrOwnerNotAllowed", err)
	}
	if _, err := set("nobody", Owner{AID: "alice"}); !errors.Is(err, ErrOperatorNotFound) {
		t.Errorf("unknown operator: err = %v, want ErrOperatorNotFound", err)
	}
	if err := Revoke(ctx, integrationPool, "other-sa", ""); err != nil {
		t.Fatalf("revoke other-sa: %v", err)
	}
	if _, err := set("other-sa", Owner{AID: "alice"}); !errors.Is(err, ErrOperatorAlreadyRevoked) {
		t.Errorf("revoked operator: err = %v, want ErrOperatorAlreadyRevoked", err)
	}
}

// The schema refuses what the code refuses, for a writer that is not the code.
func TestIntegration_Owner_SchemaRefusesWhatTheCodeRefuses(t *testing.T) {
	resetOperators(t)
	seedActiveOperator(t, "dba-sd", nil)
	seedPerson(t, "alice", AuthMethodLocal)
	seedSynod(t, "team")
	for name, sql := range map[string]string{
		"operators_owner_jwt_only": `UPDATE operators SET owner_aid = 'dba-sd' WHERE aid = 'alice'`,
		"operators_owner_not_self": `UPDATE operators SET owner_aid = 'dba-sd' WHERE aid = 'dba-sd'`,
		"operators_owner_one":      `UPDATE operators SET owner_aid = 'alice', owner_synod = 'team' WHERE aid = 'dba-sd'`,
		"operators_owner_aid_fk":   `UPDATE operators SET owner_aid = 'nobody' WHERE aid = 'dba-sd'`,
		"operators_owner_synod_fk": `UPDATE operators SET owner_synod = 'ghost' WHERE aid = 'dba-sd'`,
	} {
		_, err := integrationPool.Exec(context.Background(), sql)
		if err == nil || !strings.Contains(err.Error(), name) {
			t.Errorf("%s: err = %v, want a violation of %s", name, err, name)
		}
	}
}

func TestIntegration_Owner_CreateWithOwner(t *testing.T) {
	resetOperators(t)
	ctx := context.Background()
	seedClusterAdmin(t, "archon-admin", nil)
	seedPerson(t, "alice", AuthMethodLocal)
	s := ownerService(t)

	res, err := s.Create(ctx, CreateInput{AID: "dba-sd", CallerAID: "archon-admin", TTL: 90 * 24 * time.Hour, Owner: Owner{AID: "alice"}})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if res.Owner != (Owner{AID: "alice"}) || ownerColumns(t, "dba-sd") != (Owner{AID: "alice"}) || res.JWT == "" {
		t.Fatalf("created %+v, row owner %+v", res, ownerColumns(t, "dba-sd"))
	}
	if _, err := s.Create(ctx, CreateInput{AID: "dba-sd2", CallerAID: "archon-admin", Owner: Owner{AID: "nobody"}}); !errors.Is(err, ErrInvalidOwner) {
		t.Fatalf("bad owner: err = %v, want ErrInvalidOwner", err)
	}
	if _, err := SelectByAID(ctx, integrationPool, "dba-sd2"); !errors.Is(err, ErrOperatorNotFound) {
		t.Fatalf("an operator refused for its owner was created anyway: %v", err)
	}

	// A person has no owner: refused as such, not by the CHECK as a 500.
	s.SetPasswordPolicy(&PasswordPolicy{MinLength: 12})
	if _, err := s.Create(ctx, CreateInput{AID: "bob", CallerAID: "archon-admin", AuthMethod: AuthMethodLocal,
		Password: "a long enough password", Owner: Owner{AID: "alice"}}); !errors.Is(err, ErrOwnerNotAllowed) {
		t.Fatalf("a local operator with an owner: err = %v, want ErrOwnerNotAllowed", err)
	}
}

func TestIntegration_Owner_IssueNeedsCoverage(t *testing.T) {
	resetOperators(t)
	ctx := context.Background()
	seedActiveOperator(t, "dba-sd", nil)
	seedPerson(t, "alice", AuthMethodLocal)
	grantRole(t, "sd-reader", "dba-sd", "incarnation.get", "service.list")
	grantRole(t, "dragonfly-reader", "alice", "incarnation.get on service=dragonfly", "service.list")
	s := ownerService(t)
	if _, err := s.SetOwner(ctx, SetOwnerInput{AID: "dba-sd", Owner: Owner{AID: "alice"}}); err != nil {
		t.Fatalf("SetOwner: %v", err)
	}
	issue := func(caller, jti string) (*IssueTokenResult, error) {
		return s.IssueOwnedJWT(ctx, IssueOwnedJWTInput{AID: "dba-sd", CallerAID: caller, CallerJWTID: jti, TTL: 90 * 24 * time.Hour, Label: "rotation"})
	}
	countIssued := func() int {
		recs, _, err := ListJWTs(ctx, integrationPool, "dba-sd", JWTFilter{}, 0, 50)
		if err != nil {
			t.Fatalf("ListJWTs: %v", err)
		}
		return len(recs)
	}

	_, err := issue("alice", sessionOf(t, "alice"))
	if !errors.Is(err, ErrOwnerLacksPermissions) || !strings.Contains(err.Error(), "incarnation.get") {
		t.Fatalf("narrower owner: err = %v, want ErrOwnerLacksPermissions naming incarnation.get", err)
	}
	if n := countIssued(); n != 0 {
		t.Fatalf("a refused issue left %d token rows", n)
	}

	grantRole(t, "sd-reader", "alice")
	res, err := issue("alice", sessionOf(t, "alice"))
	if err != nil {
		t.Fatalf("covering owner: %v", err)
	}
	recs, _, _ := ListJWTs(ctx, integrationPool, "dba-sd", JWTFilter{}, 0, 50)
	if len(recs) != 1 || recs[0].JWTID != res.JWTID || recs[0].Kind != JWTKindIssued || recs[0].Label != "rotation" ||
		recs[0].CreatedByAID == nil || *recs[0].CreatedByAID != "alice" || recs[0].ExpiresAt.Sub(recs[0].CreatedAt) != 90*24*time.Hour {
		t.Fatalf("owner-minted row = %+v", recs)
	}

	personal := mintFor(t, "alice", JWTKindPersonal, "alice", "")
	if _, err := issue("alice", personal.JWTID); !errors.Is(err, ErrOwnedJWTFromPersonal) {
		t.Fatalf("personal token: err = %v, want ErrOwnedJWTFromPersonal", err)
	}

	seedPerson(t, "carol", AuthMethodLocal)
	grantClusterAdmin(t, "carol")
	if _, err := issue("carol", sessionOf(t, "carol")); !errors.Is(err, ErrOperatorNotFound) {
		t.Fatalf("a cluster-admin that is not the owner: err = %v, want ErrOperatorNotFound", err)
	}
}

func TestIntegration_Owner_ThroughASynod(t *testing.T) {
	resetOperators(t)
	ctx := context.Background()
	seedActiveOperator(t, "dba-sd", nil)
	seedActiveOperator(t, "dba-gw", nil)
	seedActiveOperator(t, "dba-old", nil)
	seedPerson(t, "alice", AuthMethodLocal)
	seedPerson(t, "bob", AuthMethodLocal)
	seedPerson(t, "carol", AuthMethodLocal)
	grantClusterAdmin(t, "bob")
	grantClusterAdmin(t, "carol")
	seedSynod(t, "team", "bob")
	s := ownerService(t)
	for aid, o := range map[string]Owner{"dba-sd": {AID: "alice"}, "dba-gw": {Synod: "team"}, "dba-old": {Synod: "team"}} {
		if _, err := s.SetOwner(ctx, SetOwnerInput{AID: aid, Owner: o}); err != nil {
			t.Fatalf("SetOwner(%s): %v", aid, err)
		}
	}
	if err := Revoke(ctx, integrationPool, "dba-old", ""); err != nil {
		t.Fatalf("revoke dba-old: %v", err)
	}

	owned := func(caller string) []string {
		ops, total, err := s.ListOwned(ctx, caller, 0, 50)
		if err != nil {
			t.Fatalf("ListOwned(%s): %v", caller, err)
		}
		var out []string
		for _, op := range ops {
			out = append(out, op.AID)
		}
		if total != len(out) {
			t.Fatalf("ListOwned(%s): total %d, items %v", caller, total, out)
		}
		return out
	}
	if got := owned("alice"); len(got) != 1 || got[0] != "dba-sd" {
		t.Errorf("alice owns %v, want [dba-sd]", got)
	}
	if got := owned("bob"); len(got) != 1 || got[0] != "dba-gw" {
		t.Errorf("bob owns %v, want [dba-gw] (the revoked dba-old is not listed)", got)
	}
	if got := owned("carol"); len(got) != 0 {
		t.Errorf("carol owns %v, want nothing: holding cluster-admin is not owning", got)
	}

	res, err := s.IssueOwnedJWT(ctx, IssueOwnedJWTInput{AID: "dba-gw", CallerAID: "bob"})
	if err != nil {
		t.Fatalf("a Synod member issues: %v", err)
	}
	if _, err := s.RevokeOwnedJWT(ctx, "carol", "dba-gw", res.JWTID); !errors.Is(err, ErrOperatorNotFound) {
		t.Fatalf("a stranger revokes: err = %v, want ErrOperatorNotFound", err)
	}
	if _, _, err := s.ListOwnedJWTs(ctx, "alice", "dba-gw", JWTFilter{}, 0, 50); !errors.Is(err, ErrOperatorNotFound) {
		t.Fatalf("a stranger lists: err = %v, want ErrOperatorNotFound", err)
	}
	rec, err := s.RevokeOwnedJWT(ctx, "bob", "dba-gw", res.JWTID)
	if err != nil || rec.RevokedByAID == nil || *rec.RevokedByAID != "bob" {
		t.Fatalf("the owner revokes: rec %+v err %v", rec, err)
	}

	// A machine operator in the owning Synod owns nothing through it: otherwise
	// one machine operator could mint another's tokens.
	execOrFatal(t, `INSERT INTO synod_operators (synod_name, aid) VALUES ('team', 'dba-sd')`)
	grantClusterAdmin(t, "dba-sd")
	if got := owned("dba-sd"); len(got) != 0 {
		t.Errorf("a machine operator in the Synod owns %v, want nothing", got)
	}
	if _, err := s.IssueOwnedJWT(ctx, IssueOwnedJWTInput{AID: "dba-gw", CallerAID: "dba-sd"}); !errors.Is(err, ErrOperatorNotFound) {
		t.Fatalf("a machine operator in the Synod issues: err = %v, want ErrOperatorNotFound", err)
	}

	execOrFatal(t, `DELETE FROM synod_operators WHERE synod_name = 'team' AND aid = 'bob'`)
	if _, err := s.IssueOwnedJWT(ctx, IssueOwnedJWTInput{AID: "dba-gw", CallerAID: "bob"}); !errors.Is(err, ErrOperatorNotFound) {
		t.Fatalf("a former member issues: err = %v, want ErrOperatorNotFound", err)
	}
}

func TestIntegration_Owner_SynodThatOwnsCannotBeDeleted(t *testing.T) {
	resetOperators(t)
	ctx := context.Background()
	seedActiveOperator(t, "dba-gw", nil)
	seedActiveOperator(t, "dba-old", nil)
	seedSynod(t, "team")
	s := ownerService(t)
	for _, aid := range []string{"dba-gw", "dba-old"} {
		if _, err := s.SetOwner(ctx, SetOwnerInput{AID: aid, Owner: Owner{Synod: "team"}}); err != nil {
			t.Fatalf("SetOwner(%s): %v", aid, err)
		}
	}
	if err := Revoke(ctx, integrationPool, "dba-old", ""); err != nil {
		t.Fatalf("revoke dba-old: %v", err)
	}
	rs, err := rbac.NewService(rbac.ServiceDeps{Pool: integrationPool})
	if err != nil {
		t.Fatalf("rbac.NewService: %v", err)
	}
	err = rs.DeleteSynod(ctx, "team")
	if !errors.Is(err, rbac.ErrSynodOwnsOperators) || !strings.Contains(err.Error(), "dba-gw, dba-old") {
		t.Fatalf("delete an owning Synod: err = %v, want ErrSynodOwnsOperators naming dba-gw, dba-old (a revoked one too)", err)
	}
	for _, aid := range []string{"dba-gw", "dba-old"} {
		if _, err := s.SetOwner(ctx, SetOwnerInput{AID: aid}); err != nil {
			t.Fatalf("clear the owner of %s (dba-old is revoked): %v", aid, err)
		}
	}
	if err := rs.DeleteSynod(ctx, "team"); err != nil {
		t.Fatalf("delete after the owners were cleared: %v", err)
	}
}

// The owner's and the operator's rights are read the way the enforcer grants
// them: a derived role under its parent's ceiling, a role's default_scope, and
// a role held through a Synod all count (the HIGH and the MEDIUM test gap of the
// NIM-1057 review).
func TestIntegration_Owner_CoverageReadsEffectiveRights(t *testing.T) {
	resetOperators(t)
	ctx := context.Background()
	seedActiveOperator(t, "dba-sd", nil)
	seedPerson(t, "alice", AuthMethodLocal)
	s := ownerService(t)
	if _, err := s.SetOwner(ctx, SetOwnerInput{AID: "dba-sd", Owner: Owner{AID: "alice"}}); err != nil {
		t.Fatalf("SetOwner: %v", err)
	}
	issue := func() error {
		_, err := s.IssueOwnedJWT(ctx, IssueOwnedJWTInput{AID: "dba-sd", CallerAID: "alice"})
		return err
	}
	grantRole(t, "sd-reader", "dba-sd", "incarnation.get")

	// A derived role: dfly-view's stored row is a bare incarnation.get, but its
	// parent caps it at service=dragonfly.
	execOrFatal(t, `INSERT INTO rbac_roles (name, description, default_scope) VALUES ('dfly-ops', '', 'service=dragonfly')`)
	execOrFatal(t, `INSERT INTO rbac_role_permissions (role_name, permission) VALUES ('dfly-ops', 'incarnation.get'), ('dfly-ops', 'incarnation.run')`)
	execOrFatal(t, `INSERT INTO rbac_roles (name, description, parent_role, scope_mode) VALUES ('dfly-view', '', 'dfly-ops', 'track')`)
	execOrFatal(t, `INSERT INTO rbac_role_permissions (role_name, permission) VALUES ('dfly-view', 'incarnation.get')`)
	execOrFatal(t, `INSERT INTO rbac_role_operators (role_name, aid) VALUES ('dfly-view', 'alice')`)
	if err := issue(); !errors.Is(err, ErrOwnerLacksPermissions) {
		t.Fatalf("derived role under a scoped parent: err = %v, want ErrOwnerLacksPermissions", err)
	}

	// A plain role with a default_scope is scoped the same way.
	grantRole(t, "plain-scoped", "alice")
	execOrFatal(t, `UPDATE rbac_roles SET default_scope = 'service=dragonfly' WHERE name = 'plain-scoped'`)
	execOrFatal(t, `INSERT INTO rbac_role_permissions (role_name, permission) VALUES ('plain-scoped', 'incarnation.get')`)
	if err := issue(); !errors.Is(err, ErrOwnerLacksPermissions) {
		t.Fatalf("plain role with a default_scope: err = %v, want ErrOwnerLacksPermissions", err)
	}

	// The operator's rights through a Synod count on its side too: a wider right
	// it holds that way must be covered as well.
	seedSynod(t, "machines", "dba-sd")
	execOrFatal(t, `INSERT INTO rbac_roles (name, description) VALUES ('svc-list', '')`)
	execOrFatal(t, `INSERT INTO rbac_role_permissions (role_name, permission) VALUES ('svc-list', 'service.list')`)
	execOrFatal(t, `INSERT INTO synod_roles (synod_name, role_name) VALUES ('machines', 'svc-list')`)
	grantRole(t, "get-all", "alice", "incarnation.get")
	if err := issue(); !errors.Is(err, ErrOwnerLacksPermissions) || !strings.Contains(err.Error(), "service.list") {
		t.Fatalf("operator's Synod right: err = %v, want ErrOwnerLacksPermissions naming service.list", err)
	}

	// Through a Synod on the owner's side, it covers.
	seedSynod(t, "people", "alice")
	execOrFatal(t, `INSERT INTO synod_roles (synod_name, role_name) VALUES ('people', 'svc-list')`)
	if err := issue(); err != nil {
		t.Fatalf("an owner covering through roles and a Synod: %v", err)
	}
}

func TestIntegration_Owner_RevokedOwnedOperatorIsConflict(t *testing.T) {
	resetOperators(t)
	ctx := context.Background()
	seedActiveOperator(t, "dba-sd", nil)
	seedPerson(t, "alice", AuthMethodLocal)
	s := ownerService(t)
	if _, err := s.SetOwner(ctx, SetOwnerInput{AID: "dba-sd", Owner: Owner{AID: "alice"}}); err != nil {
		t.Fatalf("SetOwner: %v", err)
	}
	if err := Revoke(ctx, integrationPool, "dba-sd", ""); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	if _, err := s.IssueOwnedJWT(ctx, IssueOwnedJWTInput{AID: "dba-sd", CallerAID: "alice"}); !errors.Is(err, ErrOperatorAlreadyRevoked) {
		t.Fatalf("issue for a revoked owned operator: err = %v, want ErrOperatorAlreadyRevoked", err)
	}
	if _, err := s.SetOwner(ctx, SetOwnerInput{AID: "dba-sd", Owner: Owner{AID: "alice"}}); !errors.Is(err, ErrOperatorAlreadyRevoked) {
		t.Fatalf("set the owner of a revoked operator: err = %v, want ErrOperatorAlreadyRevoked", err)
	}
	if _, err := s.SetOwner(ctx, SetOwnerInput{AID: "dba-sd"}); err != nil {
		t.Fatalf("clearing the owner of a revoked operator must work, or its Synod can never be deleted: %v", err)
	}
}

// An owner's issue and a change of owner take the operator's token lock: while
// another transaction holds it, both wait.
func TestIntegration_Owner_IssueAndSetOwnerTakeTheTokenLock(t *testing.T) {
	resetOperators(t)
	ctx := context.Background()
	seedActiveOperator(t, "dba-sd", nil)
	seedPerson(t, "alice", AuthMethodLocal)
	s := ownerService(t)
	if _, err := s.SetOwner(ctx, SetOwnerInput{AID: "dba-sd", Owner: Owner{AID: "alice"}}); err != nil {
		t.Fatalf("SetOwner: %v", err)
	}
	holder, err := integrationPool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer func() { _ = holder.Rollback(ctx) }()
	if err := lockOperatorTokens(ctx, holder, "dba-sd"); err != nil {
		t.Fatalf("hold the lock: %v", err)
	}
	for name, call := range map[string]func(context.Context) error{
		"IssueOwnedJWT": func(c context.Context) error {
			_, err := s.IssueOwnedJWT(c, IssueOwnedJWTInput{AID: "dba-sd", CallerAID: "alice"})
			return err
		},
		"SetOwner": func(c context.Context) error {
			_, err := s.SetOwner(c, SetOwnerInput{AID: "dba-sd"})
			return err
		},
		"RevokeOwnedJWT": func(c context.Context) error {
			_, err := s.RevokeOwnedJWT(c, "alice", "dba-sd", "01JTESTJT1000000000000000A")
			return err
		},
	} {
		c, cancel := context.WithTimeout(ctx, 300*time.Millisecond)
		err := call(c)
		cancel()
		if !errors.Is(err, context.DeadlineExceeded) && !strings.Contains(fmt.Sprint(err), "canceling statement") {
			t.Errorf("%s did not wait for the token lock: err = %v", name, err)
		}
	}
}

// The issue takes the token lock before it reads who owns the operator: an
// issue waiting behind a change of owner then reads the new owner, not the
// one it started under.
func TestIntegration_Owner_IssueWaitingBehindAChangeReadsTheNewOwner(t *testing.T) {
	resetOperators(t)
	ctx := context.Background()
	seedActiveOperator(t, "dba-sd", nil)
	seedPerson(t, "alice", AuthMethodLocal)
	seedPerson(t, "bob", AuthMethodLocal)
	grantClusterAdmin(t, "alice")
	s := ownerService(t)
	if _, err := s.SetOwner(ctx, SetOwnerInput{AID: "dba-sd", Owner: Owner{AID: "alice"}}); err != nil {
		t.Fatalf("SetOwner: %v", err)
	}
	holder, err := integrationPool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer func() { _ = holder.Rollback(ctx) }()
	if err := lockOperatorTokens(ctx, holder, "dba-sd"); err != nil {
		t.Fatalf("hold the lock: %v", err)
	}
	done := make(chan error, 1)
	go func() {
		_, err := s.IssueOwnedJWT(ctx, IssueOwnedJWTInput{AID: "dba-sd", CallerAID: "alice"})
		done <- err
	}()
	// The issue must already be queued on the token lock before the owner
	// changes; a sleep could let it start after the commit and pass for the
	// wrong reason.
	deadline := time.Now().Add(10 * time.Second)
	for {
		var waiting bool
		if err := integrationPool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM pg_locks WHERE locktype = 'advisory' AND NOT granted)`).Scan(&waiting); err != nil {
			t.Fatalf("read pg_locks: %v", err)
		}
		if waiting {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the issue never queued on the token lock")
		}
		time.Sleep(20 * time.Millisecond)
	}
	if _, err := holder.Exec(ctx, `UPDATE operators SET owner_aid = 'bob' WHERE aid = 'dba-sd'`); err != nil {
		t.Fatalf("change the owner: %v", err)
	}
	if err := holder.Commit(ctx); err != nil {
		t.Fatalf("commit: %v", err)
	}
	if err := <-done; !errors.Is(err, ErrOperatorNotFound) {
		t.Fatalf("an issue that waited out the change: err = %v, want ErrOperatorNotFound (alice no longer owns it)", err)
	}
}

// PUT /v1/operators/{aid}/owner answers for the target before the owner: an
// unknown, revoked or non-machine operator says so whatever owner is named.
func TestIntegration_Owner_TargetRefusalsComeFirst(t *testing.T) {
	resetOperators(t)
	ctx := context.Background()
	seedActiveOperator(t, "gone-sa", nil)
	seedActiveOperator(t, "other-sa", nil)
	seedPerson(t, "lucy", AuthMethodLocal)
	if err := Revoke(ctx, integrationPool, "gone-sa", ""); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	s := ownerService(t)
	bad := Owner{AID: "other-sa"}
	for aid, want := range map[string]error{"nobody": ErrOperatorNotFound, "gone-sa": ErrOperatorAlreadyRevoked, "lucy": ErrOwnerNotAllowed} {
		if _, err := s.SetOwner(ctx, SetOwnerInput{AID: aid, Owner: bad}); !errors.Is(err, want) {
			t.Errorf("%s with a bad owner: err = %v, want %v", aid, err, want)
		}
	}
}

// The operator's side is read resolved too: an operator whose only right comes
// through a derived role capped at service=dragonfly holds it there only, so an
// owner holding exactly that covers it.
func TestIntegration_Owner_OperatorsDerivedRoleIsReadUnderItsCeiling(t *testing.T) {
	resetOperators(t)
	ctx := context.Background()
	seedActiveOperator(t, "dba-sd", nil)
	seedPerson(t, "alice", AuthMethodLocal)
	s := ownerService(t)
	if _, err := s.SetOwner(ctx, SetOwnerInput{AID: "dba-sd", Owner: Owner{AID: "alice"}}); err != nil {
		t.Fatalf("SetOwner: %v", err)
	}
	execOrFatal(t, `INSERT INTO rbac_roles (name, description, default_scope) VALUES ('dfly-ops', '', 'service=dragonfly')`)
	execOrFatal(t, `INSERT INTO rbac_role_permissions (role_name, permission) VALUES ('dfly-ops', 'incarnation.get')`)
	execOrFatal(t, `INSERT INTO rbac_roles (name, description, parent_role, scope_mode) VALUES ('dfly-view', '', 'dfly-ops', 'track')`)
	execOrFatal(t, `INSERT INTO rbac_role_permissions (role_name, permission) VALUES ('dfly-view', 'incarnation.get')`)
	execOrFatal(t, `INSERT INTO rbac_role_operators (role_name, aid) VALUES ('dfly-view', 'dba-sd')`)
	// Negative control: the derived role does resolve to a right the owner
	// must hold — an owner with nothing is refused, naming it.
	_, err := s.IssueOwnedJWT(ctx, IssueOwnedJWTInput{AID: "dba-sd", CallerAID: "alice"})
	if !errors.Is(err, ErrOwnerLacksPermissions) || !strings.Contains(err.Error(), "incarnation.get on service=dragonfly") {
		t.Fatalf("owner with nothing: err = %v, want ErrOwnerLacksPermissions naming incarnation.get on service=dragonfly", err)
	}
	grantRole(t, "dfly-reader", "alice", "incarnation.get on service=dragonfly")
	if _, err := s.IssueOwnedJWT(ctx, IssueOwnedJWTInput{AID: "dba-sd", CallerAID: "alice"}); err != nil {
		t.Fatalf("an owner holding exactly the operator's ceilinged right: %v", err)
	}
}
