package handlers

// Guard tests for the operator membership path (ADR-008 amendment 2026-07-28,
// NIM-209). Three invariants the ticket is about:
//
//  1. an operator CAN bind an onboarded host — the step that was missing, and
//     without which a create scenario over a ready roster is unreachable;
//  2. binding a host OUTSIDE the caller's soul scope is refused — and refused
//     WHOLESALE, never partially applied;
//  3. re-binding is idempotent and still reports what actually happened.

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/souls-guild/soul-stack/keeper/internal/jwt"
	"github.com/souls-guild/soul-stack/keeper/internal/rbac"
	"github.com/souls-guild/soul-stack/shared/audit"
)

// --- fakes ---------------------------------------------------------------

// memberHost — the host facts the membership paths read: registry presence,
// lifecycle status and the scope dimensions.
type memberHost struct {
	sid    string
	status string
	covens []string
	traits map[string]any
}

// fakeMemberDB answers the four queries the membership paths issue: the
// incarnation probe, the souls lookup, the membership INSERT ... RETURNING and
// the roster read. Everything it is not asked about is deliberately absent — a
// query this fake does not recognize shows up as an empty result, which fails
// the assertion rather than passing silently.
type fakeMemberDB struct {
	incarnationExists bool
	hosts             []memberHost
	// boundNow — the SIDs the INSERT reports as newly written (what `RETURNING
	// sid` yields; a SID already bound emits no row).
	boundNow []string
	// members — the current roster for the read path.
	members []memberHost

	insertArgs   []any
	insertCalled bool
	deleteCalled bool
	deleteTag    pgconn.CommandTag
}

func (f *fakeMemberDB) Exec(_ context.Context, sql string, _ ...any) (pgconn.CommandTag, error) {
	if strings.Contains(sql, "DELETE FROM incarnation_membership") {
		f.deleteCalled = true
		if f.deleteTag.String() == "" {
			return pgconn.NewCommandTag("DELETE 1"), nil
		}
		return f.deleteTag, nil
	}
	return pgconn.CommandTag{}, nil
}

func (f *fakeMemberDB) QueryRow(_ context.Context, sql string, args ...any) pgx.Row {
	if strings.Contains(sql, "FROM incarnation") && strings.Contains(sql, "WHERE name") {
		if !f.incarnationExists {
			return memberErrRow{pgx.ErrNoRows}
		}
		return memberIncRow{name: args[0].(string)}
	}
	// Single-soul lookup (the unbind scope probe).
	if strings.Contains(sql, "FROM souls") {
		for _, h := range f.hosts {
			if len(args) > 0 && h.sid == args[0].(string) {
				return memberSoulRow{h: h}
			}
		}
		return memberErrRow{pgx.ErrNoRows}
	}
	return memberErrRow{pgx.ErrNoRows}
}

func (f *fakeMemberDB) Query(_ context.Context, sql string, args ...any) (pgx.Rows, error) {
	switch {
	case strings.Contains(sql, "INSERT INTO incarnation_membership"):
		f.insertCalled = true
		f.insertArgs = args
		rows := make([][]any, 0, len(f.boundNow))
		for _, sid := range f.boundNow {
			rows = append(rows, []any{sid})
		}
		return &memberRows{rows: rows}, nil
	case strings.Contains(sql, "FROM incarnation_membership m"):
		rows := make([][]any, 0, len(f.members))
		for _, h := range f.members {
			traits, _ := json.Marshal(h.traits)
			if len(h.traits) == 0 {
				traits = nil
			}
			rows = append(rows, []any{h.sid, h.status, h.covens, traits, time.Unix(0, 0).UTC(), (*string)(nil)})
		}
		return &memberRows{rows: rows}, nil
	case strings.Contains(sql, "FROM souls"):
		want := map[string]struct{}{}
		if len(args) > 0 {
			if sids, ok := args[0].([]string); ok {
				for _, s := range sids {
					want[s] = struct{}{}
				}
			}
		}
		rows := make([][]any, 0, len(f.hosts))
		for _, h := range f.hosts {
			if _, ok := want[h.sid]; !ok {
				continue
			}
			traits, _ := json.Marshal(h.traits)
			if len(h.traits) == 0 {
				traits = nil
			}
			rows = append(rows, []any{
				h.sid, "agent", h.status, h.covens, traits,
				time.Unix(0, 0).UTC(), (*time.Time)(nil), (*string)(nil),
				(*string)(nil), (*time.Time)(nil), (*string)(nil),
			})
		}
		return &memberRows{rows: rows}, nil
	}
	return &memberRows{}, nil
}

func (f *fakeMemberDB) BeginTx(_ context.Context, _ pgx.TxOptions) (pgx.Tx, error) {
	return nil, errors.New("fakeMemberDB: BeginTx not used by the membership paths")
}

type memberErrRow struct{ err error }

func (r memberErrRow) Scan(_ ...any) error { return r.err }

// memberIncRow feeds incarnation.SelectByName: only the identity columns matter
// to the membership paths, the rest are zero values of the right shape.
type memberIncRow struct{ name string }

func (r memberIncRow) Scan(dest ...any) error {
	// Column order of incarnation.scanIncarnation (crud.go): name, service,
	// service_version, state_schema_version, state, status, status_details,
	// created_by_aid, created_at, updated_at, covens, traits, created_scenario,
	// applying_apply_id, label.
	vals := []any{
		r.name, "redis", "v1.0.0", 1,
		[]byte(`{}`), "ready", []byte(`{}`),
		(*string)(nil), time.Unix(0, 0).UTC(), time.Unix(0, 0).UTC(),
		[]string{}, []byte(`{}`),
		(*string)(nil), (*string)(nil),
		(*string)(nil), // label (ADR-0085): unset here, reads NULL
	}
	return scanInto(dest, vals)
}

type memberSoulRow struct{ h memberHost }

func (r memberSoulRow) Scan(dest ...any) error {
	traits, _ := json.Marshal(r.h.traits)
	if len(r.h.traits) == 0 {
		traits = nil
	}
	vals := []any{
		r.h.sid, "agent", r.h.status, r.h.covens, traits,
		time.Unix(0, 0).UTC(), (*time.Time)(nil), (*string)(nil),
		(*string)(nil), (*time.Time)(nil), (*string)(nil),
	}
	return scanInto(dest, vals)
}

type memberRows struct {
	rows [][]any
	i    int
}

func (r *memberRows) Next() bool             { r.i++; return r.i <= len(r.rows) }
func (r *memberRows) Scan(dest ...any) error { return scanInto(dest, r.rows[r.i-1]) }
func (r *memberRows) Close()                 {}
func (r *memberRows) Err() error             { return nil }
func (r *memberRows) CommandTag() pgconn.CommandTag {
	return pgconn.CommandTag{}
}
func (r *memberRows) FieldDescriptions() []pgconn.FieldDescription { return nil }
func (r *memberRows) Values() ([]any, error)                       { return nil, nil }
func (r *memberRows) RawValues() [][]byte                          { return nil }
func (r *memberRows) Conn() *pgx.Conn                              { return nil }

// scanInto assigns positional values onto the scan destinations, covering the
// pointer shapes the membership queries use.
func scanInto(dest []any, vals []any) error {
	if len(dest) > len(vals) {
		return errors.New("scanInto: more destinations than values")
	}
	for i := range dest {
		switch d := dest[i].(type) {
		case *string:
			switch v := vals[i].(type) {
			case string:
				*d = v
			case *string:
				if v != nil {
					*d = *v
				}
			}
		case **string:
			if v, ok := vals[i].(*string); ok {
				*d = v
			}
		case *[]string:
			if v, ok := vals[i].([]string); ok {
				*d = v
			}
		case *[]byte:
			if v, ok := vals[i].([]byte); ok {
				*d = v
			}
		case *int:
			if v, ok := vals[i].(int); ok {
				*d = v
			}
		case *time.Time:
			if v, ok := vals[i].(time.Time); ok {
				*d = v
			}
		case **time.Time:
			if v, ok := vals[i].(*time.Time); ok {
				*d = v
			}
		}
	}
	return nil
}

// memberScoper — a PurviewResolver whose soul scope is a fixed coven set;
// unrestricted=true means "every host".
type memberScoper struct {
	unrestricted bool
	coven        string
}

func (s memberScoper) ResolvePurview(_, _, _ string) rbac.Purview {
	if s.unrestricted {
		return rbac.Purview{Unrestricted: true}
	}
	expr, err := rbac.ParseScopeExpr("coven=" + s.coven)
	if err != nil {
		panic("memberScoper: " + err.Error())
	}
	return rbac.Purview{Exprs: []*rbac.ScopeExpr{expr}}
}

// memberAuditCapture records the events a handler writes, so a test can assert on the
// trail rather than only on the reply.
type memberAuditCapture struct{ events []*audit.Event }

func (c *memberAuditCapture) Write(_ context.Context, e *audit.Event) error {
	c.events = append(c.events, e)
	return nil
}

func memberClaims() *jwt.Claims { return &jwt.Claims{Subject: "archon-alice"} }

func memberHandler(db *fakeMemberDB, scoper PurviewResolver, auditW audit.Writer) *IncarnationHandler {
	return NewIncarnationHandler(db, nil, nil, nil, nil, auditW, scoper, nil)
}

// --- guard 1: an operator can bind an onboarded host ----------------------

// The whole point of NIM-209: before it, an already-onboarded host could not be
// put into an incarnation from outside a scenario run at all.
func TestBindMembers_OperatorBindsOnboardedHost(t *testing.T) {
	db := &fakeMemberDB{
		incarnationExists: true,
		hosts: []memberHost{
			{sid: "node-1.example.com", status: "connected", covens: []string{"prod"}},
			{sid: "node-2.example.com", status: "connected", covens: []string{"prod"}},
		},
		boundNow: []string{"node-1.example.com", "node-2.example.com"},
	}
	auditCap := &memberAuditCapture{}
	h := memberHandler(db, memberScoper{unrestricted: true}, auditCap)

	view, err := h.BindMembersTyped(context.Background(), memberClaims(), "redis-prod",
		[]string{"node-2.example.com", "node-1.example.com"})
	if err != nil {
		t.Fatalf("BindMembersTyped: %v", err)
	}
	if len(view.Bound) != 2 || view.Bound[0] != "node-1.example.com" {
		t.Fatalf("bound = %v, want both SIDs sorted", view.Bound)
	}
	if len(view.AlreadyMember) != 0 {
		t.Errorf("already_member = %v, want empty", view.AlreadyMember)
	}
	if !db.insertCalled {
		t.Fatal("membership INSERT was not issued")
	}
	// bound_by_aid must carry the caller — the membership row is an audited fact.
	byAID, ok := db.insertArgs[2].(*string)
	if !ok || byAID == nil || *byAID != "archon-alice" {
		t.Errorf("bound_by_aid arg = %v, want archon-alice", db.insertArgs[2])
	}
	if len(auditCap.events) != 1 || auditCap.events[0].EventType != audit.EventIncarnationMemberBound {
		t.Fatalf("audit = %v, want one incarnation.member_bound", auditCap.events)
	}
}

// --- guard 2: a host outside the caller's soul scope is refused -----------

// THE security invariant. Gate (a) is satisfied here (the enforcer is not even
// in the picture at this layer), so what must stop the bind is the per-host
// gate: otherwise an `incarnation=`-scoped role could pull any host into its
// incarnation and reach it with incarnation.run.
func TestBindMembers_ForeignHostRefused(t *testing.T) {
	db := &fakeMemberDB{
		incarnationExists: true,
		hosts: []memberHost{
			{sid: "node-1.example.com", status: "connected", covens: []string{"prod"}},
			{sid: "foreign.example.com", status: "connected", covens: []string{"other-team"}},
		},
		boundNow: []string{"node-1.example.com"},
	}
	h := memberHandler(db, memberScoper{coven: "prod"}, &memberAuditCapture{})

	_, err := h.BindMembersTyped(context.Background(), memberClaims(), "redis-prod",
		[]string{"node-1.example.com", "foreign.example.com"})
	if err == nil {
		t.Fatal("binding a host outside the operator's soul scope must be refused")
	}
	if !strings.Contains(err.Error(), "foreign.example.com") {
		t.Errorf("error must name the offending SID, got: %v", err)
	}
	// ALL-OR-NOTHING: the in-scope host must NOT have been bound either. A partial
	// bind would read as success and leave the roster short a host.
	if db.insertCalled {
		t.Error("nothing may be written when any SID is out of scope (all-or-nothing)")
	}
}

func TestBindMembers_ForeignHostErrorDoesNotLeakItsState(t *testing.T) {
	// The foreign host is ALSO disconnected. The operator may not see it, so the
	// answer must be "forbidden" — never "that host is disconnected", which would
	// disclose the state of a host behind the scope boundary.
	db := &fakeMemberDB{
		incarnationExists: true,
		hosts: []memberHost{
			{sid: "foreign.example.com", status: "disconnected", covens: []string{"other-team"}},
		},
	}
	h := memberHandler(db, memberScoper{coven: "prod"}, &memberAuditCapture{})

	_, err := h.BindMembersTyped(context.Background(), memberClaims(), "redis-prod",
		[]string{"foreign.example.com"})
	if err == nil {
		t.Fatal("expected refusal")
	}
	if strings.Contains(err.Error(), "disconnected") {
		t.Errorf("scope refusal must not disclose the host's status, got: %v", err)
	}
	if !strings.Contains(err.Error(), "scope") {
		t.Errorf("expected a scope refusal, got: %v", err)
	}
}

// --- guard 3: re-binding is idempotent -----------------------------------

// A repeat bind succeeds and changes nothing; the reply still distinguishes it
// from a first bind, so an operator (or an agent) can tell the two apart.
func TestBindMembers_ReBindIsIdempotent(t *testing.T) {
	db := &fakeMemberDB{
		incarnationExists: true,
		hosts: []memberHost{
			{sid: "node-1.example.com", status: "connected", covens: []string{"prod"}},
		},
		boundNow: nil, // ON CONFLICT DO NOTHING: the pair already existed.
	}
	auditCap := &memberAuditCapture{}
	h := memberHandler(db, memberScoper{unrestricted: true}, auditCap)

	view, err := h.BindMembersTyped(context.Background(), memberClaims(), "redis-prod",
		[]string{"node-1.example.com"})
	if err != nil {
		t.Fatalf("a repeat bind must succeed, got: %v", err)
	}
	if len(view.Bound) != 0 {
		t.Errorf("bound = %v, want empty on a repeat", view.Bound)
	}
	if len(view.AlreadyMember) != 1 || view.AlreadyMember[0] != "node-1.example.com" {
		t.Errorf("already_member = %v, want the re-bound SID", view.AlreadyMember)
	}
	if len(auditCap.events) != 1 {
		t.Fatalf("audit events = %d, want 1 (the attempt is recorded either way)", len(auditCap.events))
	}
	if got := auditCap.events[0].Payload["already_member"]; got == nil {
		t.Error("audit payload must carry already_member so a re-bind stays distinguishable")
	}
}

// --- status gate ----------------------------------------------------------

func TestBindMembers_OnlyConnectedHostsMayBeBound(t *testing.T) {
	for _, status := range []string{"pending", "disconnected", "revoked", "expired", "destroyed"} {
		t.Run(status, func(t *testing.T) {
			db := &fakeMemberDB{
				incarnationExists: true,
				hosts:             []memberHost{{sid: "node-1.example.com", status: status, covens: []string{"prod"}}},
			}
			h := memberHandler(db, memberScoper{unrestricted: true}, &memberAuditCapture{})

			if _, err := h.BindMembersTyped(context.Background(), memberClaims(), "redis-prod",
				[]string{"node-1.example.com"}); err == nil {
				t.Fatalf("status %q must not be bindable by an operator", status)
			}
			if db.insertCalled {
				t.Error("nothing may be written when a host is not connected")
			}
		})
	}
}

func TestBindMembers_UnknownSIDIsRejectedBeforeTheFKWould(t *testing.T) {
	db := &fakeMemberDB{incarnationExists: true}
	h := memberHandler(db, memberScoper{unrestricted: true}, &memberAuditCapture{})

	_, err := h.BindMembersTyped(context.Background(), memberClaims(), "redis-prod",
		[]string{"ghost.example.com"})
	if err == nil {
		t.Fatal("an unknown SID must be rejected")
	}
	if !strings.Contains(err.Error(), "ghost.example.com") {
		t.Errorf("error must name the unknown SID, got: %v", err)
	}
}

func TestBindMembers_MissingIncarnationIs404(t *testing.T) {
	db := &fakeMemberDB{incarnationExists: false}
	h := memberHandler(db, memberScoper{unrestricted: true}, &memberAuditCapture{})

	if _, err := h.BindMembersTyped(context.Background(), memberClaims(), "redis-prod",
		[]string{"node-1.example.com"}); err == nil {
		t.Fatal("binding into a nonexistent incarnation must fail")
	}
}

// The FK invariant read from the other side: the incarnation row must exist
// BEFORE membership, and the handler proves it by probing first — no write is
// attempted against a missing incarnation.
func TestBindMembers_NoWriteWithoutTheIncarnationRow(t *testing.T) {
	db := &fakeMemberDB{
		incarnationExists: false,
		hosts:             []memberHost{{sid: "node-1.example.com", status: "connected"}},
	}
	h := memberHandler(db, memberScoper{unrestricted: true}, &memberAuditCapture{})
	_, _ = h.BindMembersTyped(context.Background(), memberClaims(), "redis-prod", []string{"node-1.example.com"})
	if db.insertCalled {
		t.Error("membership must never be written before the incarnation row exists (FK invariant)")
	}
}

func TestBindMembers_NoScoperFailsClosed(t *testing.T) {
	db := &fakeMemberDB{
		incarnationExists: true,
		hosts:             []memberHost{{sid: "node-1.example.com", status: "connected"}},
	}
	h := memberHandler(db, nil, &memberAuditCapture{})

	if _, err := h.BindMembersTyped(context.Background(), memberClaims(), "redis-prod",
		[]string{"node-1.example.com"}); err == nil {
		t.Fatal("without a purview resolver the per-host gate cannot be evaluated — must fail closed")
	}
	if db.insertCalled {
		t.Error("nothing may be written when the gate cannot be evaluated")
	}
}

// --- unbind ---------------------------------------------------------------

func TestUnbindMember_RemovesAndAudits(t *testing.T) {
	db := &fakeMemberDB{
		incarnationExists: true,
		hosts:             []memberHost{{sid: "node-1.example.com", status: "connected", covens: []string{"prod"}}},
	}
	auditCap := &memberAuditCapture{}
	h := memberHandler(db, memberScoper{unrestricted: true}, auditCap)

	if err := h.UnbindMemberTyped(context.Background(), memberClaims(), "redis-prod", "node-1.example.com"); err != nil {
		t.Fatalf("UnbindMemberTyped: %v", err)
	}
	if !db.deleteCalled {
		t.Error("membership DELETE was not issued")
	}
	if len(auditCap.events) != 1 || auditCap.events[0].EventType != audit.EventIncarnationMemberUnbound {
		t.Fatalf("audit = %v, want one incarnation.member_unbound", auditCap.events)
	}
}

func TestUnbindMember_ForeignHostRefused(t *testing.T) {
	db := &fakeMemberDB{
		incarnationExists: true,
		hosts:             []memberHost{{sid: "foreign.example.com", status: "connected", covens: []string{"other-team"}}},
	}
	h := memberHandler(db, memberScoper{coven: "prod"}, &memberAuditCapture{})

	if err := h.UnbindMemberTyped(context.Background(), memberClaims(), "redis-prod", "foreign.example.com"); err == nil {
		t.Fatal("unbinding a host outside the operator's soul scope must be refused")
	}
	if db.deleteCalled {
		t.Error("nothing may be deleted when the SID is out of scope")
	}
}

// A host deleted from the registry has already lost its memberships to the FK
// cascade, so the unbind is a no-op rather than a 404 — and it must not fail on
// a scope check it cannot perform.
func TestUnbindMember_UnknownHostIsIdempotentNoOp(t *testing.T) {
	db := &fakeMemberDB{
		incarnationExists: true,
		deleteTag:         pgconn.NewCommandTag("DELETE 0"),
	}
	auditCap := &memberAuditCapture{}
	h := memberHandler(db, memberScoper{coven: "prod"}, auditCap)

	if err := h.UnbindMemberTyped(context.Background(), memberClaims(), "redis-prod", "ghost.example.com"); err != nil {
		t.Fatalf("unbinding an unknown host must succeed as a no-op, got: %v", err)
	}
	if len(auditCap.events) != 1 {
		t.Fatalf("audit events = %d, want 1 (the intent is recorded even on a no-op)", len(auditCap.events))
	}
	if removed := auditCap.events[0].Payload["removed"]; removed != false {
		t.Errorf("audit removed = %v, want false", removed)
	}
}

// --- roster read ----------------------------------------------------------

func TestListMembers_NarrowsToTheCallersSoulScope(t *testing.T) {
	db := &fakeMemberDB{
		incarnationExists: true,
		members: []memberHost{
			{sid: "node-1.example.com", status: "connected", covens: []string{"prod"}},
			{sid: "foreign.example.com", status: "connected", covens: []string{"other-team"}},
		},
	}
	h := memberHandler(db, memberScoper{coven: "prod"}, &memberAuditCapture{})

	page, err := h.ListMembersTyped(context.Background(), memberClaims(), "redis-prod")
	if err != nil {
		t.Fatalf("ListMembersTyped: %v", err)
	}
	if len(page.Items) != 1 || page.Items[0].SID != "node-1.example.com" {
		t.Fatalf("items = %v, want only the in-scope host", page.Items)
	}
}

func TestListMembers_NoScoperYieldsNothing(t *testing.T) {
	db := &fakeMemberDB{
		incarnationExists: true,
		members:           []memberHost{{sid: "node-1.example.com", status: "connected"}},
	}
	h := memberHandler(db, nil, &memberAuditCapture{})

	page, err := h.ListMembersTyped(context.Background(), memberClaims(), "redis-prod")
	if err != nil {
		t.Fatalf("ListMembersTyped: %v", err)
	}
	if len(page.Items) != 0 {
		t.Errorf("items = %v, want empty (fail-closed without a resolver)", page.Items)
	}
}
