package ldapsync

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"slices"
	"testing"
	"time"

	"github.com/souls-guild/soul-stack/keeper/internal/auth"
	keeperldap "github.com/souls-guild/soul-stack/keeper/internal/auth/ldap"
	"github.com/souls-guild/soul-stack/shared/audit"
)

// fakeStore is the sync state in memory. Every operator it knows is due.
type fakeStore struct {
	settings Settings
	failed   map[string]int
	records  map[string]Record
}

func newFakeStore(aids ...string) *fakeStore {
	s := &fakeStore{settings: DefaultSettings(), failed: map[string]int{}, records: map[string]Record{}}
	for _, a := range aids {
		s.failed[a] = 0
	}
	return s
}

func (s *fakeStore) LoadSettings(context.Context) (Settings, bool, error) {
	return s.settings, true, nil
}

func (s *fakeStore) DueOperators(_ context.Context, _ time.Duration) ([]DueOperator, error) {
	var out []DueOperator
	for aid, f := range s.failed {
		out = append(out, DueOperator{AID: aid, FailedRuns: f})
	}
	slices.SortFunc(out, func(a, b DueOperator) int {
		if a.AID < b.AID {
			return -1
		}
		return 1
	})
	return out, nil
}

func (s *fakeStore) Record(_ context.Context, r Record) error {
	s.records[r.AID] = r
	s.failed[r.AID] = r.FailedRuns
	return nil
}

// fakeDirectory answers from a map: absent AID → not found; openErr → the
// whole directory is unreachable; lookupErr → that AID cannot be read.
type fakeDirectory struct {
	groups    map[string][]string
	lookupErr map[string]error
	openErr   error
}

func (d *fakeDirectory) OpenDirectory(context.Context) (Directory, error) {
	if d.openErr != nil {
		return nil, d.openErr
	}
	return d, nil
}

func (d *fakeDirectory) Groups(aid string) ([]string, error) {
	if err := d.lookupErr[aid]; err != nil {
		return nil, err
	}
	g, ok := d.groups[aid]
	if !ok {
		return nil, keeperldap.ErrUserNotFound
	}
	return g, nil
}

func (d *fakeDirectory) Close() error { return nil }

// fakeRoles mirrors DBMapper.ReconcileRoles over an in-memory membership,
// managed domain included, plus the last-admin refusal for `cluster-admin`.
type fakeRoles struct {
	groupRoles map[string][]string
	managed    map[string]bool
	member     map[string][]string
	// lastAdmin refuses revoking cluster-admin from anyone when only one holds it.
	lastAdmin bool
	// reconcileErr fails every reconciliation; missing is reported as skipped.
	reconcileErr error
	missing      []string
	// deleted — roles the registry no longer holds; heldErr fails ManagedRolesOf.
	deleted map[string]bool
	heldErr error
	calls   map[string][][]string
}

func (f *fakeRoles) ExistingRoles(_ context.Context, names []string) (map[string]bool, error) {
	out := map[string]bool{}
	for _, n := range names {
		if !f.deleted[n] {
			out[n] = true
		}
	}
	return out, nil
}

func (f *fakeRoles) ManagedRolesOf(_ context.Context, aid string) ([]string, error) {
	if f.heldErr != nil {
		return nil, f.heldErr
	}
	var out []string
	for _, r := range f.member[aid] {
		if f.managed[r] {
			out = append(out, r)
		}
	}
	return out, nil
}

func newFakeRoles() *fakeRoles {
	return &fakeRoles{
		groupRoles: map[string][]string{"keeper-admins": {"cluster-admin"}, "keeper-ops": {"operator"}},
		managed:    map[string]bool{"cluster-admin": true, "operator": true},
		member:     map[string][]string{},
		calls:      map[string][][]string{},
	}
}

func (f *fakeRoles) RolesForGroups(groups []string) []string {
	var out []string
	for _, g := range groups {
		out = append(out, f.groupRoles[g]...)
	}
	slices.Sort(out)
	return slices.Compact(out)
}

func (f *fakeRoles) ReconcileRoles(_ context.Context, aid string, want []string) (auth.RoleChange, error) {
	f.calls[aid] = append(f.calls[aid], want)
	if f.reconcileErr != nil {
		return auth.RoleChange{}, f.reconcileErr
	}
	change := auth.RoleChange{Missing: f.missing}
	var next []string
	for _, r := range f.member[aid] {
		if f.managed[r] && !slices.Contains(want, r) {
			if r == "cluster-admin" && f.lastAdmin && f.admins() == 1 {
				return auth.RoleChange{}, auth.ErrReconcileWouldLockOutCluster
			}
			change.Revoked = append(change.Revoked, r)
			continue
		}
		next = append(next, r)
	}
	for _, r := range want {
		if !slices.Contains(next, r) {
			next = append(next, r)
			change.Granted = append(change.Granted, r)
		}
	}
	f.member[aid] = next
	return change, nil
}

func (f *fakeRoles) admins() int {
	n := 0
	for _, roles := range f.member {
		if slices.Contains(roles, "cluster-admin") {
			n++
		}
	}
	return n
}

type captureAudit struct{ events []*audit.Event }

func (c *captureAudit) Write(_ context.Context, ev *audit.Event) error {
	c.events = append(c.events, ev)
	return nil
}

func newTestSyncer(t *testing.T, st *fakeStore, dir *fakeDirectory, roles *fakeRoles, au *captureAudit) *Syncer {
	t.Helper()
	s, err := NewSyncer(Config{
		Store: st, Directory: dir, Roles: roles, Audit: au,
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestSync_GroupChangeRevokesAndGrants(t *testing.T) {
	st := newFakeStore("alice")
	roles := newFakeRoles()
	roles.member["alice"] = []string{"cluster-admin", "synod-role"}
	au := &captureAudit{}
	dir := &fakeDirectory{groups: map[string][]string{"alice": {"keeper-ops"}}}

	if got := newTestSyncer(t, st, dir, roles, au).Run(context.Background()); got != ResultOK {
		t.Fatalf("result = %s, want ok", got)
	}
	if got := roles.member["alice"]; !slices.Equal(got, []string{"synod-role", "operator"}) {
		t.Errorf("membership = %v, want the role outside group_role_map kept and operator granted", got)
	}
	if r := st.records["alice"]; r.Outcome != OutcomeSynced || !r.Confirmed || r.FailedRuns != 0 {
		t.Errorf("record = %+v, want synced/confirmed/0", r)
	}
	if len(au.events) != 1 || au.events[0].EventType != audit.EventOperatorRolesSynced || au.events[0].Source != audit.SourceKeeperInternal {
		t.Fatalf("audit = %+v, want one operator.roles-synced from keeper_internal", au.events)
	}
	p := au.events[0].Payload
	if p["reason"] != "groups" || !slices.Equal(p["revoked"].([]string), []string{"cluster-admin"}) || !slices.Equal(p["granted"].([]string), []string{"operator"}) {
		t.Errorf("payload = %v", p)
	}
}

func TestSync_NothingChangedWritesNoAudit(t *testing.T) {
	st := newFakeStore("alice")
	roles := newFakeRoles()
	roles.member["alice"] = []string{"operator"}
	au := &captureAudit{}
	inv := &countInvalidator{}
	dir := &fakeDirectory{groups: map[string][]string{"alice": {"keeper-ops"}}}

	s := newTestSyncer(t, st, dir, roles, au)
	s.cfg.Invalidator = inv
	s.Run(context.Background())
	if len(au.events) != 0 || inv.n != 0 {
		t.Errorf("audit = %d events, invalidations = %d for a pass that changed nothing", len(au.events), inv.n)
	}
}

type countInvalidator struct{ n int }

func (c *countInvalidator) Invalidate(context.Context) { c.n++ }

// A dropped role must stop working at once, not at the next TTL poll of the
// RBAC snapshot: one invalidation per pass that changed anything.
func TestSync_ChangeInvalidatesTheSnapshotOncePerPass(t *testing.T) {
	st := newFakeStore("alice", "bob")
	roles := newFakeRoles()
	roles.member["alice"] = []string{"cluster-admin"}
	roles.member["bob"] = []string{"cluster-admin"}
	inv := &countInvalidator{}
	dir := &fakeDirectory{groups: map[string][]string{"alice": {"keeper-ops"}, "bob": {"keeper-ops"}}}

	s := newTestSyncer(t, st, dir, roles, &captureAudit{})
	s.cfg.Invalidator = inv
	s.Run(context.Background())
	if inv.n != 1 {
		t.Errorf("invalidations = %d, want 1 for a pass that changed two operators", inv.n)
	}
}

// ★ Invariant: "not found" drops the group roles and nothing else. The sync has
// no path that revokes an operator — the only thing it can do to one is
// reconcile its roles, and here that reconciliation is to the empty set.
func TestSync_NotFoundDropsGroupRolesOnly(t *testing.T) {
	st := newFakeStore("alice", "bob")
	roles := newFakeRoles()
	roles.member["alice"] = []string{"operator", "synod-role"}
	roles.member["bob"] = []string{"operator"}
	au := &captureAudit{}
	dir := &fakeDirectory{groups: map[string][]string{"bob": {"keeper-ops"}}}

	newTestSyncer(t, st, dir, roles, au).Run(context.Background())

	if got := roles.member["alice"]; !slices.Equal(got, []string{"synod-role"}) {
		t.Errorf("alice membership = %v, want only the role outside group_role_map", got)
	}
	if calls := roles.calls["alice"]; len(calls) != 1 || len(calls[0]) != 0 {
		t.Errorf("alice reconciled with %v, want one reconciliation to the empty set", calls)
	}
	if r := st.records["alice"]; r.Outcome != OutcomeNotFound || !r.Confirmed || r.FailedRuns != 0 {
		t.Errorf("record = %+v, want not_found/confirmed/0", r)
	}
	if len(au.events) != 1 || au.events[0].Payload["reason"] != "not_found" {
		t.Errorf("audit = %+v, want one not_found event", au.events)
	}
}

// ★ Invariant: LDAP unreachable keeps the roles for exactly one more interval.
// First failed attempt: untouched. Second in a row: dropped. An answer in
// between resets the count.
func TestSync_UnreachableKeepsRolesForExactlyOneInterval(t *testing.T) {
	st := newFakeStore("alice")
	roles := newFakeRoles()
	roles.member["alice"] = []string{"operator"}
	au := &captureAudit{}
	dir := &fakeDirectory{openErr: errors.New("dial tcp: connection refused")}
	s := newTestSyncer(t, st, dir, roles, au)

	if got := s.Run(context.Background()); got != ResultUnreachable {
		t.Fatalf("first pass = %s, want unreachable", got)
	}
	if !slices.Equal(roles.member["alice"], []string{"operator"}) || len(roles.calls["alice"]) != 0 {
		t.Fatalf("first failed attempt touched the roles: %v (calls %v)", roles.member["alice"], roles.calls["alice"])
	}
	if r := st.records["alice"]; r.Outcome != OutcomeUnreachable || r.Confirmed || r.FailedRuns != 1 || r.Error == "" {
		t.Fatalf("record after first = %+v, want unreachable/unconfirmed/1/with error", r)
	}

	s.Run(context.Background())
	if len(roles.member["alice"]) != 0 {
		t.Fatalf("second failed attempt kept the roles: %v", roles.member["alice"])
	}
	if r := st.records["alice"]; r.FailedRuns != 2 {
		t.Errorf("failed_runs = %d, want 2", r.FailedRuns)
	}
	if len(au.events) != 1 || au.events[0].Payload["reason"] != "unreachable" {
		t.Errorf("audit = %+v, want one unreachable event", au.events)
	}

	dir.openErr = nil
	dir.groups = map[string][]string{"alice": {"keeper-ops"}}
	s.Run(context.Background())
	if !slices.Equal(roles.member["alice"], []string{"operator"}) {
		t.Errorf("roles did not come back with the directory: %v", roles.member["alice"])
	}
	if r := st.records["alice"]; r.FailedRuns != 0 || r.Outcome != OutcomeSynced {
		t.Errorf("record after recovery = %+v, want synced/0", r)
	}
}

func TestSync_AnswerBetweenFailuresResetsTheCount(t *testing.T) {
	st := newFakeStore("alice")
	roles := newFakeRoles()
	roles.member["alice"] = []string{"operator"}
	dir := &fakeDirectory{groups: map[string][]string{"alice": {"keeper-ops"}}}
	s := newTestSyncer(t, st, dir, roles, &captureAudit{})

	dir.openErr = errors.New("down")
	s.Run(context.Background())
	dir.openErr = nil
	s.Run(context.Background())
	dir.openErr = errors.New("down")
	s.Run(context.Background())

	if !slices.Equal(roles.member["alice"], []string{"operator"}) {
		t.Errorf("failures separated by an answer dropped the roles: %v", roles.member["alice"])
	}
}

// A per-operator search error is the same "could not read" as an outage, for
// that operator only.
func TestSync_PerOperatorErrorIsUnreachableForThatOperator(t *testing.T) {
	st := newFakeStore("alice", "bob")
	roles := newFakeRoles()
	dir := &fakeDirectory{
		groups:    map[string][]string{"alice": {"keeper-ops"}, "bob": {"keeper-ops"}},
		lookupErr: map[string]error{"bob": errors.New("LDAP Result Code 3 \"Time Limit Exceeded\"")},
	}
	if got := newTestSyncer(t, st, dir, roles, &captureAudit{}).Run(context.Background()); got != ResultPartial {
		t.Fatalf("result = %s, want partial", got)
	}
	if st.records["alice"].Outcome != OutcomeSynced || st.records["bob"].Outcome != OutcomeUnreachable || st.records["bob"].FailedRuns != 1 {
		t.Errorf("records = %+v", st.records)
	}
}

// ★ Invariant: the sync never empties the admin set. Its reconciliation is the
// login's, which refuses; the sync records the refusal and changes nothing.
func TestSync_LastAdminRefusalChangesNothing(t *testing.T) {
	st := newFakeStore("alice")
	roles := newFakeRoles()
	roles.lastAdmin = true
	roles.member["alice"] = []string{"cluster-admin"}
	au := &captureAudit{}
	dir := &fakeDirectory{groups: map[string][]string{"alice": {"keeper-ops"}}}

	newTestSyncer(t, st, dir, roles, au).Run(context.Background())

	if !slices.Equal(roles.member["alice"], []string{"cluster-admin"}) {
		t.Errorf("membership = %v, want the last admin untouched", roles.member["alice"])
	}
	if r := st.records["alice"]; r.Outcome != OutcomeLockoutRefused || !r.Confirmed {
		t.Errorf("record = %+v, want lockout_refused/confirmed", r)
	}
	if len(au.events) != 0 {
		t.Errorf("audit = %d events for a refused reconciliation", len(au.events))
	}
}

// A pass in which everyone is "not found" is a broken lookup until shown
// otherwise: nothing changes and the failure count is left alone.
func TestSync_EveryoneNotFoundIsGuarded(t *testing.T) {
	st := newFakeStore("alice", "bob")
	st.failed["bob"] = 1
	roles := newFakeRoles()
	roles.member["alice"] = []string{"operator"}
	roles.member["bob"] = []string{"cluster-admin"}
	au := &captureAudit{}
	dir := &fakeDirectory{groups: map[string][]string{}}

	if got := newTestSyncer(t, st, dir, roles, au).Run(context.Background()); got != ResultGuarded {
		t.Fatalf("result = %s, want guarded", got)
	}
	if len(roles.calls) != 0 || len(au.events) != 0 {
		t.Errorf("guarded pass reconciled %v / audited %d", roles.calls, len(au.events))
	}
	if r := st.records["bob"]; r.Outcome != OutcomeGuarded || r.FailedRuns != 1 || r.Confirmed {
		t.Errorf("record = %+v, want guarded with failed_runs left at 1", r)
	}
}

// The same broken-lookup signature from the other side: every operator is
// found but carries no mapped group — a group_filter that matches nothing, or
// a service account not allowed to read membership.
func TestSync_EveryoneFoundWithoutMappedGroupsIsGuarded(t *testing.T) {
	st := newFakeStore("alice", "bob")
	roles := newFakeRoles()
	roles.member["alice"] = []string{"operator"}
	roles.member["bob"] = []string{"cluster-admin"}
	dir := &fakeDirectory{groups: map[string][]string{"alice": {}, "bob": {"unmapped"}}}

	if got := newTestSyncer(t, st, dir, roles, &captureAudit{}).Run(context.Background()); got != ResultGuarded {
		t.Fatalf("result = %s, want guarded", got)
	}
	if len(roles.calls) != 0 {
		t.Errorf("guarded pass reconciled %v", roles.calls)
	}
}

// Only operators with something to lose count. Two departed operators already
// stripped must not turn a third one's real departure into a guarded pass.
func TestSync_GuardCountsOnlyOperatorsHoldingGroupRoles(t *testing.T) {
	st := newFakeStore("alice", "bob", "carol")
	roles := newFakeRoles()
	roles.member["alice"] = []string{"operator"}
	dir := &fakeDirectory{groups: map[string][]string{}}

	if got := newTestSyncer(t, st, dir, roles, &captureAudit{}).Run(context.Background()); got != ResultOK {
		t.Fatalf("result = %s, want ok", got)
	}
	if len(roles.member["alice"]) != 0 {
		t.Errorf("alice = %v, want her group roles dropped", roles.member["alice"])
	}
}

// One operator the directory could not answer for must not switch the guard
// off for the rest; it takes the unreachable path on its own.
func TestSync_GuardSurvivesOneUnansweredOperator(t *testing.T) {
	st := newFakeStore("alice", "bob", "carol")
	roles := newFakeRoles()
	roles.member["alice"] = []string{"operator"}
	roles.member["bob"] = []string{"operator"}
	roles.member["carol"] = []string{"operator"}
	dir := &fakeDirectory{groups: map[string][]string{}, lookupErr: map[string]error{"carol": errors.New("timeout")}}

	if got := newTestSyncer(t, st, dir, roles, &captureAudit{}).Run(context.Background()); got != ResultGuarded {
		t.Fatalf("result = %s, want guarded", got)
	}
	if st.records["alice"].Outcome != OutcomeGuarded || st.records["bob"].Outcome != OutcomeGuarded {
		t.Errorf("alice/bob = %s/%s, want guarded", st.records["alice"].Outcome, st.records["bob"].Outcome)
	}
	if r := st.records["carol"]; r.Outcome != OutcomeUnreachable || r.FailedRuns != 1 {
		t.Errorf("carol = %+v, want unreachable/1", r)
	}
	if len(roles.calls) != 0 {
		t.Errorf("reconciled %v in a guarded pass", roles.calls)
	}
}

// A failed read of current roles must not switch the guard off: in doubt the
// operator counts as a holder.
func TestSync_GuardHoldsWhenCurrentRolesCannotBeRead(t *testing.T) {
	st := newFakeStore("alice", "bob")
	roles := newFakeRoles()
	roles.heldErr = errors.New("conn reset")
	dir := &fakeDirectory{groups: map[string][]string{"alice": {}, "bob": {}}}

	if got := newTestSyncer(t, st, dir, roles, &captureAudit{}).Run(context.Background()); got != ResultGuarded {
		t.Fatalf("result = %s, want guarded", got)
	}
}

// Groups that map only to roles deleted from the registry reconcile to
// nothing, so for the guard they strip like no groups at all.
func TestSync_GuardSeesThroughDeletedMappedRoles(t *testing.T) {
	st := newFakeStore("alice", "bob")
	roles := newFakeRoles()
	roles.deleted = map[string]bool{"operator": true}
	roles.member["alice"] = []string{"cluster-admin"}
	roles.member["bob"] = []string{"cluster-admin"}
	dir := &fakeDirectory{groups: map[string][]string{"alice": {"keeper-ops"}, "bob": {"keeper-ops"}}}

	if got := newTestSyncer(t, st, dir, roles, &captureAudit{}).Run(context.Background()); got != ResultGuarded {
		t.Fatalf("result = %s, want guarded", got)
	}
}

// A refusal is not an applied answer: the pass is partial, so the staleness
// alert on the last `ok` pass fires while the refusal persists.
func TestSync_LockoutRefusalMakesThePassPartial(t *testing.T) {
	st := newFakeStore("alice")
	roles := newFakeRoles()
	roles.lastAdmin = true
	roles.member["alice"] = []string{"cluster-admin"}
	dir := &fakeDirectory{groups: map[string][]string{"alice": {"keeper-ops"}}}

	if got := newTestSyncer(t, st, dir, roles, &captureAudit{}).Run(context.Background()); got != ResultPartial {
		t.Fatalf("result = %s, want partial", got)
	}
}

// The last admin whose groups LDAP cannot read twice: the drop is refused and
// the status says so, not "unreachable".
func TestSync_RefusedDropAfterOutageIsRecordedAsLockoutRefused(t *testing.T) {
	st := newFakeStore("alice")
	st.failed["alice"] = 1
	roles := newFakeRoles()
	roles.lastAdmin = true
	roles.member["alice"] = []string{"cluster-admin"}
	dir := &fakeDirectory{openErr: errors.New("down")}

	newTestSyncer(t, st, dir, roles, &captureAudit{}).Run(context.Background())

	if !slices.Equal(roles.member["alice"], []string{"cluster-admin"}) {
		t.Errorf("membership = %v, want the last admin untouched", roles.member["alice"])
	}
	if r := st.records["alice"]; r.Outcome != OutcomeLockoutRefused || r.FailedRuns != 2 || r.Confirmed {
		t.Errorf("record = %+v, want lockout_refused/2/unconfirmed", r)
	}
}

func TestSync_ReconcileFailureIsRecorded(t *testing.T) {
	st := newFakeStore("alice")
	roles := newFakeRoles()
	roles.reconcileErr = errors.New("deadlock detected")
	dir := &fakeDirectory{groups: map[string][]string{"alice": {"keeper-ops"}}}

	if got := newTestSyncer(t, st, dir, roles, &captureAudit{}).Run(context.Background()); got != ResultPartial {
		t.Fatalf("result = %s, want partial", got)
	}
	if r := st.records["alice"]; r.Outcome != OutcomeReconcileFailed || r.Error == "" {
		t.Errorf("record = %+v, want reconcile_failed with the error", r)
	}
}

func TestSync_MissingRolesAreReportedNotFatal(t *testing.T) {
	st := newFakeStore("alice")
	roles := newFakeRoles()
	roles.missing = []string{"viewer"}
	dir := &fakeDirectory{groups: map[string][]string{"alice": {"keeper-ops"}}}

	newTestSyncer(t, st, dir, roles, &captureAudit{}).Run(context.Background())
	if r := st.records["alice"]; r.Outcome != OutcomeSynced || r.Error != "skipped roles missing from the registry: viewer" {
		t.Errorf("record = %+v, want synced with the missing role named", r)
	}
}

// One operator alone is not a pattern: a single "not found" is obeyed.
func TestSync_SingleNotFoundIsNotGuarded(t *testing.T) {
	st := newFakeStore("alice")
	roles := newFakeRoles()
	roles.member["alice"] = []string{"operator"}
	dir := &fakeDirectory{groups: map[string][]string{}}

	if got := newTestSyncer(t, st, dir, roles, &captureAudit{}).Run(context.Background()); got != ResultOK {
		t.Fatalf("result = %s, want ok", got)
	}
	if len(roles.member["alice"]) != 0 {
		t.Errorf("membership = %v, want dropped", roles.member["alice"])
	}
}

func TestSync_NobodyDueIsIdle(t *testing.T) {
	dir := &fakeDirectory{openErr: errors.New("must not be dialled")}
	if got := newTestSyncer(t, newFakeStore(), dir, newFakeRoles(), &captureAudit{}).Run(context.Background()); got != ResultIdle {
		t.Fatalf("result = %s, want idle", got)
	}
}
