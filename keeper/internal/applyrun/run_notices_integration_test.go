//go:build integration

package applyrun

import (
	"context"
	"testing"
)

// TestIntegration_RunNotices — the store boundary of NIM-237, end to end at this
// layer: AppendRunNotices (what handleTaskEvent calls per reporting task) ->
// apply_runs.notices (migration 107) -> SelectRunDetail (the exact read
// projection the runs-detail API serves).
//
// The scenario is the one that matters: SEVERAL tasks report, and one of them
// reports the same deprecation a second time. That is the ordinary shape — a
// param used by twenty tasks reports twenty times — and the operator must be
// shown one thing to migrate, per host, without the repeat count leaking into
// the surface they read.
func TestIntegration_RunNotices(t *testing.T) {
	resetAll(t)
	seedOperator(t, "archon-alice")
	seedIncarnation(t, "redis-prod", "archon-alice")
	ctx := context.Background()
	aid := "archon-alice"

	const applyID = "01HNOTICES00000000000000"
	for _, sid := range []string{"host-a", "host-b"} {
		run := &ApplyRun{
			ApplyID: applyID, SID: sid, IncarnationName: "redis-prod",
			Scenario: "create", Status: StatusSuccess, StartedByAID: &aid,
		}
		if err := Insert(ctx, integrationPool, run); err != nil {
			t.Fatalf("Insert(%s): %v", sid, err)
		}
	}

	deprecatedAddress := RunNotice{
		Code: "deprecated_param", Module: "community.redis.present", Param: "address",
		Message: `param "address" is deprecated since 0.4.0 and stops working in 0.6.0; use "addr" instead`,
	}
	deprecatedTLSCA := RunNotice{
		Code: "deprecated_param", Module: "community.redis.present", Param: "tls_ca",
		Message: `param "tls_ca" is deprecated since 0.4.0 and stops working in 0.6.0`,
	}

	// host-a: two tasks report the same param, a third reports another one.
	for _, n := range []RunNotice{deprecatedAddress, deprecatedAddress, deprecatedTLSCA} {
		if err := AppendRunNotices(ctx, integrationPool, applyID, "host-a", 0, []RunNotice{n}); err != nil {
			t.Fatalf("AppendRunNotices(host-a): %v", err)
		}
	}
	// host-b runs an agent whose manifest still lists `address` as current, so it
	// reports nothing. A park mid-upgrade legitimately answers differently host
	// to host — that is why notices hang off the host row and are not flattened
	// into one run-level verdict nobody established.

	d, err := SelectRunDetail(ctx, integrationPool, applyID, "redis-prod")
	if err != nil {
		t.Fatalf("SelectRunDetail: %v", err)
	}
	if len(d.Hosts) != 2 {
		t.Fatalf("hosts = %d, want 2", len(d.Hosts))
	}

	byHost := map[string][]RunNotice{}
	for _, h := range d.Hosts {
		byHost[h.SID] = h.Notices
	}

	got := byHost["host-a"]
	if len(got) != 2 {
		t.Fatalf("host-a notices = %d, want 2 distinct findings from 3 reports: %+v", len(got), got)
	}
	if got[0].Param != "address" || got[1].Param != "tls_ca" {
		t.Errorf("host-a params = %q/%q, want address/tls_ca in key order", got[0].Param, got[1].Param)
	}
	// The message is the whole point of the channel: it has to survive the round
	// trip carrying the deadline and the replacement, or the operator learns
	// that something is deprecated and nothing more.
	if got[0].Message != deprecatedAddress.Message {
		t.Errorf("host-a message = %q, want it intact", got[0].Message)
	}
	if got[0].Code != "deprecated_param" {
		t.Errorf("host-a code = %q, want deprecated_param", got[0].Code)
	}

	if len(byHost["host-b"]) != 0 {
		t.Errorf("host-b reported nothing but reads back %+v", byHost["host-b"])
	}
}

// TestIntegration_RunNoticesDefaultEmpty — a run from before this column existed,
// or one where nothing was deprecated, must read back as no notices at all. The
// column defaults to '[]' rather than NULL specifically so the append needs no
// COALESCE; this asserts the read side agrees.
func TestIntegration_RunNoticesDefaultEmpty(t *testing.T) {
	resetAll(t)
	seedOperator(t, "archon-alice")
	seedIncarnation(t, "redis-prod", "archon-alice")
	ctx := context.Background()
	aid := "archon-alice"

	run := &ApplyRun{
		ApplyID: "01HQUIETRUN", SID: "host-a", IncarnationName: "redis-prod",
		Scenario: "create", Status: StatusSuccess, StartedByAID: &aid,
	}
	if err := Insert(ctx, integrationPool, run); err != nil {
		t.Fatalf("Insert: %v", err)
	}

	d, err := SelectRunDetail(ctx, integrationPool, run.ApplyID, "redis-prod")
	if err != nil {
		t.Fatalf("SelectRunDetail: %v", err)
	}
	if len(d.Hosts) != 1 {
		t.Fatalf("hosts = %d, want 1", len(d.Hosts))
	}
	if len(d.Hosts[0].Notices) != 0 {
		t.Errorf("a run that reported nothing reads back %+v", d.Hosts[0].Notices)
	}
}
