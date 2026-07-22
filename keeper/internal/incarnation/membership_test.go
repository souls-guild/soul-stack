package incarnation

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
)

func TestAddMembers_BuildsIdempotentInsert(t *testing.T) {
	f := &fakeDB{}
	if err := AddMembers(context.Background(), f, "redis-prod", []string{"a.example.com", "b.example.com"}, ptr("archon-alice")); err != nil {
		t.Fatalf("AddMembers: %v", err)
	}
	if f.execCalls != 1 {
		t.Fatalf("execCalls = %d, want 1", f.execCalls)
	}
	if !strings.Contains(f.lastExecSQL, "INSERT INTO incarnation_membership") ||
		!strings.Contains(f.lastExecSQL, "ON CONFLICT DO NOTHING") ||
		!strings.Contains(f.lastExecSQL, "unnest($2::text[])") {
		t.Errorf("unexpected SQL: %q", f.lastExecSQL)
	}
	// args: incName, sids, byAID.
	if len(f.lastExecArgs) != 3 {
		t.Fatalf("args = %v, want 3", f.lastExecArgs)
	}
	if f.lastExecArgs[0] != "redis-prod" {
		t.Errorf("arg0 = %v, want redis-prod", f.lastExecArgs[0])
	}
	sids, ok := f.lastExecArgs[1].([]string)
	if !ok || len(sids) != 2 || sids[0] != "a.example.com" {
		t.Errorf("arg1 = %v, want [a b]", f.lastExecArgs[1])
	}
	byAID, ok := f.lastExecArgs[2].(*string)
	if !ok || byAID == nil || *byAID != "archon-alice" {
		t.Errorf("arg2 = %v, want *archon-alice", f.lastExecArgs[2])
	}
}

func TestAddMembers_EmptySids_NoOp(t *testing.T) {
	f := &fakeDB{}
	if err := AddMembers(context.Background(), f, "redis-prod", nil, nil); err != nil {
		t.Fatalf("AddMembers: %v", err)
	}
	if f.execCalls != 0 {
		t.Errorf("execCalls = %d, want 0 (empty sids is a no-op)", f.execCalls)
	}
}

func TestAddMembers_InvalidName(t *testing.T) {
	f := &fakeDB{}
	if err := AddMembers(context.Background(), f, "Bad Name", []string{"a.example.com"}, nil); err == nil {
		t.Fatalf("AddMembers: expected error for invalid name")
	}
	if f.execCalls != 0 {
		t.Errorf("execCalls = %d, want 0 (must not hit DB on invalid name)", f.execCalls)
	}
}

func TestAddMembers_PropagatesError(t *testing.T) {
	f := &fakeDB{execErr: errors.New("boom")}
	if err := AddMembers(context.Background(), f, "redis-prod", []string{"a.example.com"}, nil); err == nil {
		t.Fatalf("AddMembers: expected the DB error to propagate")
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
