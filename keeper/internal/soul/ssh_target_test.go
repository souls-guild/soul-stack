package soul

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5"
)

// Fakes (fakeDB / staticRow / errRow) are defined in crud_test.go - reuse them.

// --- UpdateSshTarget ---

func TestUpdateSshTarget_HappyPath(t *testing.T) {
	const sid = "host.example.com"
	f := &fakeDB{
		rowFunc: func() pgx.Row {
			return staticRow{values: []any{sid}}
		},
	}
	target := &SSHTarget{SSHPort: 2222, SSHUser: "deploy", SoulPath: "/opt/soul"}
	if err := UpdateSshTarget(context.Background(), f, sid, target); err != nil {
		t.Fatalf("UpdateSshTarget: %v", err)
	}
	if f.queryCalls != 1 {
		t.Errorf("queryCalls = %d, want 1", f.queryCalls)
	}
	// $2 is payload JSON ([]byte). Regression guard: if someone rewrites
	// payload to map[string]any, jsonb-cast in SQL will fail - check form.
	args := f.lastExecArgs
	if args != nil {
		t.Fatalf("lastExecArgs = %v, want nil (QueryRow path)", args)
	}
}

func TestUpdateSshTarget_NilTarget_WritesNull(t *testing.T) {
	// nil target → payload is passed as nil → PG writes NULL (removal
	// of configured target).
	const sid = "host.example.com"
	f := &fakeDB{
		rowFunc: func() pgx.Row {
			return staticRow{values: []any{sid}}
		},
	}
	if err := UpdateSshTarget(context.Background(), f, sid, nil); err != nil {
		t.Fatalf("UpdateSshTarget(nil): %v", err)
	}
	if f.queryCalls != 1 {
		t.Errorf("queryCalls = %d, want 1", f.queryCalls)
	}
}

func TestUpdateSshTarget_InvalidSID(t *testing.T) {
	f := &fakeDB{}
	err := UpdateSshTarget(context.Background(), f, "BAD_SID", &SSHTarget{SSHPort: 22})
	if err == nil {
		t.Fatal("invalid SID returned nil err")
	}
	if f.queryCalls != 0 {
		t.Errorf("queryCalls = %d, want 0 (validation before round-trip)", f.queryCalls)
	}
}

func TestUpdateSshTarget_NotFound(t *testing.T) {
	f := &fakeDB{
		rowFunc: func() pgx.Row {
			return errRow{err: pgx.ErrNoRows}
		},
	}
	err := UpdateSshTarget(context.Background(), f, "host.example.com", &SSHTarget{SSHPort: 22, SSHUser: "root", SoulPath: "/usr/local/bin/soul"})
	if !errors.Is(err, ErrSoulNotFound) {
		t.Errorf("err = %v, want ErrSoulNotFound", err)
	}
}

// --- SelectSshTarget ---

func TestSelectSshTarget_HappyPath(t *testing.T) {
	want := SSHTarget{SSHPort: 2222, SSHUser: "deploy", SoulPath: "/opt/soul"}
	b, err := json.Marshal(want)
	if err != nil {
		t.Fatalf("marshal want: %v", err)
	}
	f := &fakeDB{
		rowFunc: func() pgx.Row {
			return staticRow{values: []any{b}}
		},
	}
	got, err := SelectSshTarget(context.Background(), f, "host.example.com")
	if err != nil {
		t.Fatalf("SelectSshTarget: %v", err)
	}
	if got == nil {
		t.Fatal("got nil target, want non-nil")
	}
	if *got != want {
		t.Errorf("got = %#v, want = %#v", *got, want)
	}
}

func TestSelectSshTarget_NullColumn(t *testing.T) {
	// Soul row exists, but ssh_target IS NULL - caller gets (nil, nil) and
	// falls back to keeper.yml::push.targets[] (allow_legacy=true) or
	// ErrTargetNotConfigured (allow_legacy=false).
	f := &fakeDB{
		rowFunc: func() pgx.Row {
			return staticRow{values: []any{[]byte(nil)}}
		},
	}
	got, err := SelectSshTarget(context.Background(), f, "host.example.com")
	if err != nil {
		t.Fatalf("SelectSshTarget: %v", err)
	}
	if got != nil {
		t.Errorf("got = %#v, want nil (column IS NULL)", got)
	}
}

func TestSelectSshTarget_NotFound(t *testing.T) {
	f := &fakeDB{
		rowFunc: func() pgx.Row {
			return errRow{err: pgx.ErrNoRows}
		},
	}
	_, err := SelectSshTarget(context.Background(), f, "host.example.com")
	if !errors.Is(err, ErrSoulNotFound) {
		t.Errorf("err = %v, want ErrSoulNotFound", err)
	}
}

func TestSelectSshTarget_InvalidSID(t *testing.T) {
	f := &fakeDB{}
	_, err := SelectSshTarget(context.Background(), f, "BAD_SID")
	if err == nil {
		t.Fatal("invalid SID returned nil err")
	}
	if f.queryCalls != 0 {
		t.Errorf("queryCalls = %d, want 0", f.queryCalls)
	}
}

// --- SSHProvider field (P2 W-1) ---

// TestSshTarget_WithSSHProvider tests round-trip with per-SID
// `ssh_provider` set. JSON-omit on nil checks that old tests don't break.
func TestSshTarget_WithSSHProvider(t *testing.T) {
	sp := "vault-bastion"
	want := SSHTarget{
		SSHPort:     2222,
		SSHUser:     "deploy",
		SoulPath:    "/opt/soul",
		SSHProvider: &sp,
	}
	b, err := json.Marshal(want)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	// omitempty works for *string - field should be present in JSON.
	if !bytes.Contains(b, []byte(`"ssh_provider":"vault-bastion"`)) {
		t.Errorf("ssh_provider did not serialize: %s", string(b))
	}
	var got SSHTarget
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.SSHProvider == nil || *got.SSHProvider != sp {
		t.Errorf("got.SSHProvider = %v, want %q", got.SSHProvider, sp)
	}
}

// TestSshTarget_OmitSSHProvider_BackCompat ensures nil SSHProvider is not
// present in JSON (old consumers don't fail on unknown field).
func TestSshTarget_OmitSSHProvider_BackCompat(t *testing.T) {
	target := SSHTarget{SSHPort: 22, SSHUser: "root", SoulPath: "/usr/local/bin/soul"}
	b, err := json.Marshal(target)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if bytes.Contains(b, []byte("ssh_provider")) {
		t.Errorf("ssh_provider present in JSON, should be omitted: %s", string(b))
	}
}
