package soul

import (
	"context"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5"
)

// recordingChecker captures every context a gate ORs over, so a test can assert
// on the SET and not merely on the verdict — the whole bug NIM-650 fixes was a
// gate that built one context too few.
type recordingChecker struct {
	seen  []map[string]string
	allow func(map[string]string) bool
}

func (r *recordingChecker) Check(_, _, _ string, ctx map[string]string) error {
	r.seen = append(r.seen, ctx)
	if r.allow != nil && r.allow(ctx) {
		return nil
	}
	return errors.New("denied")
}

func TestHostContexts_CovenlessHostKeepsTheHostContext(t *testing.T) {
	t.Parallel()
	got := HostContexts("host-a", nil)
	if len(got) != 1 || got[0]["host"] != "host-a" {
		t.Fatalf("HostContexts = %v, want exactly [{host:host-a}]", got)
	}
	if _, ok := got[0]["coven"]; ok {
		t.Error("an unknown coven must be ABSENT from the context, not an empty value: " +
			"a present-but-empty `coven` would let `coven=\"\"` conditions match")
	}
}

func TestHostContexts_EveryCovenGetsItsOwnContext(t *testing.T) {
	t.Parallel()
	got := HostContexts("host-a", []string{"web", "prod"})
	if len(got) != 2 {
		t.Fatalf("HostContexts = %v, want one context per coven", got)
	}
	for i, want := range []string{"web", "prod"} {
		if got[i]["host"] != "host-a" || got[i]["coven"] != want {
			t.Errorf("contexts[%d] = %v, want {host:host-a, coven:%s}", i, got[i], want)
		}
	}
}

func TestHostContexts_EmptySIDIsNoContextAtAll(t *testing.T) {
	t.Parallel()
	if got := HostContexts("", []string{"web"}); got != nil {
		t.Fatalf("HostContexts(\"\", …) = %v, want nil — a coven without a host is not a host context", got)
	}
}

func TestAllowAnyContext_GrantsOnAnySingleMatch(t *testing.T) {
	t.Parallel()
	rc := &recordingChecker{allow: func(c map[string]string) bool { return c["coven"] == "prod" }}
	err := AllowAnyContext(rc, "archon-alice", "soul", "console",
		HostContexts("host-a", []string{"web", "prod", "eu"}))
	if err != nil {
		t.Fatalf("err = %v, want nil — a grant on ANY of the host's covens admits it", err)
	}
	if len(rc.seen) != 2 {
		t.Errorf("checked %d contexts, want 2 (stops at the first match)", len(rc.seen))
	}
}

func TestAllowAnyContext_DeniedEverywhereReturnsTheLastDenial(t *testing.T) {
	t.Parallel()
	rc := &recordingChecker{}
	err := AllowAnyContext(rc, "archon-alice", "soul", "console",
		HostContexts("host-a", []string{"web", "prod"}))
	if err == nil {
		t.Fatal("err = nil, want a denial — no context matched")
	}
	if len(rc.seen) != 2 {
		t.Errorf("checked %d contexts, want all 2", len(rc.seen))
	}
}

func TestAllowAnyContext_EmptySetStillAsksOnce(t *testing.T) {
	t.Parallel()
	rc := &recordingChecker{allow: func(c map[string]string) bool { return c == nil }}
	if err := AllowAnyContext(rc, "archon-alice", "soul", "console", nil); err != nil {
		t.Fatalf("err = %v, want nil — a bare/`*` grant must still pass with no context", err)
	}
	if len(rc.seen) != 1 || rc.seen[0] != nil {
		t.Fatalf("seen = %v, want exactly one nil context", rc.seen)
	}
}

func TestHostContextsBySID_UnreadableRowFailsClosedOnCoven(t *testing.T) {
	t.Parallel()
	// A SID with no row: exactly what a forged or already-forgotten SID looks
	// like. It must NOT be able to conjure a coven context.
	db := &fakeDB{rowFunc: func() pgx.Row { return errRow{err: pgx.ErrNoRows} }}
	got := HostContextsBySID(context.Background(), db, "host-ghost")
	if len(got) != 1 {
		t.Fatalf("contexts = %v, want the {host} context alone", got)
	}
	if _, ok := got[0]["coven"]; ok {
		t.Fatal("an unreadable host produced a coven context — a forged SID could then " +
			"reach a `coven=`-scoped grant it was never covered by")
	}
	rc := &recordingChecker{allow: func(c map[string]string) bool { return c["coven"] == "web" }}
	if err := AllowAnyContext(rc, "archon-alice", "soul", "console", got); err == nil {
		t.Fatal("a `coven=web` grant admitted a host whose row could not be read; " +
			"the coven dimension must fail closed when it is unknown")
	}
}

func TestHostContextsBySID_NilReaderIsHostOnly(t *testing.T) {
	t.Parallel()
	got := HostContextsBySID(context.Background(), nil, "host-a")
	if len(got) != 1 || got[0]["host"] != "host-a" {
		t.Fatalf("contexts = %v, want [{host:host-a}]", got)
	}
}

func TestHostContextsBySIDs_EveryInputSIDIsAKey(t *testing.T) {
	t.Parallel()
	db := &fakeDB{queryFunc: func() (pgx.Rows, error) {
		return &fakeRows{rows: []staticRow{
			{values: []any{"host-a", []string{"web"}}},
		}}, nil
	}}
	got := HostContextsBySIDs(context.Background(), db, []string{"host-a", "host-b"})
	if len(got) != 2 {
		t.Fatalf("result = %v, want a key per input SID", got)
	}
	if len(got["host-a"]) != 1 || got["host-a"][0]["coven"] != "web" {
		t.Errorf("host-a = %v, want its coven context", got["host-a"])
	}
	// host-b had no row: host-only, and therefore closed to a coven grant. A
	// batch that dropped it entirely would be worse than useless — the caller
	// iterates the resolved SIDs and would read "no contexts" as "unrestricted".
	if len(got["host-b"]) != 1 {
		t.Fatalf("host-b = %v, want the {host} context alone", got["host-b"])
	}
	if _, ok := got["host-b"][0]["coven"]; ok {
		t.Error("host-b has no row yet carries a coven context")
	}
}

func TestHostContextsBySIDs_ReadFailureIsHostOnlyForEveryone(t *testing.T) {
	t.Parallel()
	db := &fakeDB{queryFunc: func() (pgx.Rows, error) { return nil, errors.New("pg is down") }}
	got := HostContextsBySIDs(context.Background(), db, []string{"host-a", "host-b"})
	for _, sid := range []string{"host-a", "host-b"} {
		if len(got[sid]) != 1 {
			t.Fatalf("%s = %v, want the {host} context alone", sid, got[sid])
		}
		if _, ok := got[sid][0]["coven"]; ok {
			t.Errorf("%s got a coven context out of a failed read", sid)
		}
	}
}
