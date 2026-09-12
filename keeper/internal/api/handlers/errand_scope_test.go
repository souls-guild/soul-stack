package handlers

// Scope guards for the errand read/cancel surface (NIM-841).
//
// What is being defended: `errand.list` used to be all-or-nothing. The route gate
// asked only whether the right was held, no handler on the read path ever saw the
// caller's claims, and `rowToErrandResultView` returned `Stdout`/`Stderr`
// verbatim — so a bare grant read back the output of every command anyone had run
// on any host, and the narrower spelling `errand.list on coven=dev` was denied
// outright by the NoSelector gate.
//
// These tests assert the SHAPE OF THE QUERY, not the rows that come back. That is
// deliberate twice over. The fake pools here replay fixed rows, so an assertion
// on the returned data cannot fail whatever the filter does. And a list narrowed
// in Go rather than in SQL still publishes `total` — the rows disappear and their
// number does not — which is the leak this ticket is actually about.

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/souls-guild/soul-stack/keeper/internal/errand"
)

// --- fixtures ---

// errandScopeRow — the 20 scan columns of one errands row joined onto souls
// (see errand.selectColumns). The last two are the host's coven and traits, the
// dimensions the single-object gate resolves over.
func errandScopeRow(id, sid string, covens []string) []any {
	now := time.Date(2026, 9, 12, 10, 0, 0, 0, time.UTC)
	return []any{
		id, sid, "core.cmd.shell", []byte(nil), string(errand.StatusRunning),
		nil, "root-only secret", "", false, false,
		nil, "", []byte(nil), "archon-alice", "kid-1",
		now, nil, now,
		covens, []byte(nil),
	}
}

// errandRowPool serves ONE errand row by id and records every statement, so a
// test can assert what was asked as well as what came back.
type errandRowPool struct {
	rows map[string][]any

	getSQL  string
	getArgs []any
	queries int
}

func (p *errandRowPool) Exec(context.Context, string, ...any) (pgconn.CommandTag, error) {
	return pgconn.CommandTag{}, errors.New("errandRowPool.Exec: unexpected")
}

func (p *errandRowPool) QueryRow(_ context.Context, sql string, args ...any) pgx.Row {
	p.queries++
	p.getSQL = sql
	p.getArgs = args
	if len(args) == 1 {
		if v, ok := p.rows[args[0].(string)]; ok {
			return staticRow{values: v}
		}
	}
	return staticRow{err: pgx.ErrNoRows}
}

func (p *errandRowPool) Query(context.Context, string, ...any) (pgx.Rows, error) {
	return nil, errors.New("errandRowPool.Query: unexpected")
}

// listErrands drives ListTyped through a recording pool and hands back what the
// store was asked.
func listErrands(t *testing.T, scoper PurviewResolver, in ErrandListInput) *fakeErrandPool {
	t.Helper()
	pool := &fakeErrandPool{}
	h := NewErrandHandler(nil, errand.NewStore(pool), nil, nil, nil, scoper, nil)
	if _, err := h.ListTyped(context.Background(), claimsFor("archon-alice"), in); err != nil {
		t.Fatalf("ListTyped: %v", err)
	}
	return pool
}

// --- list: the predicate reaches the query, and the COUNT with it ---

// TestErrandList_PurviewReachesBothQueries is the counter guard. A coven-scoped
// operator must have the predicate rendered into the COUNT as well as the SELECT:
// a filter applied only to the page would hide the rows of other covens and still
// report how many there are, which discloses the size of the fleet's command
// history one query at a time.
func TestErrandList_PurviewReachesBothQueries(t *testing.T) {
	pool := listErrands(t, fakeScoper{covens: []string{"dev"}}, ErrandListInput{Limit: 50})

	for _, q := range []struct {
		name string
		sql  string
		args []any
	}{
		{"count", pool.countSQL, pool.countArgs},
		{"select", pool.selectSQL, pool.selectArgs},
	} {
		if !strings.Contains(q.sql, "s.coven && $") {
			t.Errorf("%s SQL carries no coven predicate: %q", q.name, q.sql)
		}
		if !strings.Contains(q.sql, "LEFT JOIN souls") {
			t.Errorf("%s SQL does not join souls, so the coven column does not exist: %q", q.name, q.sql)
		}
		if len(q.args) == 0 {
			t.Fatalf("%s args are empty — the predicate was rendered without its bindings", q.name)
		}
		if !reflect.DeepEqual(q.args[0], []string{"dev"}) {
			t.Errorf("%s first binding = %#v, want the purview's coven set []string{\"dev\"}", q.name, q.args[0])
		}
	}
}

// TestErrandList_NoPurviewIsFailClosed — an operator the resolver knows nothing
// about gets FALSE, not an unfiltered query. The assertion is on the SQL because
// the fake returns a fixed count either way: "no rows came back" would be true of
// an unnarrowed query against an empty fixture too.
func TestErrandList_NoPurviewIsFailClosed(t *testing.T) {
	pool := listErrands(t, fakeScoper{empty: true}, ErrandListInput{Limit: 50})

	for name, sql := range map[string]string{"count": pool.countSQL, "select": pool.selectSQL} {
		if !strings.Contains(sql, "FALSE") {
			t.Errorf("%s SQL of an entitled-to-nothing operator must render FALSE: %q", name, sql)
		}
	}
}

// TestErrandList_SIDFilterCannotWidenTheScope — the scope is ANDed after every
// user-supplied filter. An operator naming a host outside their purview must not
// reach it by asking for it directly, which is exactly the call in the ticket's
// failure scenario (`GET /v1/errands?sid=<prod-host>`).
func TestErrandList_SIDFilterCannotWidenTheScope(t *testing.T) {
	pool := listErrands(t, fakeScoper{covens: []string{"dev"}},
		ErrandListInput{SID: "prod-db-01.example.com", Limit: 50})

	sid := strings.Index(pool.countSQL, "e.sid = $")
	coven := strings.Index(pool.countSQL, "s.coven && $")
	if sid < 0 || coven < 0 {
		t.Fatalf("expected both the sid filter and the scope predicate: %q", pool.countSQL)
	}
	if coven < sid {
		t.Errorf("the scope predicate must be ANDed AFTER the sid filter, not before it: %q", pool.countSQL)
	}
	if !strings.Contains(pool.countSQL, " AND ") {
		t.Errorf("filters must be conjoined, never ORed: %q", pool.countSQL)
	}
}

// --- get: out of scope reads exactly like absent ---

// TestErrandGet_OutOfScopeIsIndistinguishableFromAbsent — the SAME id is asked of
// two handlers: one whose store holds the errand (on a host outside the caller's
// purview) and one whose store is empty. Both refusals must match field for
// field.
//
// The id has to be the same one, and the two stores have to differ, or the test
// proves nothing: asking one handler twice compares a value with itself, and
// asking two ids of one handler compares two detail strings that legitimately
// differ because each echoes the id it was given. A 403 here, or a 404 whose
// detail says something else, would confirm that an errand with that id exists
// and turn the route into an oracle over which hosts have been commanded.
func TestErrandGet_OutOfScopeIsIndistinguishableFromAbsent(t *testing.T) {
	const id = "ERR-PROD"
	scoped := func(rows map[string][]any) *ErrandHandler {
		return NewErrandHandler(nil, errand.NewStore(&errandRowPool{rows: rows}), nil, nil, nil,
			fakeScoper{covens: []string{"dev"}}, nil)
	}

	// Exists, on a prod host this dev-scoped operator may not see.
	_, outOfScope := scoped(map[string][]any{
		id: errandScopeRow(id, "prod-db-01", []string{"prod"}),
	}).GetTyped(context.Background(), claimsFor("archon-alice"), id)

	// The same id, no such errand anywhere.
	_, absent := scoped(map[string][]any{}).
		GetTyped(context.Background(), claimsFor("archon-alice"), id)

	got, ok := AsProblemDetails(outOfScope)
	if !ok {
		t.Fatalf("out-of-scope error is not a problem: %T %v", outOfScope, outOfScope)
	}
	want, ok := AsProblemDetails(absent)
	if !ok {
		t.Fatalf("absent error is not a problem: %T %v", absent, absent)
	}
	if !strings.Contains(got.Type, "not-found") {
		t.Fatalf("out-of-scope problem.Type = %q, want the not-found type", got.Type)
	}
	if got.Type != want.Type || got.Detail != want.Detail {
		t.Errorf("the two refusals differ, so the pair is an existence oracle:\n"+
			" out of scope: %s / %q\n absent      : %s / %q",
			got.Type, got.Detail, want.Type, want.Detail)
	}
}

// TestErrandGet_InScopeIsServed is the other half: the narrowing must not deny an
// operator the hosts they ARE entitled to, or the guard above would pass on a
// handler that refuses everything.
func TestErrandGet_InScopeIsServed(t *testing.T) {
	pool := &errandRowPool{rows: map[string][]any{
		"ERR-DEV": errandScopeRow("ERR-DEV", "dev-web-01", []string{"dev"}),
	}}
	h := NewErrandHandler(nil, errand.NewStore(pool), nil, nil, nil,
		fakeScoper{covens: []string{"dev"}}, nil)

	reply, err := h.GetTyped(context.Background(), claimsFor("archon-alice"), "ERR-DEV")
	if err != nil {
		t.Fatalf("GetTyped on an in-scope host: %v", err)
	}
	if !reply.Running {
		t.Errorf("reply.Running = false, want the 202 shape of the fixture's running row")
	}
}

// TestErrandGet_HostScopeMatchesOnTheErrandsOwnSID — the host dimension resolves
// over the sid the ERRAND carries, not the joined souls row, so an errand against
// a host since removed from the registry stays reachable by an operator scoped to
// that host. The fixture gives the row no covens, which is what a LEFT JOIN
// yields for a deleted host.
func TestErrandGet_HostScopeMatchesOnTheErrandsOwnSID(t *testing.T) {
	pool := &errandRowPool{rows: map[string][]any{
		"ERR-GONE": errandScopeRow("ERR-GONE", "retired-01", nil),
	}}
	h := NewErrandHandler(nil, errand.NewStore(pool), nil, nil, nil,
		fakeScoper{exprs: []string{"host=retired-01"}}, nil)

	if _, err := h.GetTyped(context.Background(), claimsFor("archon-alice"), "ERR-GONE"); err != nil {
		t.Fatalf("a host-scoped operator lost the errands of their own decommissioned host: %v", err)
	}
}

// --- cancel: checked before anything is dispatched ---

// TestErrandCancel_OutOfScopeRefusesBeforeDispatch — a holder of `errand.cancel`
// must not be able to abort a command running on a host outside their purview.
// The dispatcher is deliberately nil-free here: reaching it at all would mean the
// scope check ran too late, so the assertion is that the refusal is the
// uniform 404 AND that only the handler's own lookup touched the store.
func TestErrandCancel_OutOfScopeRefusesBeforeDispatch(t *testing.T) {
	pool := &errandRowPool{rows: map[string][]any{
		"ERR-PROD": errandScopeRow("ERR-PROD", "prod-db-01", []string{"prod"}),
	}}
	d := buildCancelDispatcher(t, map[string]errand.Status{"ERR-PROD": errand.StatusRunning})
	h := NewErrandHandler(d, errand.NewStore(pool), nil, nil, nil,
		fakeScoper{covens: []string{"dev"}}, nil)

	_, err := h.CancelTyped(context.Background(), claimsFor("archon-alice"), "ERR-PROD")
	if got := cancelProblemType(t, err); !strings.Contains(got, "not-found") {
		t.Fatalf("problem.Type = %q, want the same not-found an unknown id gets", got)
	}
	if pool.queries != 1 {
		t.Errorf("store statements = %d, want exactly 1 — the authorization read and no more", pool.queries)
	}
}

// TestErrandCancel_InScopeReachesTheDispatcher pins that the check admits what it
// should. Without it the guard above is satisfied by a handler that refuses every
// cancel, which is a different bug with the same test result.
func TestErrandCancel_InScopeReachesTheDispatcher(t *testing.T) {
	pool := &errandRowPool{rows: map[string][]any{
		"ERR-DEV": errandScopeRow("ERR-DEV", "dev-web-01", []string{"dev"}),
	}}
	d := buildCancelDispatcher(t, map[string]errand.Status{"ERR-DEV": errand.StatusRunning})
	h := NewErrandHandler(d, errand.NewStore(pool), nil, nil, nil,
		fakeScoper{covens: []string{"dev"}}, nil)

	reply, err := h.CancelTyped(context.Background(), claimsFor("archon-alice"), "ERR-DEV")
	if err != nil {
		t.Fatalf("CancelTyped on an in-scope host: %v", err)
	}
	if reply.ErrandID != "ERR-DEV" {
		t.Fatalf("reply.ErrandID = %q, want ERR-DEV", reply.ErrandID)
	}
}

// TestErrandCancel_NoScoperIsFailClosed — an unconfigured resolver hides
// everything rather than admitting everything. This is the direction that matters:
// the opposite default would make every deployment that forgets to wire the
// resolver silently unscoped.
func TestErrandCancel_NoScoperIsFailClosed(t *testing.T) {
	pool := &errandRowPool{rows: map[string][]any{
		"ERR-DEV": errandScopeRow("ERR-DEV", "dev-web-01", []string{"dev"}),
	}}
	d := buildCancelDispatcher(t, map[string]errand.Status{"ERR-DEV": errand.StatusRunning})
	h := NewErrandHandler(d, errand.NewStore(pool), nil, nil, nil, nil /*scoper*/, nil)

	_, err := h.CancelTyped(context.Background(), claimsFor("archon-alice"), "ERR-DEV")
	if got := cancelProblemType(t, err); !strings.Contains(got, "not-found") {
		t.Fatalf("problem.Type = %q, want not-found — a nil resolver must hide, not admit", got)
	}
}
