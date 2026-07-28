package rbac

import (
	"context"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// Guard tests for the catalog-visibility rule (NIM-202): `role.list` answers
// with the roles the caller could grant, not with the cluster's whole privilege
// map. The fixture mirrors the live stand the leak was found on — a `dba`
// operator who held two roles and was served all nine.

// catalogPool serves the four reads ListRoles issues: the three catalog SELECTs
// of [LoadRoleViews] plus the caller's own permissions ([selectAIDPermissionsSQL]).
type catalogPool struct {
	roles      []roleViewRow
	perms      []rolePermRow
	membership []membershipRow

	// callerPerms — what the caller holds, as (permission, role default_scope)
	// pairs; the same shape the least-privilege gate reads for a mutation.
	callerPerms []callerPermRow
}

type roleViewRow struct {
	name, description string
	builtin           bool
	defaultScope      string // "" → NULL
	parentRole        string // "" → NULL
}

type callerPermRow struct {
	permission   string
	defaultScope string // "" → NULL
}

func (p *catalogPool) Exec(context.Context, string, ...any) (pgconn.CommandTag, error) {
	return pgconn.CommandTag{}, errors.New("catalogPool.Exec: not expected")
}

func (p *catalogPool) BeginTx(context.Context, pgx.TxOptions) (pgx.Tx, error) {
	return nil, errors.New("catalogPool.BeginTx: not expected (ListRoles is read-only)")
}

func (p *catalogPool) Query(_ context.Context, sql string, _ ...any) (pgx.Rows, error) {
	switch {
	// FIRST: the caller-permission join also reads rbac_role_permissions, so it
	// has to match before the plain catalog SELECT below.
	case contains(sql, "SELECT rp.permission"):
		return &callerPermRows{values: p.callerPerms}, nil
	case contains(sql, "FROM rbac_roles"):
		return &catalogRoleRows{values: p.roles}, nil
	case contains(sql, "FROM rbac_role_permissions"):
		return &snapPermRows{values: p.perms}, nil
	case contains(sql, "FROM rbac_role_operators"):
		return &snapMembershipRows{values: p.membership}, nil
	}
	return nil, errors.New("catalogPool.Query: unexpected SQL: " + sql)
}

// standCatalog is the fixture: a cluster-admin, a scoped `dba` team role, a role
// derived from it, an unrelated team, and a role that grants nothing.
func standCatalog() *catalogPool {
	return &catalogPool{
		roles: []roleViewRow{
			{name: "cluster-admin", description: "cluster admins", builtin: true},
			{name: "dba", description: "dba team", defaultScope: "coven=dba"},
			{name: "dba-aboba", description: "dba, project aboba", defaultScope: "trait.project=aboba", parentRole: "dba"},
			{name: "empty", description: "grants nothing"},
			{name: "payments", description: "payments team", defaultScope: "coven=payments"},
		},
		perms: []rolePermRow{
			{"cluster-admin", "*"},
			{"dba", "incarnation.get"},
			{"dba", "incarnation.run"},
			{"dba-aboba", "incarnation.get"},
			{"payments", "incarnation.get"},
		},
		membership: []membershipRow{
			{"cluster-admin", "archon-alice"},
			{"dba", "archon-dba"},
			{"payments", "archon-eve"},
		},
	}
}

func roleNames(views []RoleView) []string {
	out := make([]string, 0, len(views))
	for _, v := range views {
		out = append(out, v.Name)
	}
	return out
}

func listRoles(t *testing.T, pool *catalogPool, callerAID string) []RoleView {
	t.Helper()
	svc, err := NewService(ServiceDeps{Pool: pool})
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	views, err := svc.ListRoles(context.Background(), callerAID)
	if err != nil {
		t.Fatalf("ListRoles: %v", err)
	}
	return views
}

// A scoped operator sees their own role, what is derived from it, and roles that
// grant nothing — never another team's role, and never the cluster-admin row.
func TestListRoles_ScopedCallerSeesOnlyWhatItCouldGrant(t *testing.T) {
	pool := standCatalog()
	pool.callerPerms = []callerPermRow{
		{"incarnation.get", "coven=dba"},
		{"incarnation.run", "coven=dba"},
	}

	got := roleNames(listRoles(t, pool, "archon-dba"))
	want := []string{"dba", "dba-aboba", "empty"}
	if !equalStrings(got, want) {
		t.Fatalf("visible roles = %v, want %v", got, want)
	}
}

// The leak this ticket is about, stated as an assertion: the cluster-admin row
// carries `*` AND the AID holding it — a ready-made target. A scoped caller must
// not receive either.
func TestListRoles_ScopedCallerNeverSeesClusterAdminOrItsOperators(t *testing.T) {
	pool := standCatalog()
	pool.callerPerms = []callerPermRow{{"incarnation.get", "coven=dba"}}

	for _, v := range listRoles(t, pool, "archon-dba") {
		if v.Name == "cluster-admin" {
			t.Fatalf("cluster-admin is visible to a scoped caller: %+v", v)
		}
		for _, aid := range v.Operators {
			if aid == "archon-alice" {
				t.Fatalf("role %q leaks cluster-admin's operator %q", v.Name, aid)
			}
		}
	}
}

// An unrestricted `*` covers every role, so the administrator's view is
// unchanged — the filter must not cost a cluster-admin anything.
func TestListRoles_ClusterAdminSeesWholeCatalog(t *testing.T) {
	pool := standCatalog()
	pool.callerPerms = []callerPermRow{{permission: "*"}}

	got := roleNames(listRoles(t, pool, "archon-alice"))
	want := []string{"cluster-admin", "dba", "dba-aboba", "empty", "payments"}
	if !equalStrings(got, want) {
		t.Fatalf("visible roles = %v, want the whole catalog %v", got, want)
	}
}

// A scoped `* on <expr>` is a bounded super-admin (NIM-128): it must not open
// the whole catalog the way a bare `*` does.
func TestListRoles_ScopedWildcardCallerDoesNotSeeUnrestrictedRoles(t *testing.T) {
	pool := standCatalog()
	pool.callerPerms = []callerPermRow{{permission: "* on coven=dba"}}

	for _, v := range roleNames(listRoles(t, pool, "archon-dba")) {
		if v == "cluster-admin" || v == "payments" {
			t.Fatalf("scoped wildcard caller sees %q", v)
		}
	}
}

// An operator holding nothing sees no roles that grant anything — the default is
// deny here as everywhere else. Roles that grant nothing stay visible: they carry
// no privilege, and any caller could create the same empty role.
func TestListRoles_CallerWithoutRightsSeesNoGrantingRole(t *testing.T) {
	pool := standCatalog()
	pool.callerPerms = nil

	got := roleNames(listRoles(t, pool, "archon-nobody"))
	want := []string{"empty"}
	if !equalStrings(got, want) {
		t.Fatalf("visible roles = %v, want %v", got, want)
	}
}

// `role.list-all` (NIM-203) opens the whole catalog to a reader who holds
// nothing of it — the auditor case, which no coverage rule can express.
func TestListRoles_ListAllRightOpensTheWholeCatalog(t *testing.T) {
	pool := standCatalog()
	pool.callerPerms = []callerPermRow{{permission: "role.list"}, {permission: "role.list-all"}}

	got := roleNames(listRoles(t, pool, "archon-auditor"))
	want := []string{"cluster-admin", "dba", "dba-aboba", "empty", "payments"}
	if !equalStrings(got, want) {
		t.Fatalf("visible roles = %v, want the whole catalog %v", got, want)
	}
}

// A resource wildcard covers the new action like any other (`role.*`), so an
// existing RBAC administrator needs no re-grant.
func TestListRoles_ResourceWildcardCoversListAll(t *testing.T) {
	pool := standCatalog()
	pool.callerPerms = []callerPermRow{{permission: "role.*"}}

	if got := roleNames(listRoles(t, pool, "archon-rbac")); len(got) != 5 {
		t.Fatalf("visible roles = %v, want the whole catalog", got)
	}
}

// Scoping the right is meaningless — the grammar has no `role=` dimension — so
// a scoped holder gets the ordinary filtered view, never the full catalog.
func TestListRoles_ScopedListAllGrantsNoFullCatalog(t *testing.T) {
	pool := standCatalog()
	pool.callerPerms = []callerPermRow{
		{"incarnation.get", "coven=dba"},
		{"incarnation.run", "coven=dba"},
		{permission: "role.list-all", defaultScope: "coven=dba"},
	}

	got := roleNames(listRoles(t, pool, "archon-dba"))
	want := []string{"dba", "dba-aboba", "empty"}
	if !equalStrings(got, want) {
		t.Fatalf("visible roles = %v, want the filtered view %v", got, want)
	}
}

// No caller means no basis to filter against, and an unfiltered catalog is the
// leak itself — refuse instead of falling back to "everything".
func TestListRoles_MissingCallerRefused(t *testing.T) {
	svc, err := NewService(ServiceDeps{Pool: standCatalog()})
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	if _, err := svc.ListRoles(context.Background(), ""); !errors.Is(err, ErrPermissionNotHeld) {
		t.Fatalf("ListRoles(no caller) error = %v, want ErrPermissionNotHeld", err)
	}
}

// Visibility is decided on the EFFECTIVE form, not the stored rows: a derived
// role whose stored row its parent no longer covers grants nothing through that
// row, and must not be hidden for carrying it (ADR-078(c)).
func TestListRoles_DerivedRoleJudgedByItsEffectiveForm(t *testing.T) {
	pool := &catalogPool{
		roles: []roleViewRow{ // ORDER BY name, as the catalog SELECT returns them
			{name: "child", parentRole: "parent"},
			{name: "parent", defaultScope: "coven=dba"},
		},
		perms: []rolePermRow{
			{"parent", "incarnation.get"},
			{"child", "incarnation.get"},
			// Stored but no longer covered by the parent → drops out of the
			// effective set, so it must not weigh on visibility either.
			{"child", "incarnation.destroy"},
		},
		callerPerms: []callerPermRow{{"incarnation.get", "coven=dba"}},
	}

	got := roleNames(listRoles(t, pool, "archon-dba"))
	want := []string{"child", "parent"}
	if !equalStrings(got, want) {
		t.Fatalf("visible roles = %v, want %v", got, want)
	}
}

// catalogRoleRows — selectRoleViewsSQL (name, description, builtin,
// default_scope, parent_role); the trailing two are nullable ("" → NULL).
type catalogRoleRows struct {
	values []roleViewRow
	idx    int
}

func (r *catalogRoleRows) Next() bool {
	if r.idx >= len(r.values) {
		return false
	}
	r.idx++
	return true
}

func (r *catalogRoleRows) Scan(dest ...any) error {
	if len(dest) != 5 {
		return errors.New("catalogRoleRows: expected 5 dest")
	}
	row := r.values[r.idx-1]
	*(dest[0].(*string)) = row.name
	*(dest[1].(*string)) = row.description
	*(dest[2].(*bool)) = row.builtin
	assignNullableString(dest[3].(**string), row.defaultScope)
	assignNullableString(dest[4].(**string), row.parentRole)
	return nil
}

func (r *catalogRoleRows) Err() error                                   { return nil }
func (r *catalogRoleRows) Close()                                       {}
func (r *catalogRoleRows) CommandTag() pgconn.CommandTag                { return pgconn.CommandTag{} }
func (r *catalogRoleRows) FieldDescriptions() []pgconn.FieldDescription { return nil }
func (r *catalogRoleRows) Values() ([]any, error)                       { return nil, nil }
func (r *catalogRoleRows) RawValues() [][]byte                          { return nil }
func (r *catalogRoleRows) Conn() *pgx.Conn                              { return nil }

// callerPermRows — selectAIDPermissionsSQL (permission, role default_scope).
type callerPermRows struct {
	values []callerPermRow
	idx    int
}

func (r *callerPermRows) Next() bool {
	if r.idx >= len(r.values) {
		return false
	}
	r.idx++
	return true
}

func (r *callerPermRows) Scan(dest ...any) error {
	if len(dest) != 2 {
		return errors.New("callerPermRows: expected 2 dest")
	}
	row := r.values[r.idx-1]
	*(dest[0].(*string)) = row.permission
	assignNullableString(dest[1].(**string), row.defaultScope)
	return nil
}

func (r *callerPermRows) Err() error                                   { return nil }
func (r *callerPermRows) Close()                                       {}
func (r *callerPermRows) CommandTag() pgconn.CommandTag                { return pgconn.CommandTag{} }
func (r *callerPermRows) FieldDescriptions() []pgconn.FieldDescription { return nil }
func (r *callerPermRows) Values() ([]any, error)                       { return nil, nil }
func (r *callerPermRows) RawValues() [][]byte                          { return nil }
func (r *callerPermRows) Conn() *pgx.Conn                              { return nil }

// assignNullableString writes s into a nullable *string dest, treating "" as NULL.
func assignNullableString(dest **string, s string) {
	if s == "" {
		*dest = nil
		return
	}
	v := s
	*dest = &v
}
