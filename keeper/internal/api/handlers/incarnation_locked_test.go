package handlers

import (
	"bytes"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

func TestErrorLockedDetail(t *testing.T) {
	t.Parallel()
	got := ErrorLockedDetail("demo", "01HLOCKER")
	for _, want := range []string{"incarnation demo is error_locked by run 01HLOCKER",
		"POST /v1/incarnations/demo/unlock", "POST /v1/incarnations/demo/rerun-last"} {
		if !strings.Contains(got, want) {
			t.Errorf("ErrorLockedDetail = %q, missing %q", got, want)
		}
	}
	if got := ErrorLockedDetail("demo", ""); strings.Contains(got, "by run") {
		t.Errorf("ErrorLockedDetail without a run = %q, names one", got)
	}
}

// A broad target can resolve to the whole max_scope; the refusal names the first
// few and counts the rest instead of growing with the fleet.
func TestErrorLockedTargetDetail(t *testing.T) {
	t.Parallel()
	two := errorLockedTargetDetail([]LockedIncarnation{{ID: "a", LockingApplyID: "R1"}, {ID: "b"}})
	for _, want := range []string{"2 target incarnations are error_locked", "a by run R1", "b;", "leave it out of the target"} {
		if !strings.Contains(two, want) {
			t.Errorf("detail = %q, missing %q", two, want)
		}
	}

	many := make([]LockedIncarnation, maxLockedNamed+3)
	for i := range many {
		many[i] = LockedIncarnation{ID: fmt.Sprintf("inc-%02d", i)}
	}
	got := errorLockedTargetDetail(many)
	if !strings.Contains(got, "and 3 more") || strings.Contains(got, fmt.Sprintf("inc-%02d", maxLockedNamed)) {
		t.Errorf("detail = %q, want the first %d named and the rest counted", got, maxLockedNamed)
	}
}

// The single-incarnation run route names the run that locked the incarnation,
// the same sentence a refused Voyage and a refused Voyage target carry.
func TestIncarnation_Run_ErrorLocked_409NamesLockingRun(t *testing.T) {
	db := &fakeIncDB{
		selectByNameRow: func(name string) pgx.Row {
			now := time.Now()
			return staticRow{values: []any{
				name, "redis", "v1", int(1),
				[]byte("{}"), "error_locked",
				[]byte(`{"apply_id":"01HLOCKER","reason":"apply_failed"}`), any(nil),
				now, now, []string(nil),
				[]byte("{}"), "create", any(nil), any(nil),
			}}
		},
	}
	starter := &fakeStarter{}
	h := newRunHandler(db, starter, &fakeResolver{ok: true})
	req := withClaims(newChiRequestScenario(http.MethodPost,
		"/v1/incarnations/demo/scenarios/add_user", bytes.NewReader([]byte(`{}`)), "demo", "add_user"), "archon-alice")
	rec := incRun(h, req)

	if rec.Code != http.StatusConflict {
		t.Fatalf("Code = %d, want 409; body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "demo is error_locked by run 01HLOCKER") {
		t.Errorf("body = %s, want the locking run named", rec.Body.String())
	}
	if starter.calls != 0 {
		t.Errorf("starter.calls = %d, want 0", starter.calls)
	}
}
