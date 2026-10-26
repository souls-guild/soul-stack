package operator

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// ownerPool — the SQL IssueOwnedJWT sends, and nothing else: the token lock, the
// operator row, the caller's token row, the effective permissions of an AID,
// and the operator_jwts insert. Unknown SQL fails the call, so a new query on
// that path is visible here rather than answered by accident.
type ownerPool struct {
	owner      Owner
	callerKind JWTKind
	// perms — an AID's permission strings, held through one plain role named
	// "role-<aid>". holds/roles describe anything else: holds names an AID's
	// roles, roles defines a role (a parent makes it derived).
	perms      map[string][]string
	holds      map[string][]string
	roles      map[string]fakeRole
	jwtInserts [][]any
	committed  bool
}

type fakeRole struct {
	parent string
	scope  string
	perms  []string
}

func (p *ownerPool) role(name string) (fakeRole, bool) {
	if r, ok := p.roles[name]; ok {
		return r, true
	}
	if aid, ok := strings.CutPrefix(name, "role-"); ok {
		if perms, ok := p.perms[aid]; ok {
			return fakeRole{perms: perms}, true
		}
	}
	return fakeRole{}, false
}

type ownerTx struct {
	svcTx
	p *ownerPool
}

func (t *ownerTx) Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	return t.p.Exec(ctx, sql, args...)
}
func (t *ownerTx) QueryRow(ctx context.Context, sql string, args ...any) pgx.Row {
	return t.p.QueryRow(ctx, sql, args...)
}
func (t *ownerTx) Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error) {
	return t.p.Query(ctx, sql, args...)
}
func (t *ownerTx) Commit(context.Context) error   { t.p.committed = true; return nil }
func (t *ownerTx) Rollback(context.Context) error { return nil }

func (p *ownerPool) BeginTx(context.Context, pgx.TxOptions) (pgx.Tx, error) {
	return &ownerTx{p: p}, nil
}

func (p *ownerPool) Exec(_ context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	switch {
	case strings.Contains(sql, "pg_advisory_xact_lock"):
		return pgconn.NewCommandTag("SELECT 1"), nil
	case strings.Contains(sql, "INSERT INTO operator_jwts"):
		p.jwtInserts = append(p.jwtInserts, args)
		return pgconn.NewCommandTag("INSERT 0 1"), nil
	}
	return pgconn.CommandTag{}, errors.New("ownerPool.Exec: unexpected SQL: " + sql)
}

func (p *ownerPool) QueryRow(_ context.Context, sql string, args ...any) pgx.Row {
	switch {
	case strings.Contains(sql, "SELECT "+operatorColumns+" FROM operators WHERE aid"):
		aid, synod := p.owner.columns()
		var ownerAID, ownerSynod any
		if aid != nil {
			ownerAID = *aid
		}
		if synod != nil {
			ownerSynod = *synod
		}
		return staticRow{values: []any{
			args[0].(string), "machine", string(AuthMethodJWT), time.Unix(0, 0).UTC(),
			"archon-admin", CreatedViaUser, nil, []byte("{}"), ownerAID, ownerSynod,
		}}
	case strings.Contains(sql, "FROM operator_jwts WHERE jwt_id"):
		return ownerTokenRow{kind: p.callerKind}
	}
	return errRow{err: errors.New("ownerPool.QueryRow: unexpected SQL: " + sql)}
}

func (p *ownerPool) Query(_ context.Context, sql string, args ...any) (pgx.Rows, error) {
	switch {
	case strings.Contains(sql, "SELECT ro.role_name"):
		aid := args[0].(string)
		names, ok := p.holds[aid]
		if !ok {
			if _, has := p.perms[aid]; has {
				names = []string{"role-" + aid}
			}
		}
		return &fakeRows{values: names}, nil
	case strings.Contains(sql, "WITH RECURSIVE chain"):
		var rows [][4]*string
		for name := args[0].(string); name != ""; {
			r, ok := p.role(name)
			if !ok {
				break
			}
			n := name
			var parent, scope *string
			if r.parent != "" {
				parent = &r.parent
			}
			if r.scope != "" {
				scope = &r.scope
			}
			if len(r.perms) == 0 {
				rows = append(rows, [4]*string{&n, parent, scope, nil})
			}
			for i := range r.perms {
				rows = append(rows, [4]*string{&n, parent, scope, &r.perms[i]})
			}
			name = r.parent
		}
		return &chainRows{rows: rows}, nil
	}
	return nil, errors.New("ownerPool.Query: unexpected SQL: " + sql)
}

// ownerTokenRow — the caller's token as LookupJWT reads it: kind, expiry, revoked.
type ownerTokenRow struct{ kind JWTKind }

func (r ownerTokenRow) Scan(dest ...any) error {
	*dest[0].(*string) = string(r.kind)
	*dest[1].(*time.Time) = time.Now().Add(time.Hour)
	*dest[2].(*bool) = false
	return nil
}

// chainRows — (name, parent_role, default_scope, permission) rows of a role
// chain, as resolveRoleChain reads them.
type chainRows struct {
	fakeRows
	rows [][4]*string
	i    int
}

func (r *chainRows) Next() bool { r.i++; return r.i <= len(r.rows) }
func (r *chainRows) Scan(dest ...any) error {
	row := r.rows[r.i-1]
	*dest[0].(*string) = *row[0]
	*dest[1].(**string) = row[1]
	*dest[2].(**string) = row[2]
	*dest[3].(**string) = row[3]
	return nil
}

func issueOwned(t *testing.T, p *ownerPool) (*IssueTokenResult, error) {
	t.Helper()
	s := newService(t, p, &fakeIssuer{}, &fakeRBAC{roles: []string{"reader"}})
	return s.IssueOwnedJWT(context.Background(), IssueOwnedJWTInput{
		AID:         "dba-sd",
		CallerAID:   "alice",
		CallerJWTID: "01JTESTJT1000000000000000A",
		Label:       "rotation",
	})
}

// The invariant of ADR-014 amendment NIM-1057: owning an operator never lets
// the owner mint a token carrying a right the owner does not hold. Each case is
// an owner the operator's rights are NOT contained in — no row may be written.
func TestIssueOwnedJWT_RefusedWhenTheOwnerDoesNotCoverTheOperator(t *testing.T) {
	for name, ownerPerms := range map[string][]string{
		"owner holds nothing":           nil,
		"owner holds a narrower scope":  {"incarnation.get on service=dragonfly"},
		"owner holds another action":    {"incarnation.list"},
		"owner holds one right of two":  {"incarnation.get"},
		"owner holds a scoped wildcard": {"* on service=dragonfly"},
	} {
		t.Run(name, func(t *testing.T) {
			p := &ownerPool{
				owner:      Owner{AID: "alice"},
				callerKind: JWTKindSession,
				perms: map[string][]string{
					"dba-sd": {"incarnation.get", "service.list"},
					"alice":  ownerPerms,
				},
			}
			_, err := issueOwned(t, p)
			if !errors.Is(err, ErrOwnerLacksPermissions) {
				t.Fatalf("err = %v, want ErrOwnerLacksPermissions", err)
			}
			if len(p.jwtInserts) != 0 || p.committed {
				t.Fatalf("a refused issue wrote %d token rows (committed=%v)", len(p.jwtInserts), p.committed)
			}
		})
	}
}

// A derived role stores only its delta: its rows read raw would hand the owner
// the parent's rights without the parent's ceiling. The owner below holds
// incarnation.get through a child of a role capped at service=dragonfly, so it
// holds it ON service=dragonfly only, and cannot mint for an operator holding
// it unrestricted (the HIGH of the NIM-1057 review).
func TestIssueOwnedJWT_DerivedRoleIsReadUnderItsParentsCeiling(t *testing.T) {
	p := &ownerPool{
		owner:      Owner{AID: "alice"},
		callerKind: JWTKindSession,
		perms:      map[string][]string{"dba-sd": {"incarnation.get"}},
		holds:      map[string][]string{"alice": {"dfly-view"}},
		roles: map[string]fakeRole{
			"dfly-ops":  {scope: "service=dragonfly", perms: []string{"incarnation.get", "incarnation.run"}},
			"dfly-view": {parent: "dfly-ops", perms: []string{"incarnation.get"}},
		},
	}
	if _, err := issueOwned(t, p); !errors.Is(err, ErrOwnerLacksPermissions) {
		t.Fatalf("err = %v, want ErrOwnerLacksPermissions", err)
	}
	if len(p.jwtInserts) != 0 {
		t.Fatalf("a derived role's parent ceiling was skipped: %d token rows", len(p.jwtInserts))
	}

	p.roles["dfly-ops"] = fakeRole{perms: []string{"incarnation.get", "incarnation.run"}}
	if _, err := issueOwned(t, p); err != nil {
		t.Fatalf("an unrestricted parent: %v", err)
	}
}

// The refusal names the first missing right in permission order, not in the
// order the rows came back.
func TestIssueOwnedJWT_RefusalNamesTheFirstMissingRightInOrder(t *testing.T) {
	p := &ownerPool{
		owner:      Owner{AID: "alice"},
		callerKind: JWTKindSession,
		perms:      map[string][]string{"dba-sd": {"service.list", "incarnation.get"}, "alice": nil},
	}
	_, err := issueOwned(t, p)
	if !errors.Is(err, ErrOwnerLacksPermissions) || !strings.HasSuffix(err.Error(), ": incarnation.get") {
		t.Fatalf("err = %v, want the refusal to name incarnation.get", err)
	}
}

func TestIssueOwnedJWT_MintsWhenTheOwnerCoversTheOperator(t *testing.T) {
	for name, ownerPerms := range map[string][]string{
		"the same rights":     {"incarnation.get", "service.list"},
		"a wider resource":    {"incarnation.*", "service.*"},
		"cluster-admin owner": {"*"},
	} {
		t.Run(name, func(t *testing.T) {
			p := &ownerPool{
				owner:      Owner{AID: "alice"},
				callerKind: JWTKindSession,
				perms: map[string][]string{
					"dba-sd": {"incarnation.get", "service.list"},
					"alice":  ownerPerms,
				},
			}
			res, err := issueOwned(t, p)
			if err != nil {
				t.Fatalf("IssueOwnedJWT: %v", err)
			}
			if res.AID != "dba-sd" || res.TTL != time.Hour {
				t.Errorf("result = %+v, want a token of dba-sd for ttl_default", res)
			}
			if len(p.jwtInserts) != 1 || !p.committed {
				t.Fatalf("token rows = %d, committed = %v, want 1 and true", len(p.jwtInserts), p.committed)
			}
		})
	}
}

func TestIssueOwnedJWT_NotTheOwnerIsNotFound(t *testing.T) {
	p := &ownerPool{
		owner:      Owner{AID: "bob"},
		callerKind: JWTKindSession,
		perms:      map[string][]string{"alice": {"*"}},
	}
	if _, err := issueOwned(t, p); !errors.Is(err, ErrOperatorNotFound) {
		t.Fatalf("err = %v, want ErrOperatorNotFound", err)
	}
	if len(p.jwtInserts) != 0 {
		t.Fatalf("a stranger minted %d tokens", len(p.jwtInserts))
	}
}

func TestIssueOwnedJWT_PersonalTokenCannotIssue(t *testing.T) {
	p := &ownerPool{
		owner:      Owner{AID: "alice"},
		callerKind: JWTKindPersonal,
		perms:      map[string][]string{"alice": {"*"}},
	}
	if _, err := issueOwned(t, p); !errors.Is(err, ErrOwnedJWTFromPersonal) {
		t.Fatalf("err = %v, want ErrOwnedJWTFromPersonal", err)
	}
	if len(p.jwtInserts) != 0 {
		t.Fatalf("a personal token minted %d tokens", len(p.jwtInserts))
	}
}
