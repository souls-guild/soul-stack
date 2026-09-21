package incarnation

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// The insert runs through Query, not Exec: `RETURNING sid` is what makes the
// idempotent bind report WHICH SIDs it actually wrote (NIM-209).
func TestAddMembers_BuildsIdempotentInsert(t *testing.T) {
	f := &fakeDB{}
	if err := AddMembers(context.Background(), f, "redis-prod", []string{"a.example.com", "b.example.com"}, ptr("archon-alice")); err != nil {
		t.Fatalf("AddMembers: %v", err)
	}
	if f.queryCalls != 1 {
		t.Fatalf("queryCalls = %d, want 1", f.queryCalls)
	}
	if !strings.Contains(f.querySQL, "INSERT INTO incarnation_membership") ||
		!strings.Contains(f.querySQL, "ON CONFLICT DO NOTHING") ||
		!strings.Contains(f.querySQL, "RETURNING sid") ||
		!strings.Contains(f.querySQL, "unnest($2::text[])") {
		t.Errorf("unexpected SQL: %q", f.querySQL)
	}
	// args: incName, sids, byAID.
	if len(f.queryArgs) != 3 {
		t.Fatalf("args = %v, want 3", f.queryArgs)
	}
	if f.queryArgs[0] != "redis-prod" {
		t.Errorf("arg0 = %v, want redis-prod", f.queryArgs[0])
	}
	sids, ok := f.queryArgs[1].([]string)
	if !ok || len(sids) != 2 || sids[0] != "a.example.com" {
		t.Errorf("arg1 = %v, want [a b]", f.queryArgs[1])
	}
	byAID, ok := f.queryArgs[2].(*string)
	if !ok || byAID == nil || *byAID != "archon-alice" {
		t.Errorf("arg2 = %v, want *archon-alice", f.queryArgs[2])
	}
}

func TestAddMembers_EmptySids_NoOp(t *testing.T) {
	f := &fakeDB{}
	if err := AddMembers(context.Background(), f, "redis-prod", nil, nil); err != nil {
		t.Fatalf("AddMembers: %v", err)
	}
	if f.queryCalls != 0 {
		t.Errorf("queryCalls = %d, want 0 (empty sids is a no-op)", f.queryCalls)
	}
}

func TestAddMembers_InvalidName(t *testing.T) {
	f := &fakeDB{}
	if err := AddMembers(context.Background(), f, "Bad Name", []string{"a.example.com"}, nil); err == nil {
		t.Fatalf("AddMembers: expected error for invalid name")
	}
	if f.queryCalls != 0 {
		t.Errorf("queryCalls = %d, want 0 (must not hit DB on invalid name)", f.queryCalls)
	}
}

func TestAddMembers_PropagatesError(t *testing.T) {
	f := &fakeDB{queryFunc: func(string) (pgx.Rows, error) { return nil, errors.New("boom") }}
	if err := AddMembers(context.Background(), f, "redis-prod", []string{"a.example.com"}, nil); err == nil {
		t.Fatalf("AddMembers: expected the DB error to propagate")
	}
}

// AddMembersReporting reports ONLY the SIDs the insert actually wrote — that is
// what separates a first bind from a re-bind on the operator path. `RETURNING
// sid` emits no row for a pair that already existed, so a re-bind returns the
// empty set while still succeeding (NIM-209).
func TestAddMembersReporting_ReturnsOnlyNewlyWritten(t *testing.T) {
	f := &fakeDB{queryFunc: func(string) (pgx.Rows, error) {
		// Only b was new; a was already a member, so ON CONFLICT swallowed it.
		return &fakeRows{rows: []staticRow{{values: []any{"b.example.com"}}}}, nil
	}}
	bound, err := AddMembersReporting(context.Background(), f, "redis-prod",
		[]string{"a.example.com", "b.example.com"}, ptr("archon-alice"))
	if err != nil {
		t.Fatalf("AddMembersReporting: %v", err)
	}
	if len(bound) != 1 || bound[0] != "b.example.com" {
		t.Fatalf("bound = %v, want [b.example.com] (a was already a member)", bound)
	}
}

func TestAddMembersReporting_FullyIdempotentReBind_NoRowsIsNotAnError(t *testing.T) {
	f := &fakeDB{queryFunc: func(string) (pgx.Rows, error) { return &fakeRows{}, nil }}
	bound, err := AddMembersReporting(context.Background(), f, "redis-prod",
		[]string{"a.example.com"}, nil)
	if err != nil {
		t.Fatalf("re-bind must succeed unchanged, got: %v", err)
	}
	if len(bound) != 0 {
		t.Fatalf("bound = %v, want empty (nothing was newly written)", bound)
	}
}

// RemoveMember is the single-SID unbind and reports whether a row went away, so
// the caller can tell a real unbind from an idempotent repeat.
func TestRemoveMember_ReportsWhetherRowWasRemoved(t *testing.T) {
	f := &fakeDB{execTag: pgconn.NewCommandTag("DELETE 1")}
	removed, err := RemoveMember(context.Background(), f, "redis-prod", "a.example.com")
	if err != nil {
		t.Fatalf("RemoveMember: %v", err)
	}
	if !removed {
		t.Errorf("removed = false, want true")
	}
	if !strings.Contains(f.lastExecSQL, "DELETE FROM incarnation_membership") ||
		!strings.Contains(f.lastExecSQL, "sid = $2") {
		t.Errorf("unexpected SQL: %q", f.lastExecSQL)
	}
}

func TestRemoveMember_NonMemberIsSilentNoOp(t *testing.T) {
	f := &fakeDB{execTag: pgconn.NewCommandTag("DELETE 0")}
	removed, err := RemoveMember(context.Background(), f, "redis-prod", "ghost.example.com")
	if err != nil {
		t.Fatalf("unbinding a non-member must not error, got: %v", err)
	}
	if removed {
		t.Errorf("removed = true, want false (nothing was a member)")
	}
}

func TestRemoveMember_InvalidName(t *testing.T) {
	f := &fakeDB{}
	if _, err := RemoveMember(context.Background(), f, "Bad Name", "a.example.com"); err == nil {
		t.Fatalf("RemoveMember: expected error for invalid name")
	}
	if f.execCalls != 0 {
		t.Errorf("execCalls = %d, want 0 (must not hit DB on invalid name)", f.execCalls)
	}
}

func TestRemoveMembers_BuildsDelete(t *testing.T) {
	f := &fakeDB{}
	if err := RemoveMembers(context.Background(), f, "redis-prod", []string{"a.example.com"}); err != nil {
		t.Fatalf("RemoveMembers: %v", err)
	}
	if !strings.Contains(f.lastExecSQL, "DELETE FROM incarnation_membership") ||
		!strings.Contains(f.lastExecSQL, "incarnation_name = $1") ||
		!strings.Contains(f.lastExecSQL, "sid = ANY($2)") {
		t.Errorf("unexpected SQL: %q", f.lastExecSQL)
	}
}

func TestRemoveMembers_EmptySids_NoOp(t *testing.T) {
	f := &fakeDB{}
	if err := RemoveMembers(context.Background(), f, "redis-prod", nil); err != nil {
		t.Fatalf("RemoveMembers: %v", err)
	}
	if f.execCalls != 0 {
		t.Errorf("execCalls = %d, want 0", f.execCalls)
	}
}

func TestListMemberSIDs_ReturnsSorted(t *testing.T) {
	f := &fakeDB{queryFunc: func(string) (pgx.Rows, error) {
		return &fakeRows{rows: []staticRow{
			{values: []any{"a.example.com"}},
			{values: []any{"b.example.com"}},
		}}, nil
	}}
	got, err := ListMemberSIDs(context.Background(), f, "redis-prod")
	if err != nil {
		t.Fatalf("ListMemberSIDs: %v", err)
	}
	if len(got) != 2 || got[0] != "a.example.com" || got[1] != "b.example.com" {
		t.Errorf("got = %v, want [a b]", got)
	}
	if !strings.Contains(f.querySQL, "FROM incarnation_membership") ||
		!strings.Contains(f.querySQL, "incarnation_name = $1") {
		t.Errorf("unexpected SQL: %q", f.querySQL)
	}
}

func TestListMemberSIDs_InvalidName(t *testing.T) {
	f := &fakeDB{}
	if _, err := ListMemberSIDs(context.Background(), f, "Bad Name"); err == nil {
		t.Fatalf("ListMemberSIDs: expected error for invalid name")
	}
}

func ptr(s string) *string { return &s }
