package rbac

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/souls-guild/soul-stack/shared/obs"
	"github.com/souls-guild/soul-stack/shared/obs/obstest"
)

func TestRegisterRBACMetrics_RegistersFamilies(t *testing.T) {
	reg := obs.NewRegistry()
	m := RegisterRBACMetrics(reg)
	if m == nil {
		t.Fatal("RegisterRBACMetrics returned nil")
	}

	// Vec/Histogram/Counter don't publish their family until the first
	// Observe/Inc — run all Observe methods, then check family presence.
	m.ObserveRebuildSuccess(2*time.Millisecond, 3, 2)
	m.ObserveRebuildError(time.Millisecond, rebuildErrorLoad)
	m.ObserveCheck(nil)
	m.ObserveCheck(errors.New("deny"))
	m.ObserveInvalidation()

	families, err := reg.Gatherer().Gather()
	if err != nil {
		t.Fatalf("Gather: %v", err)
	}
	seen := map[string]bool{}
	for _, f := range families {
		seen[f.GetName()] = true
	}
	for _, want := range []string{
		"keeper_rbac_snapshot_rebuild_duration_seconds",
		"keeper_rbac_snapshot_rebuild_errors_total",
		"keeper_rbac_snapshot_last_success_timestamp_seconds",
		"keeper_rbac_snapshot_roles",
		"keeper_rbac_snapshot_operators",
		"keeper_rbac_checks_total",
		"keeper_rbac_invalidations_received_total",
	} {
		if !seen[want] {
			t.Errorf("MetricFamily %q not registered", want)
		}
	}
}

func TestRegisterRBACMetrics_PanicsOnDoubleRegister(t *testing.T) {
	reg := obs.NewRegistry()
	RegisterRBACMetrics(reg)
	defer func() {
		if r := recover(); r == nil {
			t.Error("expected panic on double register, got none")
		}
	}()
	RegisterRBACMetrics(reg)
}

func TestRBACMetrics_RebuildErrorsByKind(t *testing.T) {
	reg := obs.NewRegistry()
	m := RegisterRBACMetrics(reg)

	m.ObserveRebuildError(time.Millisecond, rebuildErrorLoad)
	m.ObserveRebuildError(time.Millisecond, rebuildErrorParse)
	m.ObserveRebuildError(time.Millisecond, rebuildErrorParse)

	body := obstest.Scrape(t, reg.Gatherer())
	if !strings.Contains(body, `keeper_rbac_snapshot_rebuild_errors_total{kind="load"} 1`) {
		t.Errorf("load count mismatch; got=\n%s", body)
	}
	if !strings.Contains(body, `keeper_rbac_snapshot_rebuild_errors_total{kind="parse"} 2`) {
		t.Errorf("parse count mismatch; got=\n%s", body)
	}
	// Each ObserveRebuildError also observes duration.
	if !strings.Contains(body, `keeper_rbac_snapshot_rebuild_duration_seconds_count 3`) {
		t.Errorf("rebuild duration count should be 3; got=\n%s", body)
	}
}

func TestRBACMetrics_RebuildSuccessGauges(t *testing.T) {
	reg := obs.NewRegistry()
	m := RegisterRBACMetrics(reg)

	m.ObserveRebuildSuccess(time.Millisecond, 5, 3)

	body := obstest.Scrape(t, reg.Gatherer())
	if !strings.Contains(body, `keeper_rbac_snapshot_roles 5`) {
		t.Errorf("roles gauge mismatch; got=\n%s", body)
	}
	if !strings.Contains(body, `keeper_rbac_snapshot_operators 3`) {
		t.Errorf("operators gauge mismatch; got=\n%s", body)
	}
	if !strings.Contains(body, "keeper_rbac_snapshot_last_success_timestamp_seconds") {
		t.Errorf("last_success_timestamp missing; got=\n%s", body)
	}
}

func TestRBACMetrics_ChecksByResult(t *testing.T) {
	reg := obs.NewRegistry()
	m := RegisterRBACMetrics(reg)

	m.ObserveCheck(nil)
	m.ObserveCheck(nil)
	m.ObserveCheck(errors.New("deny"))

	body := obstest.Scrape(t, reg.Gatherer())
	if !strings.Contains(body, `keeper_rbac_checks_total{result="allow"} 2`) {
		t.Errorf("allow count mismatch; got=\n%s", body)
	}
	if !strings.Contains(body, `keeper_rbac_checks_total{result="deny"} 1`) {
		t.Errorf("deny count mismatch; got=\n%s", body)
	}
}

func TestRBACMetrics_NilReceiver_NoOp(t *testing.T) {
	// Holder can start up without the obs stack (NewHolder in the bootstrap
	// path, unit tests before metrics wire-up). Method on a nil receiver is a
	// no-op, no panic.
	var m *RBACMetrics
	m.ObserveRebuildSuccess(time.Second, 1, 1)
	m.ObserveRebuildError(time.Second, rebuildErrorLoad)
	m.ObserveCheck(nil)
	m.ObserveCheck(errors.New("x"))
	m.ObserveInvalidation()
}

// TestHolder_RefreshRecordsSnapshotMetrics — integration of Holder.Refresh
// with metrics: role/operator counts come from the built enforcer.
// fakeSource/adminSnapshot are reused from holder_test.go (same package).
func TestHolder_RefreshRecordsSnapshotMetrics(t *testing.T) {
	reg := obs.NewRegistry()
	m := RegisterRBACMetrics(reg)

	src := &fakeSource{snap: &Snapshot{
		Roles: map[string][]string{
			"admin":  {"*"},
			"viewer": {"incarnation.get"},
		},
		Membership: map[string][]string{
			"archon-alice": {"admin"},
			"archon-bob":   {"viewer"},
		},
	}}
	h, err := NewHolder(context.Background(), src, time.Hour, nil)
	if err != nil {
		t.Fatalf("NewHolder: %v", err)
	}
	h.SetMetrics(m)

	if err := h.Refresh(context.Background()); err != nil {
		t.Fatalf("Refresh: %v", err)
	}

	body := obstest.Scrape(t, reg.Gatherer())
	if !strings.Contains(body, `keeper_rbac_snapshot_roles 2`) {
		t.Errorf("roles gauge mismatch; got=\n%s", body)
	}
	if !strings.Contains(body, `keeper_rbac_snapshot_operators 2`) {
		t.Errorf("operators gauge mismatch; got=\n%s", body)
	}
}

// TestHolder_RefreshLoadErrorKind — a src.Load failure maps to kind=load.
func TestHolder_RefreshLoadErrorKind(t *testing.T) {
	reg := obs.NewRegistry()
	m := RegisterRBACMetrics(reg)

	src := &fakeSource{snap: adminSnapshot("archon-alice")}
	h, err := NewHolder(context.Background(), src, time.Hour, nil)
	if err != nil {
		t.Fatalf("NewHolder: %v", err)
	}
	h.SetMetrics(m)
	src.set(nil, errors.New("db down")) // source fails after the initial load

	if err := h.Refresh(context.Background()); err == nil {
		t.Fatal("expected Refresh error")
	}

	body := obstest.Scrape(t, reg.Gatherer())
	if !strings.Contains(body, `keeper_rbac_snapshot_rebuild_errors_total{kind="load"} 1`) {
		t.Errorf("load error kind mismatch; got=\n%s", body)
	}
}

// TestHolder_CheckRecordsResult — Holder.Check increments checks_total
// based on the actual outcome of enforcer.Check.
func TestHolder_CheckRecordsResult(t *testing.T) {
	reg := obs.NewRegistry()
	m := RegisterRBACMetrics(reg)

	src := &fakeSource{snap: adminSnapshot("archon-alice")}
	h, err := NewHolder(context.Background(), src, time.Hour, nil)
	if err != nil {
		t.Fatalf("NewHolder: %v", err)
	}
	h.SetMetrics(m)

	if err := h.Check("archon-alice", "incarnation", "get", nil); err != nil {
		t.Fatalf("expected allow, got %v", err)
	}
	if err := h.Check("archon-nobody", "incarnation", "get", nil); err == nil {
		t.Fatal("expected deny for unknown AID")
	}

	body := obstest.Scrape(t, reg.Gatherer())
	if !strings.Contains(body, `keeper_rbac_checks_total{result="allow"} 1`) {
		t.Errorf("allow count mismatch; got=\n%s", body)
	}
	if !strings.Contains(body, `keeper_rbac_checks_total{result="deny"} 1`) {
		t.Errorf("deny count mismatch; got=\n%s", body)
	}
}

// TestHolder_SetMetricsRace reproduces the daemon's real init order: the
// background Run goroutine (started in setupRBAC) is already alive when
// SetMetrics gets called later, in setupMetricsRegistry. During that window
// Run concurrently reads the metrics field (via Refresh.ObserveRebuild* and
// Check.ObserveCheck) while SetMetrics writes it. With a plain
// `metrics *RBACMetrics` field this is a data race — go test -race would
// fail (write ↔ read of the same word without synchronization). With
// atomic.Pointer, Store/Load are atomic and the race detector stays clean.
//
// interval is in ms so that Run's ticks actually trigger Refresh →
// m.ObserveRebuild* — a real reader of the metrics field, not just a select
// loop. The concurrent Check is a second reader of the same field; the
// SetMetrics loop is the writer. Reverting to a plain field would fail this
// test under -race again (the regression this test catches).
func TestHolder_SetMetricsRace(t *testing.T) {
	src := &fakeSource{snap: adminSnapshot("archon-alice")}
	h, err := NewHolder(context.Background(), src, time.Millisecond, nil)
	if err != nil {
		t.Fatalf("NewHolder: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Read side: the background Run (as in setupRBAC) re-reads the snapshot
	// and reads the metrics field on every tick/check.
	go h.Run(ctx)
	checkDone := make(chan struct{})
	go func() {
		defer close(checkDone)
		for {
			select {
			case <-ctx.Done():
				return
			default:
				_ = h.Check("archon-alice", "operator", "create", nil)
			}
		}
	}()

	// Write side: SetMetrics concurrently writes the field (as in
	// setupMetricsRegistry, later in time). Each Store uses a fresh
	// descriptor — makes the race more visible.
	reg := obs.NewRegistry()
	m := RegisterRBACMetrics(reg)
	for i := 0; i < 200; i++ {
		h.SetMetrics(m) // repeating the same descriptor is fine (idempotent Store)
	}

	cancel()
	<-checkDone
}
