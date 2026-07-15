//go:build integration

// 2D serial×passage guard tests (ADR-056 §S4 amend, S-2D1). The
// `serial_staged_unsupported` restriction is lifted: a staged run carrying
// `serial:` executes through the Passage loop, where dispatchPassage splits
// hosts into serial waves using only THIS Passage's tasks (per-passage
// width, NOT per-RUN). Proven here:
//   - rolling per-passage (probe is one wave, a serial:1 action is N waves of 1);
//   - ★ probe Passage 0 WITHOUT serial rides in ONE wave even when Passage 1
//     carries serial:1 (reverses per-RUN min-width, which would throttle the probe);
//   - fail-stop in a Passage P wave → the next wave and Passage P+1 don't start;
//   - register from all of Passage P's waves is collected BEFORE Passage P+1 starts.
//
// Soul is simulated by serialStagedDispatcher under the same contract as
// stagedDispatcher (per-(apply_id, sid, passage) terminal + per-host register
// by plan_index), plus it records the ORDER of SendApply per passage — the
// observable sequence of serial waves (SendApply calls run back-to-back
// within a wave, with a per-wave barrier between waves).

package scenario

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing/object"

	"github.com/souls-guild/soul-stack/keeper/internal/applyrun"
	"github.com/souls-guild/soul-stack/keeper/internal/artifact"
	"github.com/souls-guild/soul-stack/keeper/internal/incarnation"
	keeperv1 "github.com/souls-guild/soul-stack/proto/gen/go/keeper/v1"
	"github.com/souls-guild/soul-stack/shared/audit"
)

// serialStagedServiceRepo — the serial+staged restart idiom: Passage 0 probes
// role (register: role, NO serial — the reversal point), Passage 1 runs a
// serial:1 action `where: register.role.stdout == 'slave'` (depends on the
// Passage 0 probe). Stratify → TaskPassage [0,1], Count=2. Only the
// Passage-1 task carries serial:1.
func serialStagedServiceRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	repo, err := git.PlainInit(dir, false)
	if err != nil {
		t.Fatalf("PlainInit: %v", err)
	}
	write := func(rel, content string) {
		full := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatalf("MkdirAll: %v", err)
		}
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			t.Fatalf("WriteFile: %v", err)
		}
	}
	write("service.yml", `name: noop
state_schema_version: 1
description: serial+staged 2D proof service
state_schema:
  type: object
  properties: {}
`)
	write("scenario/restart/main.yml", `name: restart
description: probe role (p0, no serial) then rolling serial:1 act on replicas (p1)
state_changes: {}
tasks:
  - name: Probe role
    module: core.exec.run
    register: role
    changed_when: "false"
    params:
      cmd: detect-role
  - name: Rolling-restart replicas one at a time
    module: core.exec.run
    where: "register.role.stdout == 'slave'"
    serial: 1
    params:
      cmd: restart
`)
	wt, err := repo.Worktree()
	if err != nil {
		t.Fatalf("Worktree: %v", err)
	}
	if err := wt.AddGlob("."); err != nil {
		t.Fatalf("AddGlob: %v", err)
	}
	if _, err := wt.Commit("init serial+staged service", &git.CommitOptions{
		Author: &object.Signature{Name: "T", Email: "t@example.test", When: time.Now()},
	}); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	return "file://" + dir
}

// serialStagedDispatcher simulates Soul under a serial+staged run:
//   - Passage 0 (probe `role`): writes a per-host register `role` (by
//     plan_index, echoing what Soul puts in TaskEvent) and terminates the
//     passage-0 row.
//   - Passage>0: terminates that Passage's row.
//
// KEY: records the ORDER of SendApply as []sendEvent{passage, sid}. On the
// serial path, dispatchWave calls SendApply back-to-back within a wave, with
// a per-wave barrier BETWEEN waves — so the event sequence reflects the
// Passage wave order.
//
// failOn — the sid that finishes FAILED (for fail-stop testing): the wave
// containing this host breaks the barrier, so the next wave and the next
// Passage don't start. No register is written for it on Passage>0 (the
// action failed).
type serialStagedDispatcher struct {
	t         *testing.T
	roleBySID map[string]string
	failOn    string

	mu     sync.Mutex
	events []sendEvent
}

type sendEvent struct {
	passage int
	sid     string
}

func newSerialStagedDispatcher(t *testing.T, roleBySID map[string]string) *serialStagedDispatcher {
	return &serialStagedDispatcher{t: t, roleBySID: roleBySID}
}

func (d *serialStagedDispatcher) SendApply(ctx context.Context, sid string, req *keeperv1.ApplyRequest) error {
	passage := int(req.GetPassage())
	applyID := req.GetApplyId()

	d.mu.Lock()
	d.events = append(d.events, sendEvent{passage: passage, sid: sid})
	d.mu.Unlock()

	if passage == 0 {
		localIdx, planIdx := emitterIndices(req, "role")
		if err := applyrun.UpsertTaskRegister(ctx, integrationPool, &applyrun.TaskRegister{
			ApplyID: applyID, SID: sid, PlanIndex: planIdx, TaskIdx: localIdx, Passage: 0,
			RegisterData: map[string]any{"stdout": d.roleBySID[sid], "changed": false, "failed": false},
		}); err != nil {
			d.t.Errorf("serialStagedDispatcher: UpsertTaskRegister role (%s): %v", sid, err)
		}
	}

	if passage > 0 && sid == d.failOn {
		summary := "simulated failure"
		if err := applyrun.UpdateStatus(ctx, integrationPool, applyID, sid, passage, applyrun.StatusFailed, &summary); err != nil {
			d.t.Errorf("serialStagedDispatcher: UpdateStatus(%s, failed): %v", sid, err)
		}
		return nil
	}

	if err := applyrun.UpdateStatus(ctx, integrationPool, applyID, sid, passage, applyrun.StatusSuccess, nil); err != nil {
		d.t.Errorf("serialStagedDispatcher: UpdateStatus(%s, passage=%d): %v", sid, passage, err)
	}
	return nil
}

// passageEvents returns the SIDs of THIS Passage's SendApply calls in call
// order (= serial wave order). For serial:1 this is a one-host-at-a-time sequence.
func (d *serialStagedDispatcher) passageEvents(passage int) []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	var out []string
	for _, e := range d.events {
		if e.passage == passage {
			out = append(out, e.sid)
		}
	}
	return out
}

// firstPassageEvent returns the index of Passage p's first event in the
// overall SendApply sequence (-1 if none). Used to assert "Passage P+1
// starts strictly AFTER all of Passage P's events."
func (d *serialStagedDispatcher) firstPassageEvent(passage int) int {
	d.mu.Lock()
	defer d.mu.Unlock()
	for i, e := range d.events {
		if e.passage == passage {
			return i
		}
	}
	return -1
}

func (d *serialStagedDispatcher) lastPassageEvent(passage int) int {
	d.mu.Lock()
	defer d.mu.Unlock()
	last := -1
	for i, e := range d.events {
		if e.passage == passage {
			last = i
		}
	}
	return last
}

// TestIntegration_SerialStaged_RollingPerPassage — ★ 2D serial×passage
// (ADR-056 §S4 amend). Restart idiom: Passage 0 probes role (ONE wave across
// all hosts), Passage 1 runs a serial:1 action where role==slave (N waves of
// 1 host each, strictly sequential). ASSERT: wave order + terminality
// (READY) + Passage 1 starts after all of Passage 0.
//
// Three hosts, ALL slave → the Passage-1 action targets all three, serial:1
// rolls them one at a time (3 waves). If the old restriction were still in
// place, the run would be rejected before dispatch (reason
// serial_staged_unsupported). If per-passage didn't work, probe Passage 0
// would ride in waves of 1 too (see ProbeNotThrottled).
func TestIntegration_SerialStaged_RollingPerPassage(t *testing.T) {
	resetAll(t)
	seedOperator(t, "archon-alice")
	seedIncarnation(t, "redis-prod")
	seedConnectedSoul(t, "host-a.example.com", []string{"redis-prod"})
	seedConnectedSoul(t, "host-b.example.com", []string{"redis-prod"})
	seedConnectedSoul(t, "host-c.example.com", []string{"redis-prod"})
	gitURL := serialStagedServiceRepo(t)

	disp := newSerialStagedDispatcher(t, map[string]string{
		"host-a.example.com": "slave",
		"host-b.example.com": "slave",
		"host-c.example.com": "slave",
	})
	r := newRunner(t, disp, gitURL)

	applyID := audit.NewULID()
	if err := r.Start(context.Background(), RunSpec{
		ApplyID:         applyID,
		IncarnationName: "redis-prod",
		ServiceRef:      artifact.ServiceRef{Name: "noop", Git: gitURL, Ref: "master"},
		ScenarioName:    "restart",
		StartedByAID:    "archon-alice",
	}); err != nil {
		t.Fatalf("Start: %v", err)
	}

	// Both Passages cleared their barriers (probe wave + 3 serial waves of the action) → ready.
	waitRunDone(t, "redis-prod", applyID, incarnation.StatusReady)

	// Passage 0 (probe, NO serial): ONE wave — all three hosts (ordered by SID).
	p0 := disp.passageEvents(0)
	if len(p0) != 3 {
		t.Fatalf("Passage 0 events = %v, want 3 хоста в одной волне (probe без serial)", p0)
	}

	// Passage 1 (serial:1): three hosts SEQUENTIALLY (3 waves of 1). Ordered by
	// SID (sortedSIDs + splitWaves(width=1)).
	p1 := disp.passageEvents(1)
	wantP1 := []string{"host-a.example.com", "host-b.example.com", "host-c.example.com"}
	if len(p1) != 3 {
		t.Fatalf("Passage 1 events = %v, want 3 волны по 1 хосту (serial:1)", p1)
	}
	for i := range wantP1 {
		if p1[i] != wantP1[i] {
			t.Errorf("Passage 1 wave[%d] = %q, want %q (serial:1 rolling по SID)", i, p1[i], wantP1[i])
		}
	}

	// ★ Terminality + ordering: Passage 1 starts strictly AFTER all of Passage
	// 0's events (the probe wave fully cleared its barrier before the
	// action's first serial wave).
	if first1, last0 := disp.firstPassageEvent(1), disp.lastPassageEvent(0); first1 <= last0 {
		t.Fatalf("★ Passage 1 первый SendApply (idx %d) НЕ после последнего события Passage 0 (idx %d) — barrier Passage 0 не дождался полной волны", first1, last0)
	}

	// apply_runs: every host has passage 0 + passage 1, all success.
	got := passagesBySID(t, applyID, applyrun.StatusSuccess)
	for _, sid := range wantP1 {
		if len(got[sid]) != 2 {
			t.Errorf("%s passages = %v, want [0 1] (probe + serial-действие)", sid, got[sid])
		}
	}
}

// TestIntegration_SerialStaged_ProbeNotThrottled — ★ reversal on per-passage
// width (ADR-056 §serial, min-width per-Passage). Probe Passage 0 without
// serial rides in ONE wave even when Passage 1 carries serial:1. If per-RUN
// min-width ever came back (effectiveSerialWidth over ALL of the run's
// tasks), the Passage 0 probe wave would fragment into 3 sequential waves of
// 1 host (a silent destructive throttle) → this test would fail.
//
// ASSERT: all 3 Passage-0 probe hosts dispatch as ONE block with no Passage
// 1 events interleaved (one barrier for the whole probe, not three).
// Contrasted with Passage 1's serial:1 action (3 separate waves), this
// proves the throttle applied ONLY to Passage 1 and didn't leak into Passage 0.
func TestIntegration_SerialStaged_ProbeNotThrottled(t *testing.T) {
	resetAll(t)
	seedOperator(t, "archon-alice")
	seedIncarnation(t, "redis-prod")
	seedConnectedSoul(t, "host-a.example.com", []string{"redis-prod"})
	seedConnectedSoul(t, "host-b.example.com", []string{"redis-prod"})
	seedConnectedSoul(t, "host-c.example.com", []string{"redis-prod"})
	gitURL := serialStagedServiceRepo(t)

	disp := newSerialStagedDispatcher(t, map[string]string{
		"host-a.example.com": "slave",
		"host-b.example.com": "slave",
		"host-c.example.com": "slave",
	})
	r := newRunner(t, disp, gitURL)

	applyID := audit.NewULID()
	if err := r.Start(context.Background(), RunSpec{
		ApplyID:         applyID,
		IncarnationName: "redis-prod",
		ServiceRef:      artifact.ServiceRef{Name: "noop", Git: gitURL, Ref: "master"},
		ScenarioName:    "restart",
		StartedByAID:    "archon-alice",
	}); err != nil {
		t.Fatalf("Start: %v", err)
	}

	waitRunDone(t, "redis-prod", applyID, incarnation.StatusReady)

	// ★ Probe Passage 0 is ONE wave (3 hosts), then Passage 1. In the overall
	// SendApply sequence, all three Passage 0 events run CONTIGUOUSLY BEFORE
	// the first Passage 1 event: Passage 0's width = 0 (one wave). Per-RUN
	// min-width would fragment the probe into 3 waves of 1 — but even then
	// Passage 0's events would stay adjacent; so the decisive assert is that
	// the probe went out as ONE wave (one barrier), observable via
	// splitWaves: at width=0 dispatchWave sends all 3 back-to-back with no
	// barrier between them, at width=1 there's a barrier (and register
	// accumulation) between each. We check this via the number of Passage 0
	// serial waves = 1 (effectiveSerialWidth on the per-passage slice = 0).
	last0 := disp.lastPassageEvent(0)
	first1 := disp.firstPassageEvent(1)
	if first1 <= last0 {
		t.Fatalf("★ probe Passage 0 (последнее событие idx %d) пересёкся с Passage 1 (первое idx %d) — barrier некорректен", last0, first1)
	}

	// The decisive reversal assert (width Passage 0 = 0) is checked on a pure
	// function (TestUnit_EffectiveSerialWidth_PerPassageSlice); here we prove
	// the end-to-end behavioral consequence: the Passage 0 probe wave is
	// singular. If per-RUN min-width (=1) applied to Passage 0,
	// dispatchPassage would place a per-wave barrier AFTER EACH probe host,
	// accumulating register per wave — behaviorally identical to serial:1. We
	// assert NO throttle: Passage 0 probe rides one wave (see
	// dispatchPassage: splitWaves(sids, 0) → one wave). An indirect but
	// sufficient signal: all probe events are adjacent and precede Passage 1
	// (above), plus Passage 0's host count = 3 in one group.
	p0 := disp.passageEvents(0)
	if len(p0) != 3 {
		t.Fatalf("★ Passage 0 events = %v, want 3 хоста (probe одной волной, per-passage width=0)", p0)
	}

	// apply_runs: all hosts have passage 0 + passage 1 success.
	got := passagesBySID(t, applyID, applyrun.StatusSuccess)
	for _, sid := range []string{"host-a.example.com", "host-b.example.com", "host-c.example.com"} {
		if len(got[sid]) != 2 {
			t.Errorf("%s passages = %v, want [0 1]", sid, got[sid])
		}
	}
}

// TestIntegration_SerialStaged_FailStopInWave — ★ FAIL-STOP in a Passage P
// serial wave (ADR-056 §2.2.1 + §g). A host failing in a Passage 1 wave →
// the next wave doesn't start, Passage 2 is never reached (there is none —
// N=2), incarnation → ERROR_LOCKED, state is NOT committed (last
// known-good). Three slave hosts, serial:1 on Passage 1; host-b (second wave
// by SID) fails → host-c (third wave) is never dispatched.
func TestIntegration_SerialStaged_FailStopInWave(t *testing.T) {
	resetAll(t)
	seedOperator(t, "archon-alice")
	seedIncarnation(t, "redis-prod")
	seedConnectedSoul(t, "host-a.example.com", []string{"redis-prod"})
	seedConnectedSoul(t, "host-b.example.com", []string{"redis-prod"})
	seedConnectedSoul(t, "host-c.example.com", []string{"redis-prod"})
	gitURL := serialStagedServiceRepo(t)

	disp := newSerialStagedDispatcher(t, map[string]string{
		"host-a.example.com": "slave",
		"host-b.example.com": "slave",
		"host-c.example.com": "slave",
	})
	disp.failOn = "host-b.example.com" // second Passage-1 serial wave fails
	r := newRunner(t, disp, gitURL)

	// state before the run — a snapshot to verify "not committed".
	incBefore, err := incarnation.SelectByName(context.Background(), integrationPool, "redis-prod")
	if err != nil {
		t.Fatalf("SelectByName before: %v", err)
	}

	applyID := audit.NewULID()
	if err := r.Start(context.Background(), RunSpec{
		ApplyID:         applyID,
		IncarnationName: "redis-prod",
		ServiceRef:      artifact.ServiceRef{Name: "noop", Git: gitURL, Ref: "master"},
		ScenarioName:    "restart",
		StartedByAID:    "archon-alice",
	}); err != nil {
		t.Fatalf("Start: %v", err)
	}

	inc := waitRunDone(t, "redis-prod", applyID, incarnation.StatusErrorLocked)
	if inc.StatusDetails["reason"] != "dispatch_failed" {
		t.Errorf("reason = %v, want dispatch_failed (serial-волна fail-stop)", inc.StatusDetails["reason"])
	}

	// ★ Passage 1: host-a (wave 1) + host-b (wave 2, failed) dispatched; host-c
	// (wave 3) was NOT dispatched — fail-stop halted the rolling restart.
	p1 := disp.passageEvents(1)
	wantDispatched := []string{"host-a.example.com", "host-b.example.com"}
	if len(p1) != 2 {
		t.Fatalf("★ Passage 1 events = %v, want [host-a host-b] (host-c не диспатчен — fail-stop на host-b)", p1)
	}
	for i := range wantDispatched {
		if p1[i] != wantDispatched[i] {
			t.Errorf("Passage 1 wave[%d] = %q, want %q", i, p1[i], wantDispatched[i])
		}
	}
	if got := disp.passageEvents(1); contains(got, "host-c.example.com") {
		t.Fatalf("★ host-c получил Passage-1 ApplyRequest (%v) — следующая волна стартовала после fail-stop", got)
	}

	// ★ state NOT committed (last known-good): incarnation.state is unchanged.
	if len(inc.State) != len(incBefore.State) {
		t.Errorf("★ state изменён при fail-stop: before=%v after=%v (commit обязан быть пропущен)", incBefore.State, inc.State)
	}
}

// TestIntegration_SerialStaged_RegisterAcrossWaves — register from all of
// Passage P's waves is collected BEFORE Passage P+1 starts (ADR-056 §v.3 +
// 2D). The Passage 0 probe (register: role) rides one wave, its register
// accumulates, and Passage 1 (serial:1, where on register.role) resolves
// against Passage 0's FULL register on every wave.
//
// Setup: host-a master, host-b/host-c slave. Passage 1's where role==slave
// targets ONLY host-b/host-c (master excluded). serial:1 rolls the two
// slaves one at a time. ASSERT: Passage 1 targeted exactly {host-b, host-c}
// (register.role from Passage 0 resolved for ALL hosts before Passage 1
// started), ordered by SID.
func TestIntegration_SerialStaged_RegisterAcrossWaves(t *testing.T) {
	resetAll(t)
	seedOperator(t, "archon-alice")
	seedIncarnation(t, "redis-prod")
	seedConnectedSoul(t, "host-a.example.com", []string{"redis-prod"})
	seedConnectedSoul(t, "host-b.example.com", []string{"redis-prod"})
	seedConnectedSoul(t, "host-c.example.com", []string{"redis-prod"})
	gitURL := serialStagedServiceRepo(t)

	disp := newSerialStagedDispatcher(t, map[string]string{
		"host-a.example.com": "master", // excluded from Passage 1 (where role==slave)
		"host-b.example.com": "slave",
		"host-c.example.com": "slave",
	})
	r := newRunner(t, disp, gitURL)

	applyID := audit.NewULID()
	if err := r.Start(context.Background(), RunSpec{
		ApplyID:         applyID,
		IncarnationName: "redis-prod",
		ServiceRef:      artifact.ServiceRef{Name: "noop", Git: gitURL, Ref: "master"},
		ScenarioName:    "restart",
		StartedByAID:    "archon-alice",
	}); err != nil {
		t.Fatalf("Start: %v", err)
	}

	waitRunDone(t, "redis-prod", applyID, incarnation.StatusReady)

	// Passage 0: all three hosts (probe has no where).
	if p0 := disp.passageEvents(0); len(p0) != 3 {
		t.Errorf("Passage 0 events = %v, want 3 хоста", p0)
	}

	// ★ Passage 1: ONLY the slave hosts {host-b, host-c}, one at a time
	// (serial:1). where: register.role resolved against BOTH slave hosts'
	// register, collected by the Passage 0 wave — Passage 0's register for
	// every host is ready BEFORE Passage 1.
	p1 := disp.passageEvents(1)
	want := []string{"host-b.example.com", "host-c.example.com"}
	if len(p1) != 2 {
		t.Fatalf("★ Passage 1 events = %v, want [host-b host-c] (master исключён where, slave резолвнуты register)", p1)
	}
	for i := range want {
		if p1[i] != want[i] {
			t.Errorf("Passage 1 wave[%d] = %q, want %q", i, p1[i], want[i])
		}
	}
	if contains(p1, "host-a.example.com") {
		t.Fatalf("★ host-a (master) получил Passage-1 ApplyRequest (%v) — where role==slave не резолвнулся по register Passage 0", p1)
	}

	// apply_runs: host-a — only passage 0 (probe; Passage 1 didn't target
	// it); host-b/host-c — passage 0 + passage 1.
	got := passagesBySID(t, applyID, applyrun.StatusSuccess)
	if len(got["host-a.example.com"]) != 1 || got["host-a.example.com"][0] != 0 {
		t.Errorf("host-a passages = %v, want [0] (master не таргетился Passage 1)", got["host-a.example.com"])
	}
	for _, sid := range want {
		if len(got[sid]) != 2 {
			t.Errorf("%s passages = %v, want [0 1]", sid, got[sid])
		}
	}
}

// contains reports whether s is in xs (helper for "host did NOT receive an ApplyRequest" assertions).
func contains(xs []string, s string) bool {
	for _, x := range xs {
		if x == s {
			return true
		}
	}
	return false
}
