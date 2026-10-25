//go:build integration

// The sync against real Postgres, with the login's real reconciliation
// (auth.DBMapper) and only the directory faked. The unit tests prove the
// decisions; these prove the SQL carries them: the pass clock that makes "one
// more interval" mean an interval and keeps everyone in one pass, the managed domain, the last-admin refusal,
// and that nothing here ever writes operators.revoked_at.

package ldapsync

import (
	"context"
	"errors"
	"io"
	"log"
	"log/slog"
	"os"
	"slices"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"

	"github.com/souls-guild/soul-stack/keeper/internal/auth"
	"github.com/souls-guild/soul-stack/keeper/internal/integrationenv"
	"github.com/souls-guild/soul-stack/keeper/internal/migrate"
	"github.com/souls-guild/soul-stack/keeper/internal/operator"
	"github.com/souls-guild/soul-stack/keeper/internal/rbac"
	"github.com/souls-guild/soul-stack/keeper/migrations"
)

var integrationPool *pgxpool.Pool

func TestMain(m *testing.M) { os.Exit(run(m)) }

func run(m *testing.M) int {
	ctx, cancel := integrationenv.SetupContext()
	defer cancel()

	ctr, err := integrationenv.Start(ctx, "postgres", func(ctx context.Context) (*tcpostgres.PostgresContainer, error) {
		return tcpostgres.Run(ctx,
			"postgres:16-alpine",
			tcpostgres.WithDatabase("keeper_test"),
			tcpostgres.WithUsername("keeper"),
			tcpostgres.WithPassword("keeper"),
			tcpostgres.BasicWaitStrategies(),
		)
	})
	if err != nil {
		if requireDocker() {
			log.Fatalf("ldapsync integration: setup failed (REQUIRE_DOCKER): %v", err)
		}
		log.Printf("ldapsync integration: skipping, docker unavailable: %v", err)
		return 0
	}
	defer func() {
		termCtx, termCancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer termCancel()
		_ = ctr.Terminate(termCtx)
	}()

	dsn, err := ctr.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		log.Printf("ldapsync integration: ConnectionString: %v", err)
		return 1
	}
	if err := migrate.Apply(ctx, dsn, migrations.FS, "."); err != nil {
		log.Printf("ldapsync integration: migrate.Apply: %v", err)
		return 1
	}
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		log.Printf("ldapsync integration: pgxpool.New: %v", err)
		return 1
	}
	defer pool.Close()
	integrationPool = pool
	return m.Run()
}

// seed resets the registry to: archon-root (bootstrap, cluster-admin outside
// any group map) and the given LDAP operators, each holding cluster-admin,
// operator and manual-extra. group_role_map owns {cluster-admin, operator};
// manual-extra stands for a Synod or hand grant the sync must never touch.
func seed(t *testing.T, ldapAIDs ...string) {
	t.Helper()
	ctx := context.Background()
	for _, q := range []string{
		`TRUNCATE TABLE operators, audit_log CASCADE`,
		`INSERT INTO rbac_roles (name, description, builtin) VALUES
		   ('cluster-admin', 'full access', true), ('operator', 'ops', false), ('manual-extra', 'hand grant', false)
		 ON CONFLICT (name) DO NOTHING`,
		`INSERT INTO rbac_role_permissions (role_name, permission) VALUES
		   ('cluster-admin', '*'), ('operator', 'soul.list'), ('manual-extra', 'soul.list')
		 ON CONFLICT (role_name, permission) DO NOTHING`,
	} {
		if _, err := integrationPool.Exec(ctx, q); err != nil {
			t.Fatalf("seed %q: %v", q, err)
		}
	}
	if err := operator.Insert(ctx, integrationPool, &operator.Operator{
		AID: "archon-root", DisplayName: "root", AuthMethod: operator.AuthMethodJWT, CreatedVia: operator.CreatedViaBootstrap,
	}); err != nil {
		t.Fatalf("insert root: %v", err)
	}
	grant(t, "archon-root", "cluster-admin")
	for _, aid := range ldapAIDs {
		if err := operator.Insert(ctx, integrationPool, &operator.Operator{
			AID: aid, DisplayName: aid, AuthMethod: operator.AuthMethodLDAP, CreatedVia: string(operator.CreatedViaLDAP),
		}); err != nil {
			t.Fatalf("insert %s: %v", aid, err)
		}
		grant(t, aid, "cluster-admin", "operator", "manual-extra")
	}
}

func grant(t *testing.T, aid string, roles ...string) {
	t.Helper()
	for _, r := range roles {
		if err := rbac.GrantOperator(context.Background(), integrationPool, r, aid, nil); err != nil {
			t.Fatalf("grant %s to %s: %v", r, aid, err)
		}
	}
}

func rolesOf(t *testing.T, aid string) []string {
	t.Helper()
	got, err := rbac.DirectRolesOf(context.Background(), integrationPool, aid)
	if err != nil {
		t.Fatalf("DirectRolesOf(%s): %v", aid, err)
	}
	slices.Sort(got)
	return got
}

// assertNeverRevoked is the ticket's first invariant, read from the column
// itself: whatever the sync decided, no operator it touched is revoked.
func assertNeverRevoked(t *testing.T) {
	t.Helper()
	var n int
	if err := integrationPool.QueryRow(context.Background(),
		`SELECT count(*) FROM operators WHERE revoked_at IS NOT NULL`).Scan(&n); err != nil {
		t.Fatalf("count revoked: %v", err)
	}
	if n != 0 {
		t.Fatalf("%d operator(s) revoked — the sync must never set revoked_at", n)
	}
}

func row(t *testing.T, aid string) (outcome string, failed int, confirmed bool) {
	t.Helper()
	var c *time.Time
	if err := integrationPool.QueryRow(context.Background(),
		`SELECT outcome, failed_runs, confirmed_at FROM ldap_sync_operators WHERE aid = $1`, aid).Scan(&outcome, &failed, &c); err != nil {
		t.Fatalf("read state of %s: %v", aid, err)
	}
	return outcome, failed, c != nil
}

// interval passes: every attempt is pushed back by more than the interval.
func intervalPasses(t *testing.T) {
	t.Helper()
	if _, err := integrationPool.Exec(context.Background(),
		`UPDATE ldap_sync_operators SET attempted_at = attempted_at - interval '16 minutes'`); err != nil {
		t.Fatalf("age attempts: %v", err)
	}
}

func newIntegrationSyncer(t *testing.T, dir *fakeDirectory) *Syncer {
	t.Helper()
	return newIntegrationSyncerMap(t, dir, map[string][]string{"keeper-admins": {"cluster-admin"}, "keeper-ops": {"operator"}})
}

func newIntegrationSyncerMap(t *testing.T, dir *fakeDirectory, groupRoleMap map[string][]string) *Syncer {
	t.Helper()
	mapper := auth.NewMapper(auth.MapperConfig{
		Method:       operator.AuthMethodLDAP,
		GroupRoleMap: groupRoleMap,
		DB:           integrationPool,
		Tx:           integrationPool,
		Audit:        &captureAudit{},
	})
	s, err := NewSyncer(Config{
		Store: PGStore{DB: integrationPool}, Directory: dir, Roles: mapper, Audit: &captureAudit{},
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestIntegration_NotFoundDropsManagedRolesAndRevokesNothing(t *testing.T) {
	seed(t, "alice", "bob")
	dir := &fakeDirectory{groups: map[string][]string{"bob": {"keeper-ops"}}}

	if got := newIntegrationSyncer(t, dir).Run(context.Background()); got != ResultOK {
		t.Fatalf("result = %s, want ok", got)
	}
	if got := rolesOf(t, "alice"); !slices.Equal(got, []string{"manual-extra"}) {
		t.Errorf("alice = %v, want only the hand grant", got)
	}
	if got := rolesOf(t, "bob"); !slices.Equal(got, []string{"manual-extra", "operator"}) {
		t.Errorf("bob = %v, want cluster-admin gone with the group, operator and the hand grant kept", got)
	}
	if o, f, c := row(t, "alice"); o != string(OutcomeNotFound) || f != 0 || !c {
		t.Errorf("alice state = %s/%d/%v, want not_found/0/confirmed", o, f, c)
	}
	assertNeverRevoked(t)
}

// ★ "Unreachable keeps the roles for exactly one more interval", end to end
// through the pass clock: a second pass inside the interval — a new leader's
// immediate tick, a restart — must not count as the second failure.
func TestIntegration_UnreachableKeepsRolesForExactlyOneInterval(t *testing.T) {
	seed(t, "alice")
	dir := &fakeDirectory{openErr: errors.New("dial tcp 10.0.0.5:636: connection refused")}
	s := newIntegrationSyncer(t, dir)
	full := []string{"cluster-admin", "manual-extra", "operator"}

	if got := s.Run(context.Background()); got != ResultUnreachable {
		t.Fatalf("first pass = %s, want unreachable", got)
	}
	if o, f, c := row(t, "alice"); o != string(OutcomeUnreachable) || f != 1 || c {
		t.Fatalf("state after first = %s/%d/%v, want unreachable/1/unconfirmed", o, f, c)
	}
	if got := s.Run(context.Background()); got != ResultIdle {
		t.Fatalf("pass inside the interval = %s, want idle: alice is not due yet", got)
	}
	if got := rolesOf(t, "alice"); !slices.Equal(got, full) {
		t.Fatalf("roles inside the grace interval = %v, want untouched", got)
	}

	intervalPasses(t)
	s.Run(context.Background())
	if got := rolesOf(t, "alice"); !slices.Equal(got, []string{"manual-extra"}) {
		t.Fatalf("roles after the second failure = %v, want only the hand grant", got)
	}
	if _, f, _ := row(t, "alice"); f != 2 {
		t.Errorf("failed_runs = %d, want 2", f)
	}
	assertNeverRevoked(t)

	dir.openErr = nil
	dir.groups = map[string][]string{"alice": {"keeper-admins", "keeper-ops"}}
	intervalPasses(t)
	s.Run(context.Background())
	if got := rolesOf(t, "alice"); !slices.Equal(got, full) {
		t.Errorf("roles after LDAP came back = %v, want them restored", got)
	}
	if o, f, c := row(t, "alice"); o != string(OutcomeSynced) || f != 0 || !c {
		t.Errorf("state after recovery = %s/%d/%v, want synced/0/confirmed", o, f, c)
	}
}

// ★ The admin set: alice is the only `*` holder once root's grant is gone, and
// her groups no longer include the admin group. The login's refusal holds here.
func TestIntegration_LastAdminIsNotStripped(t *testing.T) {
	seed(t, "alice")
	if err := rbac.RevokeOperatorRow(context.Background(), integrationPool, "cluster-admin", "archon-root"); err != nil {
		t.Fatalf("drop root's grant: %v", err)
	}
	dir := &fakeDirectory{groups: map[string][]string{"alice": {"keeper-ops"}}}

	newIntegrationSyncer(t, dir).Run(context.Background())

	if got := rolesOf(t, "alice"); !slices.Equal(got, []string{"cluster-admin", "manual-extra", "operator"}) {
		t.Errorf("alice = %v, want the last administrator untouched", got)
	}
	if o, _, c := row(t, "alice"); o != string(OutcomeLockoutRefused) || !c {
		t.Errorf("state = %s/%v, want lockout_refused/confirmed", o, c)
	}
	assertNeverRevoked(t)
}

func TestIntegration_EveryoneNotFoundChangesNothing(t *testing.T) {
	seed(t, "alice", "bob")
	newIntegrationSyncer(t, &fakeDirectory{groups: map[string][]string{}}).Run(context.Background())

	for _, aid := range []string{"alice", "bob"} {
		if got := rolesOf(t, aid); !slices.Equal(got, []string{"cluster-admin", "manual-extra", "operator"}) {
			t.Errorf("%s = %v, want untouched by a guarded pass", aid, got)
		}
		if o, _, _ := row(t, aid); o != string(OutcomeGuarded) {
			t.Errorf("%s outcome = %s, want guarded", aid, o)
		}
	}
	assertNeverRevoked(t)
}

// ★ A group_role_map value naming a role that no longer exists: granting it
// would trip the foreign key and roll back the revoke in the same
// transaction, so the operator would keep the role it lost — every pass,
// forever. The revoke must land and the missing role be reported.
func TestIntegration_MissingMappedRoleDoesNotBlockTheRevoke(t *testing.T) {
	seed(t, "alice")
	dir := &fakeDirectory{groups: map[string][]string{"alice": {"keeper-ops"}}}
	grm := map[string][]string{"keeper-admins": {"cluster-admin"}, "keeper-ops": {"operator", "viewer-deleted"}}

	if got := newIntegrationSyncerMap(t, dir, grm).Run(context.Background()); got != ResultOK {
		t.Fatalf("result = %s, want ok", got)
	}
	if got := rolesOf(t, "alice"); !slices.Equal(got, []string{"manual-extra", "operator"}) {
		t.Errorf("alice = %v, want cluster-admin revoked despite the missing mapped role", got)
	}
	var errText string
	if err := integrationPool.QueryRow(context.Background(),
		`SELECT error FROM ldap_sync_operators WHERE aid = 'alice'`).Scan(&errText); err != nil || errText != "skipped roles missing from the registry: viewer-deleted" {
		t.Errorf("recorded error = %q, %v; want the missing role named", errText, err)
	}
}

// A service account that cannot read membership finds everyone with no
// groups: the guard holds and nobody is stripped.
func TestIntegration_EveryoneWithoutGroupsChangesNothing(t *testing.T) {
	seed(t, "alice", "bob")
	newIntegrationSyncer(t, &fakeDirectory{groups: map[string][]string{"alice": {}, "bob": {}}}).Run(context.Background())
	for _, aid := range []string{"alice", "bob"} {
		if got := rolesOf(t, aid); !slices.Equal(got, []string{"cluster-admin", "manual-extra", "operator"}) {
			t.Errorf("%s = %v, want untouched by a guarded pass", aid, got)
		}
	}
	assertNeverRevoked(t)
}

// ★ One pass reads everyone. A per-operator due clock let each operator drift
// into a cohort of its own — a newcomer was looked up alone right after its
// login and stayed alone — and a guard over "everyone in the pass" then never
// saw more than one. Here a newcomer waits for the next pass, and that pass
// holds everyone.
func TestIntegration_ANewOperatorJoinsTheNextPass(t *testing.T) {
	seed(t, "alice", "bob")
	dir := &fakeDirectory{groups: map[string][]string{"alice": {"keeper-ops"}, "bob": {"keeper-ops"}, "carol": {"keeper-ops"}}}
	s := newIntegrationSyncer(t, dir)
	s.Run(context.Background())

	if err := operator.Insert(context.Background(), integrationPool, &operator.Operator{
		AID: "carol", DisplayName: "carol", AuthMethod: operator.AuthMethodLDAP, CreatedVia: string(operator.CreatedViaLDAP),
	}); err != nil {
		t.Fatalf("insert carol: %v", err)
	}
	if got := s.Run(context.Background()); got != ResultIdle {
		t.Fatalf("pass right after a newcomer = %s, want idle: it waits for the next pass", got)
	}
	intervalPasses(t)
	due, err := PGStore{DB: integrationPool}.DueOperators(context.Background(), DefaultInterval)
	if err != nil || len(due) != 3 {
		t.Fatalf("due = %v, %v; want all three operators in one pass", due, err)
	}
}

// A revoked LDAP operator is outside the sync: never due, never counted.
func TestIntegration_RevokedOperatorIsNotSynced(t *testing.T) {
	seed(t, "alice")
	if _, err := integrationPool.Exec(context.Background(),
		`UPDATE operators SET revoked_at = NOW() WHERE aid = 'alice'`); err != nil {
		t.Fatalf("revoke alice: %v", err)
	}
	due, err := PGStore{DB: integrationPool}.DueOperators(context.Background(), DefaultInterval)
	if err != nil || len(due) != 0 {
		t.Fatalf("due = %v, %v; want nobody", due, err)
	}
}

func TestIntegration_SettingsAndStatus(t *testing.T) {
	seed(t, "alice", "bob", "carol")
	ctx := context.Background()
	store := PGStore{DB: integrationPool}

	got, set, err := store.LoadSettings(ctx)
	if err != nil || set || got != DefaultSettings() {
		t.Fatalf("absent key = %+v set=%v err=%v, want the default unset", got, set, err)
	}
	if err := store.SaveSettings(ctx, Settings{Interval: time.Hour}, "archon-root"); err != nil {
		t.Fatalf("SaveSettings: %v", err)
	}
	if got, set, err = store.LoadSettings(ctx); err != nil || !set || got.Interval != time.Hour {
		t.Fatalf("saved = %+v set=%v err=%v, want 1h set", got, set, err)
	}
	if _, err := integrationPool.Exec(ctx, `DELETE FROM keeper_settings WHERE key = $1`, SettingsKey); err != nil {
		t.Fatalf("drop key: %v", err)
	}

	dir := &fakeDirectory{
		groups:    map[string][]string{"alice": {"keeper-ops"}, "carol": {"keeper-admins"}},
		lookupErr: map[string]error{"bob": errors.New("timeout")},
	}
	newIntegrationSyncer(t, dir).Run(ctx)

	st, err := store.Status(ctx)
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if st.Operators[OutcomeSynced] != 2 || st.Operators[OutcomeUnreachable] != 1 || st.Pending != 0 ||
		st.LastRunAt == nil || st.LastSuccessAt == nil {
		t.Errorf("status = %+v, want 2 synced, 1 unreachable, nothing pending, both timestamps", st)
	}

	if err := store.Record(ctx, Record{AID: "alice", Outcome: "revoked"}); err == nil {
		t.Error("an outcome outside the domain was stored; the CHECK must refuse it")
	}
}
